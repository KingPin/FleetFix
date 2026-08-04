package doctorcmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/config"
	"github.com/KingPin/FleetFix/v2/internal/exitcode"
	"github.com/KingPin/FleetFix/v2/internal/privilege"
	"github.com/KingPin/FleetFix/v2/internal/report"
	"github.com/KingPin/FleetFix/v2/internal/resolve"
)

// A fake is one check that exists to be described. Nothing here runs: doctor
// reports what a run would do, and never does it.
type fake struct{ spec check.Spec }

func (f fake) Spec() check.Spec { return f.spec }

func (fake) Run(context.Context, check.Input) check.Result {
	panic("doctor ran a check; it describes them and must not execute one")
}

// spec builds a valid default-set spec from a dotted id.
func spec(id string, bins ...string) check.Spec {
	domain, _, _ := strings.Cut(id, ".")
	return check.Spec{
		ID:        check.ID(id),
		Title:     strings.ToUpper(id),
		Domain:    domain,
		InDefault: true,
		NeedsBins: bins,
	}
}

func registry(t *testing.T, specs ...check.Spec) *check.Registry {
	t.Helper()
	reg := check.NewRegistry()
	for _, s := range specs {
		if err := reg.Register(fake{spec: s}); err != nil {
			t.Fatalf("registering a test check failed: %v", err)
		}
	}
	return reg
}

func testPaths(t *testing.T) *config.Paths {
	t.Helper()
	root := t.TempDir()
	return &config.Paths{
		SystemDir: filepath.Join(root, "etc"),
		UserDir:   filepath.Join(root, "user"),
	}
}

// bare is a host that answers nothing: no config, no docker, no sudo. Every test
// starts here and adds only the fact it is about, so an assertion about the
// container section cannot be satisfied by this developer's machine.
func bare(t *testing.T, opts ...func(*resolve.Options)) *resolve.Resolved {
	t.Helper()
	o := resolve.Options{
		Paths:      testPaths(t),
		Runner:     cmdrun.NewFake(),
		Looker:     cmdrun.NewFakeLooker(),
		Getenv:     func(string) string { return "" },
		Privilege:  &privilege.Prober{Runner: cmdrun.NewFake(), UID: 1000},
		DetectHost: func() report.Host { return report.Host{Hostname: "web-01", Kernel: "6.8.0", Arch: "aarch64"} },
	}
	for _, apply := range opts {
		apply(&o)
	}
	return resolve.New(o)
}

// run renders a report and returns it as text.
func run(t *testing.T, opts Options) string {
	t.Helper()
	var out strings.Builder
	opts.Stdout = &out
	if opts.Resolved == nil {
		opts.Resolved = bare(t)
	}
	code, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatalf("doctor failed: %v", err)
	}
	if code != exitcode.OK {
		t.Errorf("exit = %d, want %d: doctor describes rather than grades", code, exitcode.OK)
	}
	return out.String()
}

// under returns the lines beneath a heading, so an assertion about the container
// runtime cannot be satisfied by a word that appeared in a config path.
func under(t *testing.T, out, title string) []string {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if line != title {
			continue
		}
		var body []string
		for _, line := range lines[i+1:] {
			if !strings.HasPrefix(line, "  ") {
				return body
			}
			body = append(body, line)
		}
		return body
	}
	t.Fatalf("no %q section in:\n%s", title, out)
	return nil
}

func contains(lines []string, substr string) bool {
	for _, line := range lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func TestTheBuildIdentifiesItself(t *testing.T) {
	out := run(t, Options{Version: "2.0.0-rc1"})
	if !strings.HasPrefix(out, "fleetfix 2.0.0-rc1\n") {
		t.Errorf("output does not open with the version:\n%s", out)
	}
}

// A pasted doctor output should say where the diagnostics went, because "I saw
// no errors" means something different when they were going to a file.
func TestTheLogDestinationIsPrintedWhenThereIsOne(t *testing.T) {
	out := run(t, Options{Version: "2.0.0", LogDestination: "/var/log/fleetfix.log"})
	if !strings.Contains(out, "diagnostics: /var/log/fleetfix.log") {
		t.Errorf("no log destination in:\n%s", out)
	}

	// And nothing at all when the caller configured none, rather than a line
	// saying "(none)" that an operator would then go looking for.
	if bare := run(t, Options{Version: "2.0.0"}); strings.Contains(bare, "diagnostics:") {
		t.Errorf("a caller that said nothing about logging got a line about it:\n%s", bare)
	}
}

func TestTheHostIsDescribed(t *testing.T) {
	body := under(t, run(t, Options{}), "Host")
	for _, want := range []string{"web-01", "6.8.0", "aarch64"} {
		if !contains(body, want) {
			t.Errorf("no %q in the host section: %v", want, body)
		}
	}
}

// An empty field is printed rather than dropped: a blank distro and a missing
// one read the same to a person, and only one of them is worth a support
// question.
func TestAnUnknownFieldSaysSo(t *testing.T) {
	body := under(t, run(t, Options{}), "Host")
	if !contains(body, "distro") {
		t.Errorf("the absent distro was dropped rather than reported: %v", body)
	}
	if !contains(body, "(none)") {
		t.Errorf("no field was marked absent: %v", body)
	}
}

func TestTheTierTwoVerdictIsPrintedWithItsReason(t *testing.T) {
	body := under(t, run(t, Options{}), "Privilege")
	if !contains(body, "unavailable") {
		t.Errorf("tier 2 is not reported: %v", body)
	}
	// The reason is the whole value of the line. "You cannot escalate" and "your
	// cached credential expired" send an operator to different places.
	if !contains(body, "sudo") {
		t.Errorf("tier 2 is reported without a reason: %v", body)
	}
	if !contains(body, "1000") {
		t.Errorf("the uid is not reported: %v", body)
	}
}

// Root needs no escalation, so both halves of the section change at once. Worth
// its own test because "uid 0 (root)" and "tier 2 available" arriving together is
// the state most of the fleet's cron jobs run in.
func TestARootHostSaysSo(t *testing.T) {
	host := bare(t, func(o *resolve.Options) {
		o.Privilege = &privilege.Prober{Runner: cmdrun.NewFake(), UID: 0}
	})
	body := under(t, run(t, Options{Resolved: host}), "Privilege")

	if !contains(body, "0 (root)") {
		t.Errorf("root is not reported as root: %v", body)
	}
	if !contains(body, "available") || contains(body, "unavailable") {
		t.Errorf("root was told it cannot escalate: %v", body)
	}
}

// The runtime doctor names is the runtime the checks graded, which is this
// command's reason to exist. A host with no docker must say so here.
func TestAHostWithNoRuntimeSaysSo(t *testing.T) {
	body := under(t, run(t, Options{}), "Container runtime")
	if !contains(body, "no container runtime is installed") {
		t.Errorf("the absent runtime is not described: %v", body)
	}
	// "The daemon did not answer" would be a lie about a probe that never ran.
	if !contains(body, "nothing to ask") {
		t.Errorf("a probe that never happened was reported as a failure: %v", body)
	}
}

func TestARuntimeThatAnswersIsDescribedAsSuch(t *testing.T) {
	host := bare(t, func(o *resolve.Options) {
		o.Looker = cmdrun.NewFakeLooker("docker")
		o.Runner = cmdrun.NewFake().Stdout("Docker version 27.0.3\n", "docker", "version", "--format", "{{.Server.Version}}")
	})
	body := under(t, run(t, Options{Resolved: host}), "Container runtime")

	if !contains(body, "the daemon answered") {
		t.Errorf("a live daemon is not described: %v", body)
	}
}

// Installed but silent is the state that sends an operator here in the first
// place: the docker checks report unavailable on a host they can see containers
// on. The distinction from "nothing to ask" is the whole answer.
func TestARuntimeThatIsInstalledButSilentSaysSo(t *testing.T) {
	host := bare(t, func(o *resolve.Options) {
		o.Looker = cmdrun.NewFakeLooker("docker")
		o.Runner = cmdrun.NewFake().Fail(errors.New("permission denied while trying to connect"),
			"docker", "version", "--format", "{{.Server.Version}}")
	})
	body := under(t, run(t, Options{Resolved: host}), "Container runtime")

	if !contains(body, "the daemon did not answer") {
		t.Errorf("a silent daemon is not distinguished from an absent one: %v", body)
	}
	if contains(body, "nothing to ask") {
		t.Errorf("a probe that ran was reported as never having happened: %v", body)
	}
}

// A DOCKER_HOST with no docker to use it is usually a shell profile that
// outlived the package, and it is exactly what an operator reading "no container
// runtime" on a host they are certain has one needs to see.
func TestAnEndpointIsPrintedWhenOneIsSet(t *testing.T) {
	host := bare(t, func(o *resolve.Options) {
		o.Getenv = func(k string) string {
			if k == "DOCKER_HOST" {
				return "tcp://10.0.0.9:2375"
			}
			return ""
		}
	})
	body := under(t, run(t, Options{Resolved: host}), "Container runtime")

	if !contains(body, "tcp://10.0.0.9:2375") {
		t.Errorf("the endpoint is not reported: %v", body)
	}
}

// Every candidate, whether or not it exists, because doctor's job is to answer
// "which files are in play?" -- and a list of only the files that happen to
// exist cannot tell an operator that the path they wrote is not the path being
// read.
func TestEveryConfigCandidateIsListed(t *testing.T) {
	paths := testPaths(t)
	host := bare(t, func(o *resolve.Options) { o.Paths = paths })
	body := under(t, run(t, Options{Resolved: host}), "Configuration")

	for _, name := range resolve.Files {
		if !contains(body, name+":") {
			t.Errorf("%s is not listed: %v", name, body)
		}
	}
	if !contains(body, paths.SystemDir) {
		t.Errorf("the system layer is not listed: %v", body)
	}
	if !contains(body, paths.UserDir) {
		t.Errorf("the user layer is not listed: %v", body)
	}
	if !contains(body, "(absent)") {
		t.Errorf("an absent candidate is not marked: %v", body)
	}
}

// The file the operator actually wrote is marked in use, which is the answer to
// "is my file being read at all".
func TestAFileInUseIsMarked(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.UserDir, 0o755); err != nil {
		t.Fatal(err)
	}
	written := filepath.Join(paths.UserDir, config.ThresholdsFile)
	if err := os.WriteFile(written, []byte("mem.used_pct:\n  warn: 60\n  crit: 70\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	host := bare(t, func(o *resolve.Options) { o.Paths = paths })
	out := run(t, Options{Resolved: host})

	if !contains(under(t, out, "Configuration"), written+"  (in use)") {
		t.Errorf("the written file is not marked in use:\n%s", out)
	}
	// And the number it changed is marked too, so the listing answers "is my
	// file in effect?" and not merely "what are the numbers?".
	if !contains(under(t, out, "Thresholds"), "(overridden)") {
		t.Errorf("the override is not marked:\n%s", out)
	}
}

// A file that will not parse is the single most useful thing doctor can report,
// because the run it broke did not fail -- it fell back to defaults and graded
// the host against a policy nobody chose.
func TestAnUnparseableFileIsReported(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.UserDir, 0o755); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(paths.UserDir, config.ThresholdsFile)
	if err := os.WriteFile(broken, []byte("mem.used_pct: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	host := bare(t, func(o *resolve.Options) { o.Paths = paths })
	out := run(t, Options{Resolved: host})

	if !contains(under(t, out, "Configuration"), "unparseable") {
		t.Errorf("the broken file is not marked:\n%s", out)
	}
	if warnings := under(t, out, "Configuration warnings"); contains(warnings, "none") {
		t.Errorf("a broken file produced no warning:\n%s", out)
	}
}

func TestNoWarningsSaysNoneRatherThanNothing(t *testing.T) {
	body := under(t, run(t, Options{}), "Configuration warnings")
	if !contains(body, "none") {
		t.Errorf("a clean host got an empty section rather than an answer: %v", body)
	}
}

// The inventory comes from Spec.NeedsBins, which is the same field the runner's
// presence gate reads -- so this section is that decision made visible before
// the run rather than explained after it.
func TestTheExternalProgramsComeFromTheSpecs(t *testing.T) {
	host := bare(t, func(o *resolve.Options) { o.Looker = cmdrun.NewFakeLooker("df") })
	body := under(t, run(t, Options{
		Resolved: host,
		Registry: registry(
			t,
			spec("disk.usage", "df"),
			spec("disk.inodes", "df"),
			spec("network.ladder", "traceroute"),
			spec("logsqueeze.candidates"),
		),
	}), "External programs")

	if len(body) != 2 {
		t.Fatalf("got %d programs, want df and traceroute:\n%s", len(body), strings.Join(body, "\n"))
	}
	// Sorted by name, and each names the checks that want it -- which is what
	// turns "traceroute is missing" into "so network.ladder will not run".
	if !strings.Contains(body[0], "df") || !strings.Contains(body[0], "/usr/bin/df") {
		t.Errorf("df is not reported as installed: %q", body[0])
	}
	if !strings.Contains(body[0], "disk.inodes, disk.usage") {
		t.Errorf("df does not name its checks: %q", body[0])
	}
	if !strings.Contains(body[1], "not installed") || !strings.Contains(body[1], "network.ladder") {
		t.Errorf("the missing traceroute is not reported against its check: %q", body[1])
	}
}

// A long name must not shunt the column, because a table that loses its
// alignment on one row is harder to read than one that never had any.
func TestTheProgramColumnsFitTheLongestName(t *testing.T) {
	host := bare(t, func(o *resolve.Options) { o.Looker = cmdrun.NewFakeLooker("systemd-analyze", "ss") })
	body := under(t, run(t, Options{
		Resolved: host,
		Registry: registry(t, spec("services.boot", "systemd-analyze"), spec("network.sockets", "ss")),
	}), "External programs")

	if len(body) != 2 {
		t.Fatalf("got %d programs, want 2", len(body))
	}
	first := strings.Index(body[0], "/usr/bin/")
	second := strings.Index(body[1], "/usr/bin/")
	if first != second {
		t.Errorf("the path column is at %d and %d:\n%s", first, second, strings.Join(body, "\n"))
	}
}

func TestABuildWithNoShellingOutSaysSo(t *testing.T) {
	body := under(t, run(t, Options{Registry: registry(t, spec("logsqueeze.candidates"))}), "External programs")
	if !contains(body, "no check in this build shells out") {
		t.Errorf("an empty inventory was not explained: %v", body)
	}
}

// The gap between "registered" and "default run" is the line worth printing: a
// check that is registered and out of the default set looks exactly like a
// missing check to an operator grepping a report for it.
func TestTheWithheldChecksAreNamed(t *testing.T) {
	withheld := spec("storage.stale")
	withheld.InDefault = false
	body := under(t, run(t, Options{
		Registry: registry(t, spec("disk.usage"), spec("network.ping"), withheld),
	}), "Checks")

	if !contains(body, "3 in 3 domains") {
		t.Errorf("the registered count is wrong: %v", body)
	}
	if !contains(body, "default run     2") {
		t.Errorf("the default count is wrong: %v", body)
	}
	if !contains(body, "storage.stale") {
		t.Errorf("the withheld check is not named: %v", body)
	}
}

// Nothing to name, so no line -- rather than "only when named: " trailing into
// nothing, which reads as a rendering bug.
func TestNothingIsSaidAboutWithheldChecksWhenThereAreNone(t *testing.T) {
	body := under(t, run(t, Options{Registry: registry(t, spec("disk.usage"))}), "Checks")
	if contains(body, "only when named") {
		t.Errorf("an empty withheld list was printed anyway: %v", body)
	}
}

// A build with no collectors is a fact worth printing rather than crashing over,
// and the nil registry is what a caller that supplied none gets.
func TestABuildWithNoChecksIsDescribedRatherThanRefused(t *testing.T) {
	out := run(t, Options{Version: "2.0.0"})
	if !contains(under(t, out, "Checks"), "this build ships no checks") {
		t.Errorf("an empty build was not described:\n%s", out)
	}
}

// The one thing doctor must not do. Every fake in this file panics on Run, so a
// section that reached for a result rather than a spec fails loudly here.
func TestDoctorDescribesChecksWithoutRunningThem(t *testing.T) {
	run(t, Options{Registry: registry(t, spec("disk.usage", "df"), spec("network.ping", "ping"))})
}

// The stream is written once, at the end, so a failure does not leave the
// operator reading half a host description.
func TestAWriteFailureIsReturnedRatherThanPrinted(t *testing.T) {
	code, err := Run(t.Context(), Options{Stdout: refusingWriter{}, Resolved: bare(t)})
	if err == nil {
		t.Fatal("a failed write was reported as success")
	}
	if code != exitcode.Unknown {
		t.Errorf("exit = %d, want %d", code, exitcode.Unknown)
	}
}

// A caller that hands over nothing gets the live host, which is right for a
// caller with nobody to share a resolver with -- and wrong for the CLI, whose
// whole point is to hand the same one to both front doors. Asserted so the
// convenience default cannot quietly become the CLI's path.
func TestACallerThatSharesNothingGetsTheLiveHost(t *testing.T) {
	var out strings.Builder
	code, err := Run(t.Context(), Options{Stdout: &out, Version: "2.0.0"})
	if err != nil {
		t.Fatalf("doctor failed: %v", err)
	}
	if code != exitcode.OK {
		t.Errorf("exit = %d, want %d", code, exitcode.OK)
	}
	// The hostname rather than the arch, because Host.Arch is uname's spelling
	// (x86_64) and runtime.GOARCH is Go's (amd64) -- comparing them would fail on
	// every machine for a reason that has nothing to do with this function.
	name, err := os.Hostname()
	if err != nil {
		t.Skipf("this machine will not say what it is called: %v", err)
	}
	if !contains(under(t, out.String(), "Host"), name) {
		t.Errorf("the live host was not described:\n%s", out.String())
	}
}

// Nothing in this build produces an empty section today -- programs, checks and
// or all return at least one line. The guard is here so that a section added
// later cannot print a bare heading with nothing under it, and this is the test
// that keeps the guard honest rather than dead.
func TestAnEmptySectionPrintsNothingAtAll(t *testing.T) {
	var b strings.Builder
	section(&b, "Nothing", nil)
	if b.String() != "" {
		t.Errorf("an empty section printed %q", b.String())
	}
}

type refusingWriter struct{}

func (refusingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }
