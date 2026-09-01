// Package check is the contract all three front doors share.
//
// A Check is one thing FleetFix can find out about a host. The TUI runs them to
// paint a pane, `check --json` runs them to fill checks[], and the agent runs them
// on a schedule -- one implementation each, not three. v1 had no such contract:
// the collectors were pure, but everything that decided what to run, what it meant
// and how to say so lived in screens/, which is why nothing headless could reach
// it.
//
// Two rules hold the wire format still, and both are easy to break by accident:
//
//   - No omitempty, anywhere. An absent key and a null key are different documents
//     to a consumer, and the difference is invisible in Go until a parser breaks.
//   - No nil slices. nil marshals to null; an empty slice marshals to []. Runner
//     normalises every Result on the way out so a check that returns nothing still
//     produces [], but a struct built anywhere else has to hold the line itself.
package check

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/exitcode"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// Status is what became of a check. A frozen closed enum: consumers switch on
// these strings, so adding a seventh value is a breaking change to the schema and
// spelling one differently is a silent one.
type Status string

// The six statuses. The three that are not gradeable exist because "we did not
// find out" has three different causes and they need three different responses:
//
//	skipped      we chose not to run it -- not selected, needs a parameter,
//	             or needs a privilege this process does not have.
//	unavailable  the host cannot answer -- no docker, no systemd, no thermal
//	             zone. An expected absence on some hosts, not a fault.
//	error        it ran and broke. Always worth a look, never normal.
//
// Collapsing these into one would make a fleet-wide "docker checks unavailable"
// indistinguishable from "the docker check is crashing", which is the difference
// between a shrug and an incident.
const (
	StatusOK          Status = "ok"
	StatusWarn        Status = "warn"
	StatusCrit        Status = "crit"
	StatusSkipped     Status = "skipped"
	StatusUnavailable Status = "unavailable"
	StatusError       Status = "error"
)

// rank orders statuses for worst-of aggregation.
//
// error outranks warn deliberately. A warning is a host saying something mild and
// true; an error is a check that did not answer, and an unnoticed broken check is
// how a fleet stays green while a disk fills. crit still outranks both, because a
// definite bad state beats an unknown one when deciding what to wake someone for.
func (s Status) rank() int {
	switch s {
	case StatusOK:
		return 0
	case StatusSkipped:
		return 1
	case StatusUnavailable:
		return 2
	case StatusWarn:
		return 3
	case StatusError:
		return 4
	case StatusCrit:
		return 5
	default:
		// An unrecognised status is itself a defect, and ranking it below ok would
		// hide it. Rank it above everything so it cannot be ignored.
		return 6
	}
}

// Valid reports whether s is one of the six. Used where a Status crosses a
// boundary -- a decoded report, a check's return value -- rather than trusting
// that a string-typed enum holds only the constants.
func (s Status) Valid() bool { return s.rank() <= 5 }

// Worse returns the more serious of two statuses, so a check with several findings
// and a report with several checks both take the worst rather than the last.
func Worse(a, b Status) Status {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// ExitCode is the process exit code this status calls for.
//
// skipped and unavailable exit 0: a host with no docker is not an unhealthy host,
// and exiting non-zero for an absence would make every cron job on every
// docker-less host page forever. The counts block still reports them separately,
// so a fleet that does want to alert on "checks stopped being available" can.
func (s Status) ExitCode() int {
	switch s {
	case StatusWarn:
		return exitcode.Warn
	case StatusCrit:
		return exitcode.Crit
	case StatusOK, StatusSkipped, StatusUnavailable:
		return exitcode.OK
	default:
		return exitcode.Unknown
	}
}

// FromSeverity converts a graded severity into a status. The three gradeable
// statuses are exactly threshold's three severities, and this is the only place
// that correspondence is written down.
func FromSeverity(sev threshold.Severity) Status {
	switch sev {
	case threshold.Crit:
		return StatusCrit
	case threshold.Warn:
		return StatusWarn
	default:
		return StatusOK
	}
}

// An ID is a check's stable public name, dotted like "disk.usage".
//
// Public API: it appears in checks[], in --check selectors, in an operator's
// Ansible, and in whatever dashboard consumes the metrics. An ID may be retired,
// never repurposed -- see Retired.
type ID string

// Retired holds IDs that once meant something and no longer do.
//
// Reusing a retired ID for a different measurement is the worst kind of breaking
// change, because nothing errors: the operator's alert keeps firing on a name that
// now means something else. The registry refuses to register one, which turns a
// silent semantic break into a build-time failure.
var Retired = map[ID]string{}

// A Spec is everything the runner needs to know about a check without running it:
// what to call it, what it needs, and whether to run it at all.
type Spec struct {
	// ID is the stable public name. Required.
	ID ID

	// Title is the human name, for the TUI and for `--list`.
	Title string

	// Domain groups checks the way the TUI's nav does -- "disk", "network",
	// "docker" -- and is what --check disk selects as a prefix.
	Domain string

	// Tier2 means the check needs root or a working sudo -n. The runner decides
	// at run time rather than at registration, because a sudo credential can be
	// granted or expire between the two; v1 evaluated it once at compose time and
	// never re-checked.
	Tier2 bool

	// NeedsBins are external programs the check shells out to. The runner looks
	// them up and reports unavailable before calling Run, so a missing smartctl
	// is one answer written once rather than an error message per check.
	NeedsBins []string

	// Params, when non-empty, makes the check operator-driven: it needs an
	// argument nobody can guess, so a default run skips it rather than inventing
	// one. Inspecting a path is the example -- there is no sensible default path.
	Params []ParamSpec

	// Budget caps how long Run may take; the runner enforces it with a context
	// deadline. Zero means the runner's default. A check that ignores its context
	// still gets cut off at the result, so a hung traceroute cannot hold a cron
	// job open forever.
	Budget time.Duration

	// InDefault says whether a bare `fleetfix check` runs this. False for
	// anything slow, noisy or operator-driven; --check names it explicitly.
	InDefault bool
}

// A ParamSpec is one argument an operator-driven check takes.
type ParamSpec struct {
	Name        string
	Description string
	Required    bool
	// Default is used when the operator did not supply the parameter. A required
	// parameter with no default and no value makes the check skipped, not errored:
	// not asking for something is not a failure.
	Default string
}

// Validate reports what is wrong with a Spec. Registration calls it, so a
// malformed check fails at startup rather than producing a report field that is
// empty for reasons nobody can trace.
func (s Spec) Validate() error {
	switch {
	case s.ID == "":
		return fmt.Errorf("check has no id")
	case !validID(string(s.ID)):
		return fmt.Errorf("%s: an id must be dotted lower-case, like disk.usage", s.ID)
	case s.Title == "":
		return fmt.Errorf("%s: no title, which is what --list and the TUI print", s.ID)
	case s.Domain == "":
		return fmt.Errorf("%s: no domain, so --check %s would not select it", s.ID, s.ID)
	case !strings.HasPrefix(string(s.ID), s.Domain+"."):
		// Selection is by dotted prefix, so an id that does not start with its own
		// domain is unselectable by domain -- silently, and only for that check.
		return fmt.Errorf("%s: id does not start with its domain %q", s.ID, s.Domain)
	case s.Budget < 0:
		return fmt.Errorf("%s: a negative budget would cancel the check before it started", s.ID)
	}
	for _, p := range s.Params {
		if p.Name == "" {
			return fmt.Errorf("%s: a parameter has no name", s.ID)
		}
	}
	return nil
}

func validID(id string) bool {
	if id == "" || !strings.Contains(id, ".") {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
		default:
			return false
		}
	}
	return !strings.HasPrefix(id, ".") && !strings.HasSuffix(id, ".") && !strings.Contains(id, "..")
}

// Input is what a check is given to do its work.
type Input struct {
	// Params are the operator's arguments, already defaulted by the runner.
	Params map[string]string

	// Progress is never nil. A check emits to it as it goes; whether that paints
	// a TUI row, appends to steps[] or goes nowhere is not the check's business.
	// This is what lets the network ladder be one implementation instead of a
	// streaming one for the TUI and a batch one for the report.
	Progress Emitter

	// Thresholds is the host's grading policy, already merged from every config
	// layer. Passed in rather than read, so a check cannot grade by a policy the
	// report does not describe.
	Thresholds threshold.Set
}

// Param reads a parameter. The runner has already applied every spec default, so a
// check reading one it declared always gets a value; an empty string means the
// operator supplied nothing and the spec offered no default.
func (in Input) Param(name string) string { return in.Params[name] }

// An Event is one step of a running check, in plain text.
//
// Text is never markup. v1's ProbeOutput.verdict carried Textual markup, which is
// exactly why it could not be reused headlessly -- and why network.py had to
// escape all tool output to keep a hostname containing brackets from corrupting
// the display. Formatting lives in internal/render, colour in internal/tui.
type Event struct {
	Text   string `json:"text"`
	Status Status `json:"status"`
}

// An Emitter receives progress events. Implementations must tolerate being called
// from a goroutine other than the one that created them.
type Emitter interface {
	Emit(Event)
}

// EmitterFunc adapts a function to Emitter.
type EmitterFunc func(Event)

// Emit calls f.
func (f EmitterFunc) Emit(e Event) { f(e) }

// Discard is the Emitter for a caller that wants no progress at all. Checks are
// promised a non-nil Progress, and handing them this is cheaper than making every
// check nil-check its own input.
var Discard Emitter = EmitterFunc(func(Event) {})

// A Metric is one number a check produced, in the shape the alerting contract
// promises. Unlike Data, which is best-effort and shaped by the check, metrics[]
// is what a dashboard and `--prom` are allowed to depend on.
type Metric struct {
	// Name is dotted like an ID and is a public name in its own right.
	Name string `json:"name"`

	Value float64 `json:"value"`

	// Unit is the operator-facing label: "%", "bytes", "count", "seconds".
	Unit string `json:"unit"`

	// Labels distinguish several readings of the same metric -- a mount point, an
	// interface, a thermal zone. Never nil. JSON marshals map keys sorted, so this
	// does not threaten byte-stability.
	Labels map[string]string `json:"labels"`

	// Kind is "gauge" or "counter", which is the one distinction a Prometheus
	// scrape cannot infer and cannot do without.
	Kind MetricKind `json:"kind"`

	// Help is the HELP line in --prom output.
	Help string `json:"help"`
}

// MetricKind is a metric's Prometheus type.
type MetricKind string

// Gauge and Counter are the two kinds. Histograms and summaries are not here
// because nothing in the port produces one, and a kind nothing emits is a wire
// value a consumer would have to handle for no reason.
const (
	Gauge   MetricKind = "gauge"
	Counter MetricKind = "counter"
)

// A Result is what a check found. This is the checks[] element, so its JSON tags
// are the public contract.
type Result struct {
	ID      ID     `json:"id"`
	Status  Status `json:"status"`
	Summary string `json:"summary"`

	// Trips are the graded findings: which rule fired, on what value, against
	// which bound. Never nil.
	Trips []threshold.Trip `json:"trips"`

	// Metrics is the alerting contract. Never nil.
	Metrics []Metric `json:"metrics"`

	// Data is best-effort detail whose shape depends on the id, documented as
	// such so no consumer treats it as stable. May be null.
	Data any `json:"data"`

	// Steps are the progress events, in order. Never nil, and empty for a check
	// that emits nothing.
	Steps []Event `json:"steps"`

	// Error is the reason a check errored, in prose, and empty otherwise. A code
	// rather than prose would be better for machines, but every producer here is
	// a Go error whose text is the only thing that distinguishes two failures.
	Error string `json:"error"`

	// Duration is how long Run took. Not on the wire: the envelope carries one
	// duration_ms for the whole report, and a per-check time would make every
	// consecutive run differ. `fleetfix doctor` prints it.
	Duration time.Duration `json:"-"`
}

// Normalize fills in the empty slices the wire format requires and derives the
// status from the trips when the check did not set one.
//
// The []-versus-null rule is a class of cosmetic difference the differential
// harness exists to keep out of the diff, and a check author cannot be expected to
// remember it -- so the runner applies this to everything on the way out, and this
// method is exported so a caller assembling a Result by hand can too.
func (r Result) Normalize() Result {
	if r.Trips == nil {
		r.Trips = []threshold.Trip{}
	}
	if r.Metrics == nil {
		r.Metrics = []Metric{}
	}
	if r.Steps == nil {
		r.Steps = []Event{}
	}
	for i, m := range r.Metrics {
		if m.Labels == nil {
			r.Metrics[i].Labels = map[string]string{}
		}
	}
	if r.Status == "" {
		r.Status = StatusOK
		for _, t := range r.Trips {
			r.Status = Worse(r.Status, FromSeverity(t.Severity))
		}
	}
	return r
}

// A Check is one thing FleetFix can find out about a host.
//
// Run returns a Result rather than an error: "this check failed" is a finding
// about the host, not a failure of the run, and a report that omitted the checks
// that broke would be the most misleading document the tool could produce. A check
// that panics is caught by the runner and turned into StatusError for the same
// reason.
type Check interface {
	Spec() Spec
	Run(ctx context.Context, in Input) Result
}

// Counts is the report's tally, one field per status.
//
// Every field is present even at zero -- "crit": 0 is the answer a dashboard needs
// to draw a green panel, and an absent key would make it draw nothing.
type Counts struct {
	OK          int `json:"ok"`
	Warn        int `json:"warn"`
	Crit        int `json:"crit"`
	Skipped     int `json:"skipped"`
	Unavailable int `json:"unavailable"`
	Error       int `json:"error"`
}

// Tally counts results by status and returns the tally with the worst status seen.
//
// An empty run is ok rather than unknown: the runner reports "no checks selected"
// as its own condition, and folding that into a status here would make a
// deliberately narrow --check look like a broken one.
func Tally(results []Result) (Counts, Status) {
	var c Counts
	worst := StatusOK
	for _, r := range results {
		worst = Worse(worst, r.Status)
		switch r.Status {
		case StatusOK:
			c.OK++
		case StatusWarn:
			c.Warn++
		case StatusCrit:
			c.Crit++
		case StatusSkipped:
			c.Skipped++
		case StatusUnavailable:
			c.Unavailable++
		default:
			c.Error++
		}
	}
	return c, worst
}

// SortResults orders results by id, so two runs over the same host produce the
// same document. Checks run concurrently and finish in whatever order they finish
// in; without this the report would differ between consecutive runs on an
// unchanged host, which is the property the byte-stability test asserts.
func SortResults(results []Result) {
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })
}
