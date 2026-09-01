package checkcmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/config"
	"github.com/KingPin/FleetFix/v2/internal/exitcode"
	"github.com/KingPin/FleetFix/v2/internal/privilege"
	"github.com/KingPin/FleetFix/v2/internal/report"
	"github.com/KingPin/FleetFix/v2/internal/resolve"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// A fake is one check whose behaviour the test states outright. Everything here
// is about what this package does with a result, never about what a real
// collector would produce.
type fake struct {
	spec check.Spec
	run  func(ctx context.Context, in check.Input) check.Result
}

func (f fake) Spec() check.Spec { return f.spec }

func (f fake) Run(ctx context.Context, in check.Input) check.Result {
	if f.run == nil {
		return check.Result{Status: check.StatusOK, Summary: "nothing to report"}
	}
	return f.run(ctx, in)
}

// spec builds a valid default-set spec from a dotted id. The domain is the id's
// first segment, which Spec.Validate requires and `--check disk` relies on.
func spec(id string) check.Spec {
	domain, _, _ := strings.Cut(id, ".")
	return check.Spec{
		ID:        check.ID(id),
		Title:     strings.ToUpper(id),
		Domain:    domain,
		InDefault: true,
	}
}

// ok is a check that passes.
func ok(id string) fake { return fake{spec: spec(id)} }

// says is a check that reports one status and nothing else.
func says(id string, status check.Status) fake {
	return fake{
		spec: spec(id),
		run: func(context.Context, check.Input) check.Result {
			return check.Result{Status: status, Summary: string(status)}
		},
	}
}

func registry(t *testing.T, checks ...check.Check) *check.Registry {
	t.Helper()
	reg := check.NewRegistry()
	for _, c := range checks {
		if err := reg.Register(c); err != nil {
			t.Fatalf("registering a test check failed: %v", err)
		}
	}
	return reg
}

// clock advances a fixed step per reading, so a run's duration is a number the
// test chose rather than however long the machine took.
func clock(start time.Time, step time.Duration) func() time.Time {
	now := start
	return func() time.Time {
		out := now
		now = now.Add(step)
		return out
	}
}

var epoch = time.Date(2026, 7, 31, 22, 15, 30, 500*int(time.Millisecond), time.UTC)

func testPaths(t *testing.T) *config.Paths {
	t.Helper()
	root := t.TempDir()
	return &config.Paths{
		SystemDir: filepath.Join(root, "etc"),
		UserDir:   filepath.Join(root, "user"),
	}
}

// resolved builds a host that answers nothing: no config files, no docker, no
// sudo, an unreadable /proc. Tests add only the fact they are about.
func resolved(paths *config.Paths, runner cmdrun.Runner) *resolve.Resolved {
	return resolve.New(resolve.Options{
		Paths:      paths,
		Runner:     runner,
		Looker:     cmdrun.NewFakeLooker(),
		Getenv:     func(k string) string { return map[string]string{"USER": "operator"}[k] },
		Privilege:  &privilege.Prober{Runner: runner, UID: 1000},
		DetectHost: func() report.Host { return report.Host{Hostname: "testbox", Arch: "amd64"} },
	})
}

func host(t *testing.T) *resolve.Resolved {
	t.Helper()
	return resolved(testPaths(t), cmdrun.NewFake())
}

// out captures one invocation.
type out struct {
	stdout strings.Builder
	code   int
}

func invoke(t *testing.T, opts Options) *out {
	t.Helper()
	got := &out{}
	if opts.Resolved == nil {
		opts.Resolved = host(t)
	}
	if opts.Now == nil {
		opts.Now = clock(epoch, 150*time.Millisecond)
	}
	if opts.Version == "" {
		opts.Version = "2.0.0-test"
	}
	opts.Stdout = &got.stdout

	code, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	got.code = code
	return got
}

// decoded returns the document, having first insisted stdout is exactly one.
func (o *out) decoded(t *testing.T) report.Report {
	t.Helper()
	var rep report.Report
	if err := json.Unmarshal([]byte(o.stdout.String()), &rep); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, o.stdout.String())
	}
	return rep
}

func TestARunProducesTheDocumentedEnvelope(t *testing.T) {
	got := invoke(t, Options{Registry: registry(t, ok("disk.usage"), ok("net.ladder"))})

	rep := got.decoded(t)
	if rep.Schema != report.Schema {
		t.Errorf("schema = %q, want %q", rep.Schema, report.Schema)
	}
	if rep.FleetFixVersion != "2.0.0-test" {
		t.Errorf("fleetfix_version = %q", rep.FleetFixVersion)
	}
	if rep.Host.Hostname != "testbox" {
		t.Errorf("host.hostname = %q, want the resolved host", rep.Host.Hostname)
	}
	if rep.Status != check.StatusOK || rep.ExitCode != exitcode.OK {
		t.Errorf("status = %q exit_code = %d, want ok/0", rep.Status, rep.ExitCode)
	}
	if got.code != exitcode.OK {
		t.Errorf("process exit code = %d, want %d", got.code, exitcode.OK)
	}
	if rep.Counts.OK != 2 {
		t.Errorf("counts.ok = %d, want 2", rep.Counts.OK)
	}
	// Ordered by id, not by whichever goroutine finished first.
	if len(rep.Checks) != 2 || rep.Checks[0].ID != "disk.usage" || rep.Checks[1].ID != "net.ladder" {
		t.Errorf("checks = %+v, want disk.usage then net.ladder", rep.Checks)
	}
}

// The envelope's timestamp is the moment the run started, not the moment it was
// written: a consumer correlating this against a log line wants to know when
// FleetFix looked, and duration_ms already says how long it looked for.
func TestTheClockIsTheEnvelopesTimestamp(t *testing.T) {
	got := invoke(t, Options{
		Registry: registry(t, ok("disk.usage")),
		Now:      clock(epoch, 250*time.Millisecond),
	})

	rep := got.decoded(t)
	if want := report.Timestamp(epoch); rep.GeneratedAt != want {
		t.Errorf("generated_at = %q, want %q", rep.GeneratedAt, want)
	}
	if rep.DurationMS != 250 {
		t.Errorf("duration_ms = %d, want 250", rep.DurationMS)
	}
}

// The M3 exit criterion, asserted where it can be: two runs over an unchanged
// host differ only in the two fields that describe the run rather than the host.
func TestConsecutiveRunsDifferOnlyInTheirTimestamps(t *testing.T) {
	shared := host(t)
	reg := registry(t, ok("disk.usage"), says("net.ladder", check.StatusWarn))

	first := invoke(t, Options{Registry: reg, Resolved: shared, Now: clock(epoch, 150*time.Millisecond)})
	second := invoke(t, Options{Registry: reg, Resolved: shared, Now: clock(epoch.Add(time.Hour), 2*time.Second)})

	if a, b := redacted(t, first), redacted(t, second); a != b {
		t.Errorf("consecutive runs differ after redaction:\n%s\n%s", a, b)
	}
}

// redacted drops the two keys that describe the invocation rather than the host.
// Dropped rather than blanked, so a key that stopped being emitted at all would
// still show up as a difference.
func redacted(t *testing.T, o *out) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(o.stdout.String()), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	delete(doc, "generated_at")
	delete(doc, "duration_ms")
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A build with no collectors is the state of this branch, and it has to produce
// a document rather than a bare error line: the parser reading it does not care
// why the run went wrong, only that it can still read the answer.
func TestAnEmptyRegistryIsStillAValidDocument(t *testing.T) {
	got := invoke(t, Options{})

	rep := got.decoded(t)
	if rep.Status != check.StatusError {
		t.Errorf("status = %q, want error", rep.Status)
	}
	if got.code != exitcode.Unknown || rep.ExitCode != exitcode.Unknown {
		t.Errorf("exit = %d, document exit_code = %d, want %d", got.code, rep.ExitCode, exitcode.Unknown)
	}
	if !strings.Contains(rep.Error, "no checks are registered") {
		t.Errorf("error = %q", rep.Error)
	}
	if rep.Checks == nil {
		t.Error("checks is null; a consumer iterating it gets an error rather than nothing")
	}
}

// The objection the M1 stand-in raised, answered. A failed run emits the real
// schema, so every block a consumer pins on has to be there and populated --
// otherwise the pin would accept a document that inspected nothing.
func TestAFailedRunStillCarriesEveryEnvelopeBlock(t *testing.T) {
	rep := invoke(t, Options{}).decoded(t)

	switch {
	case rep.Host.Hostname == "":
		t.Error("host block is empty on a failed run")
	case rep.Privilege.Reason == "":
		t.Error("privilege block carries no reason on a failed run")
	case rep.Operator.UnixUser == "":
		t.Error("operator block is empty on a failed run")
	case rep.ConfigWarnings == nil:
		t.Error("config_warnings is null rather than an empty array")
	}
}

func TestATypoInASelectorIsLoudAndStillJSON(t *testing.T) {
	got := invoke(t, Options{
		Registry: registry(t, ok("disk.usage")),
		Include:  []string{"dsk"},
	})

	rep := got.decoded(t)
	if got.code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d: a typo that ran nothing must not read as a healthy host", got.code, exitcode.Unknown)
	}
	if !strings.Contains(rep.Error, "dsk") {
		t.Errorf("error does not quote the selector: %q", rep.Error)
	}
	if rep.Counts.OK != 0 {
		t.Errorf("counts.ok = %d, want 0: nothing ran", rep.Counts.OK)
	}
}

func TestAnEmptySelectionExplainsWhichKindItWas(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{
			"nothing registered",
			Options{},
			"no checks are registered",
		},
		{
			"nothing runs by default",
			Options{Registry: registry(t, fake{spec: check.Spec{
				ID: "disk.inspect", Title: "Inspect", Domain: "disk",
			}})},
			"no checks run by default",
		},
		{
			"selected and then excluded",
			Options{
				Registry: registry(t, ok("disk.usage")),
				Include:  []string{"disk"},
				Exclude:  []string{"disk.usage"},
			},
			"every selected check was also excluded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := invoke(t, tc.opts).decoded(t)
			if !strings.Contains(rep.Error, tc.want) {
				t.Errorf("error = %q, want it to mention %q", rep.Error, tc.want)
			}
		})
	}
}

func TestTheExitCodeFollowsTheWorstFinding(t *testing.T) {
	cases := []struct {
		status check.Status
		want   int
	}{
		{check.StatusOK, exitcode.OK},
		{check.StatusSkipped, exitcode.OK},
		{check.StatusUnavailable, exitcode.OK},
		{check.StatusWarn, exitcode.Warn},
		{check.StatusCrit, exitcode.Crit},
		{check.StatusError, exitcode.Unknown},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			got := invoke(t, Options{Registry: registry(t, says("disk.usage", tc.status))})

			if got.code != tc.want {
				t.Errorf("exit code = %d, want %d", got.code, tc.want)
			}
			if rep := got.decoded(t); rep.ExitCode != tc.want {
				t.Errorf("document exit_code = %d, want %d", rep.ExitCode, tc.want)
			}
		})
	}
}

// --strict is for a fleet where an unmonitored host is the thing worth knowing
// about: a run that found nothing wrong because it did not look is not a pass.
func TestStrictTurnsNotFindingOutIntoUnknown(t *testing.T) {
	for _, status := range []check.Status{check.StatusSkipped, check.StatusUnavailable} {
		t.Run(string(status), func(t *testing.T) {
			got := invoke(t, Options{
				Registry: registry(t, says("disk.usage", status)),
				Strict:   true,
			})

			if got.code != exitcode.Unknown {
				t.Errorf("exit code = %d, want %d under --strict", got.code, exitcode.Unknown)
			}
			// The document reports the host, not the flag. A report stored and read
			// a week later should say what was found, not what this caller wanted
			// done about it.
			if rep := got.decoded(t); rep.ExitCode != exitcode.OK {
				t.Errorf("document exit_code = %d, want %d: --strict is about the process", rep.ExitCode, exitcode.OK)
			}
		})
	}
}

func TestStrictLeavesARealVerdictAlone(t *testing.T) {
	got := invoke(t, Options{
		Registry: registry(t, says("disk.usage", check.StatusWarn), says("net.ladder", check.StatusSkipped)),
		Strict:   true,
	})

	if got.code != exitcode.Warn {
		t.Errorf("exit code = %d, want %d: --strict raises silence, not a finding", got.code, exitcode.Warn)
	}
}

func TestStrictIsQuietWhenEverythingRan(t *testing.T) {
	got := invoke(t, Options{Registry: registry(t, ok("disk.usage")), Strict: true})

	if got.code != exitcode.OK {
		t.Errorf("exit code = %d, want %d", got.code, exitcode.OK)
	}
}

// --exit-zero is for a pipeline where a non-zero code kills the job and the JSON
// is the actual product. The verdict does not disappear; it stays in the
// document, where it already was.
func TestExitZeroKeepsTheVerdictInTheDocument(t *testing.T) {
	got := invoke(t, Options{Registry: registry(t, says("disk.usage", check.StatusCrit)), ExitZero: true})

	if got.code != exitcode.OK {
		t.Errorf("exit code = %d, want 0", got.code)
	}
	rep := got.decoded(t)
	if rep.Status != check.StatusCrit || rep.ExitCode != exitcode.Crit {
		t.Errorf("document says status %q exit_code %d; the finding was erased", rep.Status, rep.ExitCode)
	}
}

func TestExitZeroSurvivesStrict(t *testing.T) {
	got := invoke(t, Options{
		Registry: registry(t, says("disk.usage", check.StatusSkipped)),
		Strict:   true,
		ExitZero: true,
	})

	if got.code != exitcode.OK {
		t.Errorf("exit code = %d, want 0: --exit-zero is the last word", got.code)
	}
}

func TestNDJSONIsAnEnvelopeThenOneObjectPerCheck(t *testing.T) {
	got := invoke(t, Options{
		Registry: registry(t, ok("disk.usage"), says("net.ladder", check.StatusWarn)),
		Format:   FormatNDJSON,
	})

	lines := strings.Split(strings.TrimSuffix(got.stdout.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want an envelope and two checks:\n%s", len(lines), got.stdout.String())
	}

	var envelope report.Report
	if err := json.Unmarshal([]byte(lines[0]), &envelope); err != nil {
		t.Fatalf("the first line is not the envelope: %v", err)
	}
	// The envelope goes first so a streaming reader can route the rest, which
	// means its checks[] has to be empty rather than a copy of what follows.
	if len(envelope.Checks) != 0 {
		t.Errorf("the envelope carries %d checks; a streaming reader would see them twice", len(envelope.Checks))
	}
	if envelope.Status != check.StatusWarn {
		t.Errorf("envelope status = %q, want warn", envelope.Status)
	}

	for i, line := range lines[1:] {
		var res check.Result
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			t.Fatalf("line %d is not a check result: %v", i+2, err)
		}
		if res.ID == "" {
			t.Errorf("line %d carries no id", i+2)
		}
	}
	if got.code != exitcode.Warn {
		t.Errorf("exit code = %d, want %d; the format must not change the verdict", got.code, exitcode.Warn)
	}
}

func TestTheDefaultFormatIsOneIndentedDocument(t *testing.T) {
	plain := invoke(t, Options{Registry: registry(t, ok("disk.usage"))})
	named := invoke(t, Options{Registry: registry(t, ok("disk.usage")), Format: FormatJSON})

	if plain.stdout.String() != named.stdout.String() {
		t.Error("the zero Format is not FormatJSON")
	}
	if !strings.Contains(plain.stdout.String(), "\n  \"schema\"") {
		t.Errorf("the document is not indented:\n%s", plain.stdout.String())
	}
}

// The one stream this package must not put a diagnostic on is the one it writes
// to, so a write failure goes back to the caller, who owns stderr.
func TestAWriteFailureIsReturnedRatherThanPrinted(t *testing.T) {
	broken := errors.New("broken pipe")

	code, err := Run(t.Context(), Options{
		Stdout:   failingWriter{err: broken},
		Registry: registry(t, ok("disk.usage")),
		Resolved: host(t),
		Now:      clock(epoch, time.Millisecond),
	})

	if !errors.Is(err, broken) {
		t.Errorf("err = %v, want it to wrap %v", err, broken)
	}
	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
}

// failingWriter is a stdout that will not accept writes -- a closed pipe, which
// is what `fleetfix check --json | head -1` produces.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestAWriteFailureInTheListingIsReturnedToo(t *testing.T) {
	broken := errors.New("broken pipe")

	code, err := Run(t.Context(), Options{
		Stdout:   failingWriter{err: broken},
		List:     true,
		Registry: registry(t, ok("disk.usage")),
		Resolved: host(t),
	})

	if !errors.Is(err, broken) {
		t.Errorf("err = %v, want it to wrap %v", err, broken)
	}
	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
}

func TestTheParamsReachTheCheck(t *testing.T) {
	var seen string
	inspect := fake{
		spec: check.Spec{
			ID: "disk.inspect", Title: "Inspect", Domain: "disk",
			Params: []check.ParamSpec{{Name: "path", Description: "what to look at", Required: true}},
		},
		run: func(_ context.Context, in check.Input) check.Result {
			seen = in.Param("path")
			return check.Result{Status: check.StatusOK, Summary: seen}
		},
	}

	invoke(t, Options{
		Registry: registry(t, inspect),
		Include:  []string{"disk.inspect"},
		Params:   map[string]string{"path": "/var/log"},
	})

	if seen != "/var/log" {
		t.Errorf("the check read path = %q, want /var/log", seen)
	}
}

// The runner is built from the resolved host, so a check cannot be graded by a
// policy the envelope does not describe.
func TestTheRunnerGradesByTheResolvedPolicy(t *testing.T) {
	paths := testPaths(t)
	writeYAML(t, paths.SystemDir, config.ThresholdsFile, "disk.used_pct:\n  warn: 55\n")

	var handed threshold.Set
	inspect := fake{
		spec: spec("disk.usage"),
		run: func(_ context.Context, in check.Input) check.Result {
			handed = in.Thresholds
			return check.Result{Status: check.StatusOK}
		},
	}

	invoke(t, Options{
		Registry: registry(t, inspect),
		Resolved: resolved(paths, cmdrun.NewFake()),
	})

	rule, found := handed.Get(threshold.DiskUsedPct)
	if !found {
		t.Fatal("the check was handed no policy for disk.used_pct")
	}
	if rule.Warn != 55 {
		t.Errorf("warn = %g, want the resolved 55", rule.Warn)
	}
}

// And gated by the same privilege answer the envelope reports, rather than a
// second probe that could disagree with it.
func TestTier2IsGatedByTheResolvedPrivilege(t *testing.T) {
	tier2 := fake{spec: check.Spec{
		ID: "disk.smart", Title: "SMART", Domain: "disk", Tier2: true, InDefault: true,
	}}

	rep := invoke(t, Options{Registry: registry(t, tier2)}).decoded(t)

	if len(rep.Checks) != 1 {
		t.Fatalf("got %d checks, want 1", len(rep.Checks))
	}
	if rep.Checks[0].Status != check.StatusSkipped {
		t.Errorf("status = %q, want skipped on a host that cannot escalate", rep.Checks[0].Status)
	}
	if rep.Privilege.CanTier2 {
		t.Error("the envelope claims Tier 2 while the check was skipped for want of it")
	}
}

// An explicit Runner wins, which is what the TUI needs when it wants live
// progress out of the same checks.
func TestAnExplicitRunnerIsUsedAsGiven(t *testing.T) {
	var events int
	invoke(t, Options{
		Registry: registry(t, fake{
			spec: spec("disk.usage"),
			run: func(_ context.Context, in check.Input) check.Result {
				in.Progress.Emit(check.Event{Text: "looking", Status: check.StatusOK})
				return check.Result{Status: check.StatusOK}
			},
		}),
		Runner: &check.Runner{
			Concurrency: 1,
			Progress:    check.EmitterFunc(func(check.Event) { events++ }),
		},
	})

	if events != 1 {
		t.Errorf("the supplied runner saw %d events, want 1", events)
	}
}

func TestListDescribesTheBuild(t *testing.T) {
	inspect := fake{spec: check.Spec{
		ID: "disk.inspect", Title: "Inspect a path", Domain: "disk",
		Tier2: true, NeedsBins: []string{"du"}, Budget: 3 * time.Second,
		Params: []check.ParamSpec{{
			Name: "path", Description: "what to look at", Required: true, Default: "/var",
		}},
	}}
	got := invoke(t, Options{List: true, Registry: registry(t, inspect, ok("net.ladder"))})

	if got.code != exitcode.OK {
		t.Errorf("exit code = %d, want 0: being asked what the build can do says nothing about the host", got.code)
	}

	var listing Listing
	if err := json.Unmarshal([]byte(got.stdout.String()), &listing); err != nil {
		t.Fatalf("the listing is not one JSON document: %v\n%s", err, got.stdout.String())
	}
	if listing.Schema != ListSchema {
		t.Errorf("schema = %q, want %q", listing.Schema, ListSchema)
	}
	if listing.Schema == report.Schema {
		t.Error("the listing reuses the report's schema; a consumer pinning one would accept the other")
	}
	if listing.FleetFixVersion != "2.0.0-test" {
		t.Errorf("fleetfix_version = %q", listing.FleetFixVersion)
	}
	if len(listing.Checks) != 2 {
		t.Fatalf("got %d checks, want 2", len(listing.Checks))
	}

	want := ListedCheck{
		ID: "disk.inspect", Title: "Inspect a path", Domain: "disk",
		Tier2: true, NeedsBins: []string{"du"}, BudgetMS: 3000, InDefault: false,
		Params: []ListedParam{{
			Name: "path", Description: "what to look at", Required: true, Default: "/var",
		}},
	}
	first := listing.Checks[0]
	if first.ID != want.ID || first.Title != want.Title || first.Domain != want.Domain ||
		first.Tier2 != want.Tier2 || first.BudgetMS != want.BudgetMS || first.InDefault != want.InDefault {
		t.Errorf("listed check = %+v, want %+v", first, want)
	}
	if len(first.NeedsBins) != 1 || first.NeedsBins[0] != "du" {
		t.Errorf("needs_bins = %v, want [du]", first.NeedsBins)
	}
	if len(first.Params) != 1 || first.Params[0] != want.Params[0] {
		t.Errorf("params = %+v, want %+v", first.Params, want.Params)
	}
}

// Reporting a literal 0 for "whatever the runner's default is" would tell an
// operator reading the listing that the check has no time to run.
func TestTheListingReportsTheEffectiveBudget(t *testing.T) {
	got := invoke(t, Options{List: true, Registry: registry(t, ok("disk.usage"))})

	var listing Listing
	if err := json.Unmarshal([]byte(got.stdout.String()), &listing); err != nil {
		t.Fatal(err)
	}
	if want := check.DefaultBudget.Milliseconds(); listing.Checks[0].BudgetMS != want {
		t.Errorf("budget_ms = %d, want the runner's default %d", listing.Checks[0].BudgetMS, want)
	}
}

// Same wire rules as the report, for the same reason: a consumer generating an
// Ansible task list off this reads it exactly as it reads the report.
func TestTheListingHasNoNulls(t *testing.T) {
	got := invoke(t, Options{List: true, Registry: registry(t, ok("disk.usage"))})

	if strings.Contains(got.stdout.String(), "null") {
		t.Errorf("the listing contains a null:\n%s", got.stdout.String())
	}
	for _, want := range []string{`"params": []`, `"needs_bins": []`} {
		if !strings.Contains(got.stdout.String(), want) {
			t.Errorf("the listing does not carry %s:\n%s", want, got.stdout.String())
		}
	}
}

func TestAnEmptyListingIsStillADocument(t *testing.T) {
	got := invoke(t, Options{List: true})

	if got.code != exitcode.OK {
		t.Errorf("exit code = %d, want 0", got.code)
	}
	if !strings.Contains(got.stdout.String(), `"checks": []`) {
		t.Errorf("checks is not an empty array:\n%s", got.stdout.String())
	}
}

// --list answers a question about the build, so it must not go asking the host
// anything -- including the sudo probe every other path pays for.
func TestListAsksTheHostNothing(t *testing.T) {
	runner := cmdrun.NewFake()

	invoke(t, Options{
		List:     true,
		Resolved: resolved(testPaths(t), runner),
		Registry: registry(t, ok("disk.usage")),
	})

	if calls := runner.Calls(); len(calls) != 0 {
		t.Errorf("--list ran %v", calls)
	}
}

// The nil-Options defaults, exercised through the one path that resolves the
// live host without asking it anything: an empty build's listing is the same
// document on every machine.
func TestTheZeroOptionsListsThisBuild(t *testing.T) {
	var stdout strings.Builder

	code, err := Run(t.Context(), Options{Stdout: &stdout, List: true})
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if code != exitcode.OK {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), ListSchema) {
		t.Errorf("the listing carries no schema:\n%s", stdout.String())
	}
}

func TestUsageDocumentsEveryFlag(t *testing.T) {
	var w strings.Builder
	Usage(&w)

	for _, flag := range []string{"--json", "--ndjson", "--check", "--exclude", "--param", "--list", "--exit-zero", "--strict"} {
		if !strings.Contains(w.String(), flag) {
			t.Errorf("usage does not document %s:\n%s", flag, w.String())
		}
	}
	// The exit codes are what a monitoring integration is configured against, so
	// they belong in the help rather than only in the README.
	if !strings.Contains(w.String(), "Exit codes:") {
		t.Errorf("usage does not state the exit codes:\n%s", w.String())
	}
}

func writeYAML(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
