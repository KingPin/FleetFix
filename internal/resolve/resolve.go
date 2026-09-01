// Package resolve answers, once per process, every question about the host and
// its configuration that more than one front door needs.
//
// It exists because of one M3 exit criterion: `fleetfix doctor` and
// `fleetfix check --json` must agree about which runtime and which config files
// are in play. Two resolvers is how a support call ends with "but doctor says
// the threshold is 85" -- doctor read one set of files, the checks read another,
// and both were telling the truth about what they read. Here there is one set,
// read once, and doctor prints exactly what the checks graded by.
//
// Reading once also makes a run internally consistent. Six separate readers
// would each see whatever thresholds.yml said at the moment they got to it, so
// an Ansible push landing mid-run could produce a report graded by two different
// policies with nothing in the document saying so.
//
// Nothing here fails. Every degradation -- an unreadable /proc, a file that will
// not parse, a stopped docker daemon -- becomes a warning or an empty field, and
// a fleet tool that stops reporting because of a stray tab is worse than one
// reporting on defaults.
package resolve

import (
	"context"
	"os"
	"sync"

	"github.com/KingPin/FleetFix/v2/internal/audit"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/config"
	"github.com/KingPin/FleetFix/v2/internal/container"
	corenet "github.com/KingPin/FleetFix/v2/internal/core/network"
	"github.com/KingPin/FleetFix/v2/internal/hostinfo"
	"github.com/KingPin/FleetFix/v2/internal/identity"
	"github.com/KingPin/FleetFix/v2/internal/privilege"
	"github.com/KingPin/FleetFix/v2/internal/report"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
	"github.com/KingPin/FleetFix/v2/internal/version"
)

// Files are the configuration files read on every run, in the order their
// warnings are reported and doctor lists them.
//
// Alphabetical, deliberately. The order has to be fixed -- config_warnings[] is
// compared between consecutive runs by the byte-stability test, and a map
// iteration would reorder it every time -- and alphabetical is the one order
// nobody has to remember or maintain as files are added.
//
// Every file is read even when this invocation will not consult it, because
// doctor's job is to answer "which files are in play?" and a list of only the
// files someone happened to open cannot tell an operator that the path they
// meant to write is not the path being read.
var Files = []string{
	config.IdentityFile,
	config.OtelFile,
	config.PathsFile,
	config.PerfFile,
	config.ProbesFile,
	config.ThresholdsFile,
}

// Options are the seams. The zero value resolves the live host.
type Options struct {
	// Paths is where configuration is read from. Nil means ResolvePaths(); a
	// pointer rather than a value so a test can ask for no configuration at all
	// without that being indistinguishable from not having said anything.
	Paths *config.Paths

	Runner cmdrun.Runner
	Looker cmdrun.Looker
	Getenv func(string) string

	// Privilege is the Tier 2 gate. Nil means one built from Runner for this
	// process; supply your own to be root without being root.
	Privilege *privilege.Prober

	// DetectHost names the machine. Nil means hostinfo.Detect, which reads the
	// live /proc and /etc.
	DetectHost func() report.Host

	// AuditPath decides where the trail goes. Nil means Paths.AuditPath, which
	// prefers /var/log and falls back to the state directory.
	//
	// A seam because the default's answer depends on who is running: as root it
	// is the real fleet-wide trail, and the CI smoke matrix runs a root leg. A
	// test that appended there would be writing to the file an operator greps.
	AuditPath func() (string, error)
}

// Resolved is the answer. One per process, shared by every front door.
//
// A pointer type: it memoises the container probe, so copying it would produce
// two memos and two subprocesses for one question.
type Resolved struct {
	Paths config.Paths

	// Host, Operator and Privilege are the report envelope's three identity
	// blocks, in the shapes the envelope wants. No mapping functions: two of
	// these are several same-typed fields in a row, and a transposition between
	// identical shapes compiles and reports the arch as the kernel forever.
	Host      report.Host
	Operator  identity.Operator
	Privilege *privilege.Prober

	// Thresholds is the grading policy every consumer uses: the TUI's colours,
	// trips[] in the report, the agent's trip events, doctor's listing.
	Thresholds threshold.Set

	// Probes is probes.yml merged over the defaults: which targets the network
	// checks probe and how long each is given. Resolved here for the same reason
	// as Thresholds -- a check grading against a file doctor does not describe is
	// how a support call ends with "but doctor says we ping 1.1.1.1".
	Probes corenet.Probes

	// Configs is every file in Files, merged across layers, keyed by file name.
	// Use Config rather than indexing, so a caller cannot silently read a nil map.
	Configs map[string]config.Loaded

	// Warnings is every complaint from every file, in Files order, then in the
	// order each file produced them. Never nil. This is config_warnings[].
	Warnings []string

	// Runner and Looker are carried so the collectors run their subprocesses
	// through the same seam this package probed with. A collector that reached
	// for os/exec directly would be untestable and, on a fake, unfaked.
	Runner cmdrun.Runner
	Looker cmdrun.Looker

	// runtime is the detected container runtime, before any daemon was asked.
	runtime container.Runtime

	// auditPath and the memo behind Audit. Separate from mu because opening the
	// trail and probing the container daemon have nothing to say to each other,
	// and a caller waiting on a docker probe should not be what delays the
	// intent line of a destructive action.
	auditPath  func() (string, error)
	auditOnce  sync.Once
	auditW     *audit.Writer
	auditErr   error
	auditWhere string

	// mu is held across the container probe, not just around the memo, so N
	// callers arriving together produce one subprocess. Same reasoning as
	// privilege.Prober: the probe is bounded, so the wait is bounded too.
	mu     sync.Mutex
	probed *container.Runtime
}

// New resolves the host. Never fails, never blocks on a subprocess: the two
// questions that cost one -- can this process escalate, does the container
// daemon answer -- are asked lazily by Privilege and Container.
func New(opts Options) *Resolved {
	if opts.Paths == nil {
		paths := config.ResolvePaths()
		opts.Paths = &paths
	}
	if opts.Runner == nil {
		opts.Runner = cmdrun.New()
	}
	if opts.Looker == nil {
		opts.Looker = cmdrun.NewPATH()
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Privilege == nil {
		opts.Privilege = privilege.New(opts.Runner)
	}
	if opts.DetectHost == nil {
		opts.DetectHost = hostinfo.Detect
	}
	if opts.AuditPath == nil {
		opts.AuditPath = opts.Paths.AuditPath
	}

	r := &Resolved{
		Paths:     *opts.Paths,
		Host:      opts.DetectHost(),
		Privilege: opts.Privilege,
		Configs:   make(map[string]config.Loaded, len(Files)),
		Warnings:  []string{},
		Runner:    opts.Runner,
		Looker:    opts.Looker,
		runtime:   container.Detect(opts.Looker, opts.Getenv),
		auditPath: opts.AuditPath,
	}

	// Thresholds, identity and probes are read through their own resolvers
	// rather than Load, because those are where each file's meaning lives -- the
	// bounds parser, the principals table, the probe knobs and their clamps --
	// and the warnings each produces belong with the file's own.
	thresholds, thresholdsLoaded := opts.Paths.Thresholds()
	principals, identityLoaded := opts.Paths.Principals()
	probes, probesLoaded := resolveProbes(*opts.Paths)
	r.Thresholds = thresholds
	r.Probes = probes
	r.Configs[config.ThresholdsFile] = thresholdsLoaded
	r.Configs[config.IdentityFile] = identityLoaded
	r.Configs[config.ProbesFile] = probesLoaded

	for _, name := range Files {
		if _, done := r.Configs[name]; done {
			continue
		}
		r.Configs[name] = opts.Paths.Load(name)
	}
	for _, name := range Files {
		r.Warnings = append(r.Warnings, r.Configs[name].Warnings...)
	}

	r.Operator = identity.From(opts.Getenv, principals)
	return r
}

// Config returns one file's merged configuration and the layers it came from.
//
// An unknown name yields an empty Loaded rather than a zero one, so a caller
// indexing Values does not read a nil map. Names are the constants in
// internal/config; anything else is a typo that will read as an empty file.
func (r *Resolved) Config(name string) config.Loaded {
	loaded, ok := r.Configs[name]
	if !ok || loaded.Values == nil {
		loaded.Values = map[string]any{}
	}
	return loaded
}

// Runtime is the container runtime as found on PATH, before any daemon was
// asked. Available is always false here; use Container for the probed answer.
func (r *Resolved) Runtime() container.Runtime { return r.runtime }

// Container probes the container daemon once and returns the answer thereafter.
//
// Memoised for the run rather than for a TTL, unlike the privilege probe: a
// daemon that comes up mid-run should not make one check in a report say
// unavailable and the next say ok, because a reader of that document has no way
// to tell it from a flapping daemon.
func (r *Resolved) Container(ctx context.Context) container.Runtime {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.probed == nil {
		probed := r.runtime.Probe(ctx, r.Runner)
		r.probed = &probed
	}
	return *r.probed
}

// Audit opens the audit trail on first use and returns the same writer after.
//
// Lazy, unlike everything else this package resolves. Opening the trail creates
// the file, and `check --json` runs from a scheduler on every host: creating a
// trail there would put an empty fleetfix-audit.log on every machine in a fleet,
// which tells a reader nothing and looks like it should. `doctor` does open it,
// because "can this install write its audit trail?" is one of the questions
// doctor exists to answer and there is no way to answer it without trying.
//
// One writer per process, because the session id is what joins a launch to the
// exit that followed it and the sequence is what orders records a millisecond
// timestamp cannot. Two writers would issue two of each.
//
// The error is the caller's to act on, not to log: a destructive action whose
// trail will not open must be refused, which is the only thing that makes the
// local file authoritative rather than aspirational.
func (r *Resolved) Audit() (*audit.Writer, error) {
	r.auditOnce.Do(func() {
		// The reason is kept, not returned. It says the trail fell back to the
		// state directory, which is doctor's to report and is not a failure --
		// the path beside it is usable either way.
		path, reason := r.auditPath()
		if reason != nil {
			r.auditWhere = reason.Error()
		}
		// No Sink. The OTLP destination resolves (audit.LoadOtelConfig) but
		// nothing exports yet, and a sink that silently dropped every record
		// would be worse than the absence an operator can see in doctor.
		r.auditW, r.auditErr = audit.New(audit.Options{
			Path:     path,
			Operator: r.Operator,
			Version:  version.Version(),
		})
	})
	return r.auditW, r.auditErr
}

// AuditFallback reports why the trail is not in /var/log, or "" when it is.
//
// Empty until Audit has been called, because the answer is a side effect of
// trying: /var/log exists and is root-owned everywhere, so statting it says
// nothing that opening the file does not say better.
func (r *Resolved) AuditFallback() string { return r.auditWhere }

// Meta fills the report envelope's host-facing half.
//
// Version, GeneratedAt and Duration are left to the caller: those are facts
// about the invocation, which this package deliberately knows nothing about --
// it is shared by a check run, a doctor run and, later, an agent tick.
//
// The Operator conversion is a struct conversion rather than a field-by-field
// copy on purpose: Go permits it only when the field names, order and types all
// match, so the compiler rejects the transposition a hand-written mapper would
// happily ship.
func (r *Resolved) Meta(ctx context.Context) report.Meta {
	return report.Meta{
		Host:           r.Host,
		Operator:       report.Operator(r.Operator),
		Privilege:      r.Privilege.State(ctx),
		ConfigWarnings: r.Warnings,
	}
}

// DescribeConfig renders the search path for doctor: every file, every layer,
// in precedence order, whether or not it exists.
func (r *Resolved) DescribeConfig() []string {
	var lines []string
	for _, name := range Files {
		lines = append(lines, name+":")
		for _, layer := range r.Config(name).DescribeLayers() {
			lines = append(lines, "  "+layer)
		}
	}
	return lines
}
