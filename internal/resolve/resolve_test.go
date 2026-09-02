package resolve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/audit"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/config"
	"github.com/KingPin/FleetFix/v2/internal/container"
	"github.com/KingPin/FleetFix/v2/internal/identity"
	"github.com/KingPin/FleetFix/v2/internal/privilege"
	"github.com/KingPin/FleetFix/v2/internal/report"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

func testPaths(t *testing.T) *config.Paths {
	t.Helper()
	root := t.TempDir()
	return &config.Paths{
		SystemDir: filepath.Join(root, "etc"),
		UserDir:   filepath.Join(root, "user"),
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

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func noEnv(string) string { return "" }

// A host that answers nothing: no config, no docker, no sudo, an unreadable
// /proc. Every test starts here and adds only the fact it is about.
func bare(t *testing.T) Options {
	t.Helper()
	trail := t.TempDir()
	return Options{
		Paths:      testPaths(t),
		Runner:     cmdrun.NewFake(),
		Looker:     cmdrun.NewFakeLooker(),
		Getenv:     noEnv,
		Privilege:  &privilege.Prober{Runner: cmdrun.NewFake(), UID: 1000},
		DetectHost: func() report.Host { return report.Host{} },
		// Into the test's own directory. The default answers /var/log for a root
		// process, and the CI smoke matrix runs a root leg.
		AuditPath: func() (string, error) { return filepath.Join(trail, "audit.log"), nil },
	}
}

// The whole reason this package exists. Two resolvers is how doctor reports a
// policy the checks did not grade by, so the set doctor lists and the set a
// check grades with have to be the same object, resolved once from the same
// files.
func TestOneSetOfFilesAnswersBothFrontDoors(t *testing.T) {
	opts := bare(t)
	writeYAML(t, opts.Paths.SystemDir, config.ThresholdsFile, "disk.used_pct:\n  warn: 70\n")
	writeYAML(t, opts.Paths.UserDir, config.ThresholdsFile, "disk.used_pct:\n  crit: 81\n")

	r := New(opts)

	rule, ok := r.Thresholds.Get(threshold.DiskUsedPct)
	if !ok {
		t.Fatal("the shipped policy lost a rule the files only amended")
	}
	if rule.Warn != 70 || rule.Crit != 81 {
		t.Errorf("disk = warn %g crit %g, want the merge of both layers", rule.Warn, rule.Crit)
	}

	// And what doctor would print names both of the files that produced it.
	listing := strings.Join(r.DescribeConfig(), "\n")
	for _, dir := range []string{opts.Paths.SystemDir, opts.Paths.UserDir} {
		want := filepath.Join(dir, config.ThresholdsFile)
		if !strings.Contains(listing, want+"  (in use)") {
			t.Errorf("doctor's listing does not report %s as in use:\n%s", want, listing)
		}
	}
}

// Doctor's job is to answer "which files are in play?", and a list of only the
// files that happened to exist cannot tell an operator that the path they meant
// to write is not the path being read.
func TestEveryCandidateIsListedIncludingTheAbsentOnes(t *testing.T) {
	opts := bare(t)
	r := New(opts)

	lines := r.DescribeConfig()
	for _, name := range Files {
		if !strings.Contains(strings.Join(lines, "\n"), name+":") {
			t.Errorf("%s is missing from doctor's listing", name)
		}
		for _, dir := range []string{opts.Paths.SystemDir, opts.Paths.UserDir} {
			want := filepath.Join(dir, name) + "  (absent)"
			if !strings.Contains(strings.Join(lines, "\n"), want) {
				t.Errorf("the listing does not report %s", want)
			}
		}
	}
}

// config_warnings[] is compared between consecutive runs by the byte-stability
// criterion, so its order cannot come from a map.
func TestWarningsComeOutInFileOrderEveryTime(t *testing.T) {
	opts := bare(t)
	for _, name := range Files {
		writeYAML(t, opts.Paths.UserDir, name, "\tthis is not yaml\n")
	}

	first := New(opts).Warnings
	if len(first) != len(Files) {
		t.Fatalf("warnings = %v, want one per unparseable file", first)
	}
	for i, name := range Files {
		if !strings.Contains(first[i], name) {
			t.Errorf("warning %d is %q, want the one for %s", i, first[i], name)
		}
	}

	for range 5 {
		got := New(opts).Warnings
		if strings.Join(got, "\n") != strings.Join(first, "\n") {
			t.Fatalf("a second run reordered the warnings:\n%v\n%v", first, got)
		}
	}
}

// Never nil: the report forbids nil slices, and a config_warnings that marshals
// to null rather than [] breaks a consumer writing .config_warnings[].
func TestAHostWithNoConfigWarnsAboutNothingRatherThanNull(t *testing.T) {
	got := New(bare(t)).Warnings
	if got == nil {
		t.Fatal("Warnings is nil; it marshals to null")
	}
	if len(got) != 0 {
		t.Errorf("warnings = %v on a host with no config files", got)
	}
}

// Warnings from the two files that are read through their own accessors have to
// arrive as well: those readers know what the file means, so they are the only
// ones that can complain about a bound that is not a number or a principal that
// is not a string.
func TestTheAccessorsWarningsAreCarriedToo(t *testing.T) {
	opts := bare(t)
	writeYAML(t, opts.Paths.UserDir, config.ThresholdsFile, "disk.used_pct:\n  warn: high\n")
	writeYAML(t, opts.Paths.UserDir, config.IdentityFile, "principals: not-a-mapping\n")

	got := New(opts).Warnings
	if len(got) != 2 {
		t.Fatalf("warnings = %v, want one from each file", got)
	}
	if !strings.Contains(got[0], config.IdentityFile) {
		t.Errorf("first warning is %q, want the identity one first", got[0])
	}
	if !strings.Contains(got[1], threshold.DiskUsedPct) {
		t.Errorf("second warning is %q, want the one naming the rule", got[1])
	}
}

// Each block comes from a different source, and all of them are small structs of
// strings, so a crossed wire compiles and reports the source IP as the principal
// on every host at once.
func TestEachBlockComesFromItsOwnSource(t *testing.T) {
	opts := bare(t)
	opts.Getenv = env(map[string]string{
		"SUDO_USER":             "alice",
		"USER":                  "root",
		"SSH_CONNECTION":        "203.0.113.7 54321 198.51.100.2 22",
		container.DockerHostEnv: "unix:///run/user/1000/docker.sock",
	})
	opts.DetectHost = func() report.Host {
		return report.Host{Hostname: "web-01", OS: "linux", Arch: "x86_64", Kernel: "6.8.0"}
	}
	opts.Looker = cmdrun.NewFakeLooker("docker")
	writeYAML(t, opts.Paths.UserDir, config.IdentityFile, "principals:\n  alice: alice@corp.example\n")

	r := New(opts)

	wantOperator := identity.Operator{
		UnixUser:      "alice",
		AuthPrincipal: "alice@corp.example",
		SourceIP:      "203.0.113.7",
	}
	if r.Operator != wantOperator {
		t.Errorf("operator\n got %+v\nwant %+v", r.Operator, wantOperator)
	}
	if r.Host.Hostname != "web-01" || r.Host.Kernel != "6.8.0" {
		t.Errorf("host = %+v", r.Host)
	}
	if got := r.Runtime(); got.Kind != container.Docker {
		t.Errorf("runtime = %+v, want docker", got)
	}
	if got := r.Runtime().Endpoint; got != "unix:///run/user/1000/docker.sock" {
		t.Errorf("endpoint = %q; DOCKER_HOST did not reach the resolver", got)
	}
}

// The identity.yml table is read here, not in three collectors, which is what
// stops the audit trail and the report disagreeing about who is running.
func TestThePrincipalsTableReachesTheOperator(t *testing.T) {
	opts := bare(t)
	opts.Getenv = env(map[string]string{"USER": "bob"})
	writeYAML(t, opts.Paths.SystemDir, config.IdentityFile,
		"principals:\n  alice: alice@corp.example\n  bob: bob@corp.example\n")

	if got := New(opts).Operator.AuthPrincipal; got != "bob@corp.example" {
		t.Errorf("auth_principal = %q, want the entry for this login", got)
	}
}

// New runs on every invocation, including `fleetfix --version`. A subprocess
// here is a cost every one of them pays for a question it did not ask -- and on
// a fleet, a sudo entry in auth.log for each.
func TestNewShellsOutForNothing(t *testing.T) {
	opts := bare(t)
	opts.Looker = cmdrun.NewFakeLooker("docker")
	runner := cmdrun.NewFake()
	opts.Runner = runner

	r := New(opts)

	if calls := runner.Calls(); len(calls) != 0 {
		t.Errorf("New ran %v", calls)
	}
	if r.Runtime().Available {
		t.Error("Available is set without anything having asked the daemon")
	}
}

// One question, one subprocess, however many callers. A report assembled by
// several collectors must not produce several `docker version` calls, and on a
// wedged socket must not produce several two-second waits.
func TestConcurrentCallersProduceOneProbe(t *testing.T) {
	opts := bare(t)
	opts.Looker = cmdrun.NewFakeLooker("docker")
	runner := cmdrun.NewFake().Stdout("27.3.1\n", "docker", "version", "--format", "{{.Server.Version}}")
	opts.Runner = runner

	r := New(opts)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := r.Container(t.Context()); !got.Available {
				t.Errorf("Container() = %+v, want available", got)
			}
		}()
	}
	wg.Wait()

	if calls := runner.Calls(); len(calls) != 1 {
		t.Errorf("asked the daemon %d times: %v", len(calls), calls)
	}
}

// Memoised for the run, not for a window. A daemon that comes up mid-run must
// not make one check in a report say unavailable and the next say ok, because a
// reader of that document cannot tell that from a flapping daemon.
func TestTheContainerAnswerDoesNotChangeMidRun(t *testing.T) {
	opts := bare(t)
	opts.Looker = cmdrun.NewFakeLooker("docker")
	runner := cmdrun.NewFake().Exit(
		1, "", "Cannot connect to the Docker daemon.", "docker", "version", "--format", "{{.Server.Version}}",
	)
	opts.Runner = runner

	r := New(opts)
	first := r.Container(t.Context())
	if first.Available {
		t.Fatal("a refusing daemon reported available")
	}

	// The daemon comes up. The answer must not.
	runner.Stdout("27.3.1\n", "docker", "version", "--format", "{{.Server.Version}}")
	if second := r.Container(t.Context()); second != first {
		t.Errorf("the answer changed mid-run:\n got %+v\nwas %+v", second, first)
	}
}

// A host with no runtime is a supported host, and probing it must not be an
// error or a subprocess.
func TestProbingAHostWithNoRuntime(t *testing.T) {
	opts := bare(t)
	runner := cmdrun.NewFake()
	opts.Runner = runner

	r := New(opts)
	if got := r.Container(t.Context()); got.Found() || got.Available {
		t.Errorf("Container() = %+v on a host with no runtime", got)
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Errorf("ran %v with nothing to run", calls)
	}
}

// Meta is the envelope's host-facing half, and the reason there is no mapping
// function between identity.Operator and report.Operator: the struct conversion
// is compiler-checked on names, order and types.
func TestMetaCarriesEveryBlockAndNothingElse(t *testing.T) {
	opts := bare(t)
	opts.Getenv = env(map[string]string{"USER": "alice"})
	opts.DetectHost = func() report.Host { return report.Host{Hostname: "web-01", OS: "linux"} }
	opts.Privilege = &privilege.Prober{Runner: cmdrun.NewFake(), UID: 0}
	writeYAML(t, opts.Paths.UserDir, config.ThresholdsFile, "disk.used_pct:\n  warn: high\n")

	meta := New(opts).Meta(t.Context())

	if meta.Host.Hostname != "web-01" {
		t.Errorf("host = %+v", meta.Host)
	}
	if meta.Operator.UnixUser != "alice" {
		t.Errorf("operator = %+v", meta.Operator)
	}
	if !meta.Privilege.IsRoot || !meta.Privilege.CanTier2 {
		t.Errorf("privilege = %+v, want root able to act", meta.Privilege)
	}
	if len(meta.ConfigWarnings) != 1 {
		t.Errorf("config_warnings = %v, want the threshold complaint", meta.ConfigWarnings)
	}
	// The invocation's own facts are the caller's: this package is shared by a
	// check run, a doctor run and later an agent tick, and none of them agree
	// about when the run started.
	if meta.Version != "" || meta.Duration != 0 || !meta.GeneratedAt.IsZero() {
		t.Errorf("Meta invented an invocation fact: %+v", meta)
	}
}

// Every consumer reads its own file out of the one read, so a collector cannot
// re-open probes.yml and get a different answer than the report was built from.
func TestEveryFileIsAvailableToItsConsumer(t *testing.T) {
	opts := bare(t)
	writeYAML(t, opts.Paths.UserDir, config.ProbesFile, "ping:\n  count: 10\n")

	r := New(opts)
	probes := r.Config(config.ProbesFile)
	ping, ok := probes.Values["ping"].(map[string]any)
	if !ok {
		t.Fatalf("probes.yml did not reach the resolver: %+v", probes.Values)
	}
	// Rendered rather than compared to a Go literal: how a YAML scalar is typed
	// is internal/config's contract with PyYAML, and asserting it here would
	// duplicate that test and break on a change this package does not care about.
	if got := config.PyStr(ping["count"]); got != "10" {
		t.Errorf("ping.count = %s, want 10", got)
	}
	if got := probes.Effective(); len(got) != 1 {
		t.Errorf("Effective() = %v, want the one file that exists", got)
	}
}

// A typo in a file name reads as an empty file, not as a nil map a caller
// dereferences three frames later.
func TestAnUnknownFileIsEmptyAndNotNil(t *testing.T) {
	got := New(bare(t)).Config("nonesuch.yml")
	if got.Values == nil {
		t.Fatal("Values is nil")
	}
	if len(got.Values) != 0 || len(got.Sources) != 0 {
		t.Errorf("Config(unknown) = %+v", got)
	}
}

// The zero Options resolves the live host: that is what the front doors pass.
// Nothing to compare it against, so this asserts the contract -- it returns, it
// fills its seams, and it does not shell out doing it.
func TestTheZeroOptionsResolvesThisHost(t *testing.T) {
	r := New(Options{})

	if r.Paths.UserDir == "" || r.Paths.SystemDir == "" {
		t.Errorf("paths = %+v", r.Paths)
	}
	if r.Runner == nil || r.Looker == nil || r.Privilege == nil {
		t.Error("a seam was left nil, so the first caller through it panics")
	}
	if r.Warnings == nil || r.Configs == nil {
		t.Error("Warnings or Configs is nil")
	}
	if len(r.Thresholds) == 0 {
		t.Error("the shipped grading policy is empty")
	}
	if r.Host.OS != "linux" {
		t.Errorf("os = %q on a Linux-only tool", r.Host.OS)
	}
	for _, name := range Files {
		if len(r.Config(name).Sources) != 2 {
			t.Errorf("%s was not looked for in both layers", name)
		}
	}
}

// The identity the report envelope names and the identity in the trail have to
// be one identity. Two resolutions is how a record says "unknown" on a host
// whose report says "bob", and both would be telling the truth about what they
// read.
func TestTheTrailIsWrittenAsTheSameOperatorTheReportNames(t *testing.T) {
	opts := bare(t)
	opts.Getenv = env(map[string]string{"USER": "bob", "SSH_CONNECTION": "10.0.0.9 51234 10.0.0.1 22"})
	writeYAML(t, opts.Paths.SystemDir, config.IdentityFile, "principals:\n  bob: bob@corp.example\n")

	r := New(opts)
	w, err := r.Audit()
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if err := w.Event("fleetfix.launch", nil); err != nil {
		t.Fatalf("Event: %v", err)
	}

	line := readTrail(t, w.Path())
	for _, want := range []string{
		`"unix_user": "bob"`,
		`"auth_principal": "bob@corp.example"`,
		`"source_ip": "10.0.0.9"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the record does not carry %s:\n%s", want, line)
		}
	}
	if op := r.Meta(t.Context()).Operator; op.UnixUser != "bob" || op.AuthPrincipal != "bob@corp.example" {
		t.Errorf("the envelope names a different operator: %+v", op)
	}
}

// One writer per process. The session id joins a launch to the exit that
// followed it, and the sequence orders records a millisecond stamp cannot --
// two writers would issue two of each and the trail would read as two runs.
func TestConcurrentCallersShareOneWriter(t *testing.T) {
	r := New(bare(t))

	const n = 20
	var wg sync.WaitGroup
	got := make([]*audit.Writer, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, err := r.Audit()
			if err != nil {
				t.Errorf("Audit: %v", err)
				return
			}
			got[i] = w
		}()
	}
	wg.Wait()

	for i, w := range got {
		if w != got[0] {
			t.Fatalf("caller %d got a different writer", i)
		}
	}
}

// Opening the trail creates the file, so a read-only front door must not do it.
// An empty audit log on every host in a fleet is a trail that says nothing and
// looks like it should.
func TestNewDoesNotOpenTheTrail(t *testing.T) {
	opts := bare(t)
	var asked int
	inner := opts.AuditPath
	opts.AuditPath = func() (string, error) { asked++; return inner() }

	r := New(opts)
	if asked != 0 {
		t.Fatal("New opened the audit trail; check --json and doctor would leave one on every host")
	}

	if _, err := r.Audit(); err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if asked != 1 {
		t.Errorf("the path was resolved %d times, want once", asked)
	}
}

// A trail that will not open is the caller's problem to act on: a destructive
// action has to be refused, which is the only thing that makes the local file
// authoritative rather than aspirational.
func TestAnUnopenableTrailIsReportedToTheCaller(t *testing.T) {
	opts := bare(t)
	opts.AuditPath = func() (string, error) {
		return filepath.Join(t.TempDir(), "no-such-dir", "audit.log"), nil
	}

	if _, err := New(opts).Audit(); err == nil {
		t.Fatal("Audit succeeded on a path with no parent directory")
	}
}

// The fallback reason is doctor's to report, not a failure: the path beside it
// is usable either way, and "the trail is under XDG_STATE_HOME because /var/log
// is not writable by this user" is the answer to a question an operator asks
// after finding an empty file.
func TestTheFallbackReasonIsKeptForDoctor(t *testing.T) {
	opts := bare(t)
	path := filepath.Join(t.TempDir(), "audit.log")
	opts.AuditPath = func() (string, error) {
		return path, errors.New("/var/log/fleetfix-audit.log: permission denied")
	}

	r := New(opts)
	if got := r.AuditFallback(); got != "" {
		t.Errorf("a reason was reported before anything opened the trail: %q", got)
	}
	w, err := r.Audit()
	if err != nil {
		t.Fatalf("Audit refused a usable fallback path: %v", err)
	}
	if w.Path() != path {
		t.Errorf("writing to %q, want the fallback %q", w.Path(), path)
	}
	if !strings.Contains(r.AuditFallback(), "permission denied") {
		t.Errorf("the reason was lost: %q", r.AuditFallback())
	}
}

func readTrail(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	return string(data)
}
