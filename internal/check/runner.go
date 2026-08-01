package check

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// DefaultBudget caps a check that did not name its own.
//
// Ten seconds is above every collector's realistic worst case and well below the
// minute a cron job gets, which is the pair of numbers that matters: a check has
// to be able to finish on a loaded host, and the whole run has to end before the
// next one starts.
const DefaultBudget = 10 * time.Second

// DefaultConcurrency is how many checks run at once when the caller says nothing.
//
// Fixed rather than scaled off GOMAXPROCS. These checks are almost entirely
// waiting -- on a subprocess, on /proc, on a socket -- so the useful width is set
// by how much load the host should feel, not by how many cores it has. Eight keeps
// a single-core box responsive, which is the box v1 spent a whole release making
// the TUI usable on.
const DefaultConcurrency = 8

// A Runner runs checks and turns whatever happens into Results.
//
// Nothing it does can fail the run. A check that panics, blows its budget, needs a
// program that is not installed or needs a privilege this process lacks all become
// a Result saying so, because a report that omitted the checks that went wrong
// would be the most misleading document the tool could produce.
type Runner struct {
	// Look decides whether a Spec's NeedsBins are installed. Nil means nothing is
	// gated, which is right for a test that stages its own answers and wrong for
	// production -- New wires the real one.
	Look cmdrun.Looker

	// CanTier2 reports whether this process can act as root right now. Called per
	// run rather than read from a snapshot, because a sudo credential can be
	// granted or expire between startup and a check; v1 evaluated it once at
	// compose time and never re-checked. Nil means no Tier 2.
	CanTier2 func(context.Context) bool

	// Thresholds is the host's grading policy, handed to every check so none of
	// them can grade by a policy the report does not describe.
	Thresholds threshold.Set

	// Budget is the per-check cap for a Spec that names none.
	Budget time.Duration

	// Concurrency is how many checks run at once. Zero means DefaultConcurrency;
	// one runs them in order, which is what `doctor` and a deterministic test want.
	Concurrency int

	// Progress, when set, receives every event from every check as it happens.
	// The TUI sets this; `check --json` leaves it nil and reads steps[] afterwards.
	Progress Emitter
}

// New returns a Runner wired to the real host: PATH decides what is installed, and
// the caller supplies the privilege probe and the policy.
func New(thresholds threshold.Set, canTier2 func(context.Context) bool) *Runner {
	return &Runner{
		Look:       cmdrun.NewPATH(),
		CanTier2:   canTier2,
		Thresholds: thresholds,
	}
}

// Run runs every check and returns one Result each, ordered by id.
//
// Ordered by id, not by completion: checks finish in whatever order they finish
// in, and the report is asserted byte-stable across consecutive runs on an
// unchanged host.
func (r *Runner) Run(ctx context.Context, checks []Check, params map[string]string) []Result {
	width := r.Concurrency
	if width <= 0 {
		width = DefaultConcurrency
	}
	if width > len(checks) {
		width = len(checks)
	}

	results := make([]Result, len(checks))
	// Tier 2 is one question about the process, not one per check, and asking it
	// per check would mean a `sudo -n true` per Tier 2 check -- a burst of auth.log
	// lines for a single run.
	tier2 := r.tier2(ctx)

	var wg sync.WaitGroup
	slots := make(chan struct{}, max(width, 1))
	for i, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[i] = r.runOne(ctx, c, params, tier2)
		}()
	}
	wg.Wait()

	SortResults(results)
	return results
}

func (r *Runner) tier2(ctx context.Context) bool {
	if r.CanTier2 == nil {
		return false
	}
	return r.CanTier2(ctx)
}

// runOne gates one check and, if it survives the gates, runs it.
//
// Gate order is deliberate: skipped before unavailable. A Tier 2 check on a host
// without the tool it needs is more usefully reported as "we did not try" than as
// "the tool is missing", because the operator's next move is to grant the
// privilege, not to install anything.
func (r *Runner) runOne(ctx context.Context, c Check, params map[string]string, tier2 bool) Result {
	spec := c.Spec()

	if spec.Tier2 && !tier2 {
		return skipped(spec.ID, "needs root or a working sudo -n; run as root or refresh the credential")
	}
	if missing, why := r.missingParams(spec, params); why != "" {
		return skipped(spec.ID, why).withData(missing)
	}
	if name, ok := r.missingBin(spec); !ok {
		return Result{
			ID:      spec.ID,
			Status:  StatusUnavailable,
			Summary: fmt.Sprintf("%s is not installed on this host", name),
		}.Normalize()
	}

	budget := spec.Budget
	if budget <= 0 {
		budget = r.budget()
	}
	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	steps := &collector{fanout: r.Progress}
	in := Input{
		Params:     r.resolveParams(spec, params),
		Progress:   steps,
		Thresholds: r.Thresholds,
	}

	started := time.Now()
	res := r.call(runCtx, c, in)
	res.ID = spec.ID
	res.Duration = time.Since(started)

	// The check's own steps win if it built any; otherwise it gets what it emitted.
	if len(res.Steps) == 0 {
		res.Steps = steps.taken()
	}

	// A check that returned an ordinary-looking result after its budget expired
	// reported on a host it did not finish looking at. Saying ok there is how a
	// timeout becomes a green panel.
	if runCtx.Err() != nil && res.Status != StatusError {
		res.Status = StatusError
		if res.Error == "" {
			res.Error = fmt.Sprintf("exceeded its %s budget", budget)
		}
		if res.Summary == "" {
			res.Summary = "timed out"
		}
	}
	return res.Normalize()
}

// call runs the check and converts a panic into a result.
//
// A panicking check must not take the process down: `check --json` runs unattended
// on a fleet, and one collector tripping over an unexpected /proc layout should
// cost that check, not the report. The stack goes to the Error field rather than
// stderr because a cron job keeps stdout and discards the rest.
func (r *Runner) call(ctx context.Context, c Check, in Input) (res Result) {
	defer func() {
		if p := recover(); p != nil {
			res = Result{
				Status:  StatusError,
				Summary: "the check panicked",
				Error:   fmt.Sprintf("panic: %v\n%s", p, debug.Stack()),
			}
		}
	}()
	return c.Run(ctx, in)
}

func (r *Runner) budget() time.Duration {
	if r.Budget > 0 {
		return r.Budget
	}
	return DefaultBudget
}

// missingBin reports the first NeedsBins entry that is not installed.
//
// First rather than all: the operator's next action is to install something, and a
// list of four missing tools reads as a broken host when it is one absent package.
// A nil Looker gates nothing, which is what a test staging its own answers wants.
func (r *Runner) missingBin(spec Spec) (string, bool) {
	if r.Look == nil {
		return "", true
	}
	for _, name := range spec.NeedsBins {
		if _, err := r.Look.Look(name); err != nil {
			return name, false
		}
	}
	return "", true
}

// missingParams reports the required parameters the operator did not supply.
//
// Skipped, not errored: not asking for something is not a failure. An inspect
// check with no path is a check the operator did not request, and a default run
// full of errors for checks nobody invoked would bury the ones that matter.
func (r *Runner) missingParams(spec Spec, params map[string]string) ([]string, string) {
	var missing []string
	for _, p := range spec.Params {
		if !p.Required || p.Default != "" {
			continue
		}
		if strings.TrimSpace(params[p.Name]) == "" {
			missing = append(missing, p.Name)
		}
	}
	if len(missing) == 0 {
		return nil, ""
	}
	sort.Strings(missing)
	return missing, "needs " + strings.Join(missing, ", ") + ", which has no default"
}

// resolveParams applies the spec's defaults so a check reading a parameter it
// declared always gets a value.
func (r *Runner) resolveParams(spec Spec, params map[string]string) map[string]string {
	out := make(map[string]string, len(spec.Params))
	for _, p := range spec.Params {
		if v, ok := params[p.Name]; ok && v != "" {
			out[p.Name] = v
			continue
		}
		out[p.Name] = p.Default
	}
	return out
}

func skipped(id ID, why string) Result {
	return Result{ID: id, Status: StatusSkipped, Summary: why}
}

func (r Result) withData(data any) Result {
	r.Data = data
	return r
}

// collector records a check's events and, when the caller wants live progress,
// passes them on as they happen.
//
// Both at once rather than either/or: the TUI needs the event now to paint a row,
// and the report needs the same event later in steps[]. Two code paths for that is
// how a rung shows up on screen and not in the JSON.
type collector struct {
	mu     sync.Mutex
	events []Event
	fanout Emitter
}

func (c *collector) Emit(e Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
	// Outside the lock: a slow consumer -- a TUI queue, a terminal -- must not
	// serialise the checks behind it. Same reasoning as the audit log's OTEL sink.
	if c.fanout != nil {
		c.fanout.Emit(e)
	}
}

func (c *collector) taken() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event{}, c.events...)
}
