package check

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// runner is a Runner with nothing gated: no Looker, no privilege, one at a time.
// A test that cares about a gate turns that one on and says so.
func runner() *Runner {
	return &Runner{Concurrency: 1, Thresholds: threshold.Defaults()}
}

func byID(results []Result) map[ID]Result {
	out := make(map[ID]Result, len(results))
	for _, r := range results {
		out[r.ID] = r
	}
	return out
}

func TestRunReturnsOneResultPerCheckOrderedByID(t *testing.T) {
	checks := []Check{stub("net.ladder"), stub("disk.usage"), stub("disk.inodes")}
	got := runner().Run(context.Background(), checks, nil)

	want := []ID{"disk.inodes", "disk.usage", "net.ladder"}
	if len(got) != len(want) {
		t.Fatalf("got %d results for %d checks", len(got), len(checks))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("results[%d] = %s, want %s", i, got[i].ID, id)
		}
		if got[i].Status != StatusOK {
			t.Errorf("%s = %s, want ok", id, got[i].Status)
		}
	}
}

// The id comes from the spec, not from whatever the check filled in. A check that
// returns someone else's id would put a result under a name an operator's alert
// already watches.
func TestTheIDComesFromTheSpec(t *testing.T) {
	c := stub("disk.usage")
	c.run = func(context.Context, Input) Result {
		return Result{ID: "something.else", Status: StatusOK}
	}
	got := runner().Run(context.Background(), []Check{c}, nil)
	if got[0].ID != "disk.usage" {
		t.Errorf("id = %s, want the spec's disk.usage", got[0].ID)
	}
}

// Every result leaving the runner satisfies the two wire rules, whatever the check
// returned -- a check author cannot be expected to remember them.
func TestEveryResultIsNormalisedOnTheWayOut(t *testing.T) {
	c := stub("disk.usage")
	c.run = func(context.Context, Input) Result {
		return Result{Metrics: []Metric{{Name: "disk.used_pct"}}} // nil trips, nil steps, nil labels, no status
	}
	got := runner().Run(context.Background(), []Check{c}, nil)[0]
	if got.Trips == nil || got.Steps == nil || got.Metrics[0].Labels == nil {
		t.Errorf("a nil slice or map survived: %+v", got)
	}
	if got.Status != StatusOK {
		t.Errorf("status = %q, want ok derived from no trips", got.Status)
	}
}

// A missing program is an absence, not a fault: a host with no smartctl is a
// supported host. Reporting it once, by name, before Run is what stops every such
// check writing the same ErrNotFound branch.
func TestAMissingProgramIsUnavailableAndNamed(t *testing.T) {
	ran := false
	c := stub("disk.smart")
	c.spec.NeedsBins = []string{"smartctl"}
	c.run = func(context.Context, Input) Result {
		ran = true
		return Result{Status: StatusOK}
	}

	r := runner()
	r.Look = cmdrun.NewFakeLooker("df")
	got := r.Run(context.Background(), []Check{c}, nil)[0]

	if got.Status != StatusUnavailable {
		t.Errorf("status = %s, want unavailable", got.Status)
	}
	if !strings.Contains(got.Summary, "smartctl") {
		t.Errorf("summary does not name the missing program: %q", got.Summary)
	}
	if ran {
		t.Error("the check ran without the program it declared it needs")
	}
}

// A list of four missing tools reads as a broken host when it is one absent
// package, so only the first is named.
func TestOnlyTheFirstMissingProgramIsNamed(t *testing.T) {
	c := stub("docker.ps")
	c.spec.NeedsBins = []string{"docker", "docker-compose"}
	r := runner()
	r.Look = cmdrun.NewFakeLooker()
	got := r.Run(context.Background(), []Check{c}, nil)[0]
	if strings.Contains(got.Summary, "docker-compose") {
		t.Errorf("every missing tool was listed: %q", got.Summary)
	}
	if !strings.Contains(got.Summary, "docker") {
		t.Errorf("no missing tool was named: %q", got.Summary)
	}
}

func TestAProgramThatIsInstalledDoesNotGate(t *testing.T) {
	c := stub("disk.smart")
	c.spec.NeedsBins = []string{"smartctl"}
	r := runner()
	r.Look = cmdrun.NewFakeLooker("smartctl")
	if got := r.Run(context.Background(), []Check{c}, nil)[0]; got.Status != StatusOK {
		t.Errorf("status = %s (%s), want ok", got.Status, got.Summary)
	}
}

// v1 evaluated Tier 2 once at compose time and never re-checked, so a credential
// granted after startup never took effect. The probe runs per Run.
func TestTier2IsAskedOncePerRunAndNotPerCheck(t *testing.T) {
	var asked atomic.Int32
	r := runner()
	r.CanTier2 = func(context.Context) bool {
		asked.Add(1)
		return false
	}

	tier2 := func(id ID) fake {
		c := stub(id)
		c.spec.Tier2 = true
		return c
	}
	got := byID(r.Run(context.Background(), []Check{
		tier2("procs.kill_preview"), tier2("services.restart_preview"), stub("disk.usage"),
	}, nil))

	// One `sudo -n true` per Tier 2 check would be a burst of auth.log lines for a
	// single run.
	if n := asked.Load(); n != 1 {
		t.Errorf("the privilege probe ran %d times, want once", n)
	}
	for _, id := range []ID{"procs.kill_preview", "services.restart_preview"} {
		if got[id].Status != StatusSkipped {
			t.Errorf("%s = %s, want skipped", id, got[id].Status)
		}
		if !strings.Contains(got[id].Summary, "sudo") {
			t.Errorf("%s does not say what would fix it: %q", id, got[id].Summary)
		}
	}
	if got["disk.usage"].Status != StatusOK {
		t.Error("a Tier 1 check was gated by the privilege probe")
	}

	// And with the credential, it runs.
	r.CanTier2 = func(context.Context) bool { return true }
	if s := r.Run(context.Background(), []Check{tier2("procs.kill_preview")}, nil)[0].Status; s != StatusOK {
		t.Errorf("with sudo, status = %s, want ok", s)
	}
}

// A nil probe means no Tier 2 rather than a permissive default: fail-closed is the
// only safe reading of "nobody wired the privilege check up".
func TestANilPrivilegeProbeMeansNoTier2(t *testing.T) {
	c := stub("procs.kill_preview")
	c.spec.Tier2 = true
	if got := runner().Run(context.Background(), []Check{c}, nil)[0]; got.Status != StatusSkipped {
		t.Errorf("status = %s, want skipped", got.Status)
	}
}

// Not asking for something is not a failure. A default run full of errors for
// checks nobody invoked would bury the ones that matter.
func TestAMissingRequiredParameterSkipsRatherThanErrors(t *testing.T) {
	c := stub("storage.inspect")
	c.spec.Params = []ParamSpec{
		{Name: "path", Required: true, Description: "the directory to inspect"},
		{Name: "min_bytes", Default: "10485760"},
	}
	got := runner().Run(context.Background(), []Check{c}, nil)[0]
	if got.Status != StatusSkipped {
		t.Fatalf("status = %s, want skipped", got.Status)
	}
	if !strings.Contains(got.Summary, "path") {
		t.Errorf("summary does not name the parameter: %q", got.Summary)
	}
	// The names go in data as well, so a TUI can prompt for exactly them rather
	// than parsing the sentence.
	names, ok := got.Data.([]string)
	if !ok || len(names) != 1 || names[0] != "path" {
		t.Errorf("data = %#v, want the missing names", got.Data)
	}
}

func TestParametersAreDefaultedBeforeTheCheckSeesThem(t *testing.T) {
	var seen map[string]string
	c := stub("storage.inspect")
	c.spec.Params = []ParamSpec{
		{Name: "path", Required: true, Default: "/var/log"},
		{Name: "min_bytes", Default: "10485760"},
		{Name: "pattern"},
	}
	c.run = func(_ context.Context, in Input) Result {
		seen = in.Params
		return Result{Status: StatusOK}
	}

	runner().Run(context.Background(), []Check{c}, map[string]string{"min_bytes": "42"})

	for name, want := range map[string]string{
		"path":      "/var/log", // the spec's default
		"min_bytes": "42",       // the operator's value
		"pattern":   "",         // declared, no default, not supplied
	} {
		if seen[name] != want {
			t.Errorf("Param(%s) = %q, want %q", name, seen[name], want)
		}
	}
	// A required parameter with a default is not missing, so the check ran at all.
	if seen == nil {
		t.Fatal("the check did not run")
	}
}

// `check --json` runs unattended on a fleet. One collector tripping over an
// unexpected /proc layout should cost that check, not the report.
func TestAPanickingCheckCostsOnlyItself(t *testing.T) {
	boom := stub("procs.top")
	boom.run = func(context.Context, Input) Result { panic("unexpected /proc layout") }

	got := byID(runner().Run(context.Background(), []Check{boom, stub("disk.usage")}, nil))

	if got["procs.top"].Status != StatusError {
		t.Errorf("the panicking check = %s, want error", got["procs.top"].Status)
	}
	if !strings.Contains(got["procs.top"].Error, "unexpected /proc layout") {
		t.Errorf("the panic value is lost: %q", got["procs.top"].Error)
	}
	// The stack goes in the field rather than on stderr, because a cron job keeps
	// stdout and discards the rest.
	if !strings.Contains(got["procs.top"].Error, "runtime/debug") &&
		!strings.Contains(got["procs.top"].Error, "goroutine") {
		t.Errorf("no stack was captured: %q", got["procs.top"].Error)
	}
	if got["disk.usage"].Status != StatusOK {
		t.Error("a panic in one check took another down with it")
	}
}

// Saying ok after the budget expired is how a timeout becomes a green panel.
func TestACheckThatOutlivesItsBudgetCannotReportOK(t *testing.T) {
	slow := stub("net.traceroute")
	slow.spec.Budget = 10 * time.Millisecond
	slow.run = func(ctx context.Context, _ Input) Result {
		<-ctx.Done() // event-driven, not a poll
		return Result{Status: StatusOK, Summary: "all hops responded"}
	}

	got := runner().Run(context.Background(), []Check{slow}, nil)[0]
	if got.Status != StatusError {
		t.Fatalf("status = %s, want error", got.Status)
	}
	if !strings.Contains(got.Error, "budget") {
		t.Errorf("the error does not say what happened: %q", got.Error)
	}
	if got.Duration <= 0 {
		t.Error("no duration was recorded")
	}
}

// A check that noticed its own deadline and said so keeps its own words: the
// runner is a backstop, not an overwrite.
func TestACheckThatReportsItsOwnTimeoutKeepsItsMessage(t *testing.T) {
	slow := stub("net.traceroute")
	slow.spec.Budget = 10 * time.Millisecond
	slow.run = func(ctx context.Context, _ Input) Result {
		<-ctx.Done()
		return Result{Status: StatusError, Summary: "traceroute did not finish", Error: "deadline exceeded at hop 12"}
	}
	got := runner().Run(context.Background(), []Check{slow}, nil)[0]
	if got.Error != "deadline exceeded at hop 12" || got.Summary != "traceroute did not finish" {
		t.Errorf("the runner overwrote the check's own account: %+v", got)
	}
}

// A cancelled parent stops the run. This is what the TUI's worker groups do when
// an operator switches away from a pane mid-scan.
func TestACancelledParentEndsEveryCheck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := stub("net.traceroute")
	c.run = func(ctx context.Context, _ Input) Result {
		<-ctx.Done()
		return Result{Status: StatusOK}
	}
	got := runner().Run(ctx, []Check{c}, nil)[0]
	if got.Status != StatusError {
		t.Errorf("status = %s, want error", got.Status)
	}
}

// The TUI needs the event now to paint a row and the report needs the same event
// later in steps[]. Two code paths for that is how a rung shows up on screen and
// not in the JSON.
func TestProgressIsBothStreamedAndCollected(t *testing.T) {
	var mu sync.Mutex
	var live []Event

	c := stub("net.ladder")
	c.run = func(_ context.Context, in Input) Result {
		in.Progress.Emit(Event{Text: "gateway 1.2 ms", Status: StatusOK})
		in.Progress.Emit(Event{Text: "dns 41 ms", Status: StatusWarn})
		return Result{Status: StatusWarn}
	}

	r := runner()
	r.Progress = EmitterFunc(func(e Event) {
		mu.Lock()
		live = append(live, e)
		mu.Unlock()
	})
	got := r.Run(context.Background(), []Check{c}, nil)[0]

	if len(got.Steps) != 2 || got.Steps[0].Text != "gateway 1.2 ms" || got.Steps[1].Status != StatusWarn {
		t.Errorf("steps = %+v, want both events in order", got.Steps)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(live) != 2 || live[0].Text != "gateway 1.2 ms" {
		t.Errorf("the live stream got %+v, want both events in order", live)
	}
}

// Checks are promised a non-nil Progress, so a check emitting into a run nobody is
// watching must not panic.
func TestEmittingWithNoListenerIsSafe(t *testing.T) {
	c := stub("net.ladder")
	c.run = func(_ context.Context, in Input) Result {
		in.Progress.Emit(Event{Text: "gateway", Status: StatusOK})
		return Result{Status: StatusOK}
	}
	got := runner().Run(context.Background(), []Check{c}, nil)[0]
	if len(got.Steps) != 1 {
		t.Errorf("steps = %+v, want the one event", got.Steps)
	}
}

// A check that assembled its own steps -- replaying a cached scan, say -- keeps
// them rather than having them silently replaced by an empty collector.
func TestACheckMayBuildItsOwnSteps(t *testing.T) {
	c := stub("net.ladder")
	c.run = func(context.Context, Input) Result {
		return Result{Status: StatusOK, Steps: []Event{{Text: "from cache", Status: StatusOK}}}
	}
	got := runner().Run(context.Background(), []Check{c}, nil)[0]
	if len(got.Steps) != 1 || got.Steps[0].Text != "from cache" {
		t.Errorf("steps = %+v", got.Steps)
	}
}

// The policy is handed to every check, so none of them can grade by a policy the
// report does not describe.
func TestEveryCheckIsHandedTheSamePolicy(t *testing.T) {
	r := runner()
	r.Thresholds = tightenedPolicy(t)

	var seen threshold.Set
	c := stub("disk.usage")
	c.run = func(_ context.Context, in Input) Result {
		seen = in.Thresholds
		return Result{Status: StatusOK}
	}
	r.Run(context.Background(), []Check{c}, nil)

	rule, ok := seen.Get(threshold.DiskUsedPct)
	if !ok || rule.Warn != 70 {
		t.Errorf("the check was handed %+v, want the runner's policy", rule)
	}
}

// tightenedPolicy is a policy that is visibly not the default, built here rather
// than loaded through internal/config, which imports this package.
func tightenedPolicy(t *testing.T) threshold.Set {
	t.Helper()
	warn := 70.0
	set, warnings := threshold.Merge(map[string]threshold.Override{
		threshold.DiskUsedPct: {Warn: &warn},
	})
	if len(warnings) != 0 {
		t.Fatalf("the test's own policy is invalid: %v", warnings)
	}
	return set
}

// Concurrency is bounded so a fleet-wide run does not put a single-core host on
// its knees -- the box v1 spent a whole release making the TUI usable on.
func TestConcurrencyIsBounded(t *testing.T) {
	const width = 2
	ids := []ID{"a.one", "b.two", "c.three", "d.four", "e.five", "f.six"}

	var live, peak atomic.Int32
	entered := make(chan struct{}, len(ids))
	release := make(chan struct{})

	var checks []Check
	for _, id := range ids {
		c := stub(id)
		c.run = func(context.Context, Input) Result {
			n := live.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			entered <- struct{}{}
			<-release
			live.Add(-1)
			return Result{Status: StatusOK}
		}
		checks = append(checks, c)
	}

	r := runner()
	r.Concurrency = width
	done := make(chan []Result, 1)
	go func() { done <- r.Run(context.Background(), checks, nil) }()

	// Wait for the first wave to be inside the checks rather than sleeping for it:
	// a timer would make the lower-bound assertion a race against the scheduler,
	// and a bound that is never reached proves nothing about the bound.
	for range width {
		<-entered
	}
	close(release)
	got := <-done

	if len(got) != len(checks) {
		t.Fatalf("got %d results for %d checks", len(got), len(checks))
	}
	if p := peak.Load(); p > width {
		t.Errorf("%d checks ran at once, want at most %d", p, width)
	}
	if p := peak.Load(); p < width {
		t.Errorf("peak concurrency was %d, so the bound was never actually exercised", p)
	}
}

// The default is used when the caller says nothing, and a run wider than the work
// must not deadlock on an oversized bound.
func TestConcurrencyDefaultsAndHandlesSmallRuns(t *testing.T) {
	r := &Runner{} // no concurrency, no policy, no gates
	if got := r.Run(context.Background(), []Check{stub("disk.usage")}, nil); len(got) != 1 {
		t.Errorf("a one-check run returned %d results", len(got))
	}
	if got := r.Run(context.Background(), nil, nil); len(got) != 0 {
		t.Errorf("an empty run returned %d results", len(got))
	}
}

func TestTheDefaultBudgetAppliesWhenASpecNamesNone(t *testing.T) {
	var deadline time.Time
	c := stub("disk.usage")
	c.run = func(ctx context.Context, _ Input) Result {
		deadline, _ = ctx.Deadline()
		return Result{Status: StatusOK}
	}

	r := runner()
	r.Budget = 50 * time.Millisecond
	r.Run(context.Background(), []Check{c}, nil)
	if until := time.Until(deadline); until <= 0 || until > 50*time.Millisecond {
		t.Errorf("deadline is %s away, want the runner's 50ms budget", until)
	}

	r.Budget = 0
	r.Run(context.Background(), []Check{c}, nil)
	if until := time.Until(deadline); until <= DefaultBudget/2 || until > DefaultBudget {
		t.Errorf("deadline is %s away, want the package default of %s", until, DefaultBudget)
	}
}

// New wires the real host, and the one thing worth asserting about it is that it
// gates on PATH rather than on nothing -- a nil Looker would silently run every
// check on a host missing every tool.
func TestNewGatesOnTheRealPath(t *testing.T) {
	r := New(threshold.Defaults(), nil)
	if r.Look == nil {
		t.Fatal("New left the binary probe nil, so nothing would be gated")
	}
	c := stub("disk.smart")
	c.spec.NeedsBins = []string{"fleetfix-no-such-program-exists"}
	if got := r.Run(context.Background(), []Check{c}, nil)[0]; got.Status != StatusUnavailable {
		t.Errorf("status = %s, want unavailable", got.Status)
	}
}
