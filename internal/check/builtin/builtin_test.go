package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/disk"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/docker"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/logsqueeze"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/network"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/services"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/storage"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/system"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/container"
	corenet "github.com/KingPin/FleetFix/v2/internal/core/network"
	coresystem "github.com/KingPin/FleetFix/v2/internal/core/system"
	"github.com/KingPin/FleetFix/v2/internal/hostfs"
	"github.com/KingPin/FleetFix/v2/internal/netprobe"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// TestRegistryAcceptsEverythingThisBuildShips is the reason MustRegister is the
// right call in a startup path. A duplicated id, a spec missing a title, an id
// that was retired -- each is a defect in builtin.go that this run catches, and
// the alternative to catching it here is a release where a check is silently
// absent from every host's report.
func TestRegistryAcceptsEverythingThisBuildShips(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("this build's checks do not form a valid registry: %v", r)
		}
	}()

	reg := Registry(Deps{})

	if got, want := reg.Len(), len(Checks(Deps{})); got != want {
		t.Errorf("registry holds %d checks, want the %d this build ships", got, want)
	}
	if reg.Len() == 0 {
		t.Fatal("the registry is empty; every front door would report that it found nothing to do")
	}
}

// The ids are the public surface -- they appear in checks[], in --check
// selectors, and in whatever Ansible an operator wrote against them. Naming them
// here rather than counting means dropping one from the build is a failure with
// the missing name in it.
func TestTheDiskDomainIsRegistered(t *testing.T) {
	reg := Registry(Deps{})

	for _, id := range []check.ID{disk.UsageID, disk.InodesID} {
		found := false
		for _, spec := range reg.Specs() {
			if spec.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is not registered in this build", id)
		}
	}
}

func TestTheNetworkDomainIsRegistered(t *testing.T) {
	reg := Registry(Deps{})

	registered := map[check.ID]bool{}
	for _, spec := range reg.Specs() {
		registered[spec.ID] = true
	}
	for _, id := range []check.ID{
		network.LadderID, network.InterfaceID, network.ResolverID,
		network.PingID, network.DNSID, network.HTTPSID, network.TCPID,
		network.SocketsID, network.TracerouteID,
	} {
		if !registered[id] {
			t.Errorf("%s is not registered in this build", id)
		}
	}
}

// The network domain's seam, same property as the disk domain's runner: a
// collector that built its own netprobe.New() would pass every other test here
// and still ping the internet from a unit test.
func TestTheStagedProberReachesTheNetworkChecks(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Stdout("", "ss", "-tlnpH")
	reg := Registry(Deps{Prober: &netprobe.Prober{Run: fake, Look: cmdrun.NewFakeLooker("ss")}})

	selected, err := reg.Select([]string{string(network.SocketsID)}, nil)
	if err != nil {
		t.Fatalf("selecting %s failed: %v", network.SocketsID, err)
	}
	selected[0].Run(t.Context(), check.Input{Params: map[string]string{}, Progress: check.Discard})

	if !fake.Called("ss", "-tlnpH") {
		t.Errorf("calls = %v, want the sockets collector's ss among them", fake.Calls())
	}
}

// Run and Look flow into the default prober, so setting those two is enough for
// a caller that does not want to assemble a whole fake host -- and so one
// invocation's subprocesses all go through the seam it was resolved with.
func TestTheSharedSeamsReachTheDefaultProber(t *testing.T) {
	fake := cmdrun.NewFake()
	look := cmdrun.NewFakeLooker("traceroute")

	p := Deps{Run: fake, Look: look}.prober()
	if p.Run != cmdrun.Runner(fake) {
		t.Error("the default prober did not take the supplied runner")
	}
	if p.Look != cmdrun.Looker(look) {
		t.Error("the default prober did not take the supplied looker")
	}
	// The rest is still the live host, which is what makes Run and Look on their
	// own a usable configuration rather than half of one.
	if p.Dial == nil || p.Lookup == nil || p.ReadFile == nil {
		t.Error("the default prober is missing a seam nobody overrode")
	}
}

// Nil probes means the defaults, and the defaults have targets. The zero Probes
// looks valid and has none, which would skip every network probe on a host whose
// caller simply did not set the field.
func TestNilProbesMeansTheShippedTargets(t *testing.T) {
	if got := (Deps{}).probes(); len(got.Ping.Targets) == 0 || got.Ladder.InternetTarget == "" {
		t.Fatalf("Deps{}.probes() = %+v", got)
	}

	// And a supplied probes.yml reaches the specs. Asserted through the
	// traceroute param default because that is the one configured value visible
	// without running anything -- if the config did not arrive, this is the
	// shipped 8.8.8.8.
	custom := corenet.DefaultProbes()
	custom.Ladder.InternetTarget = "9.9.9.9"
	for _, c := range Checks(Deps{Probes: &custom}) {
		if c.Spec().ID != network.TracerouteID {
			continue
		}
		if got := c.Spec().Params[0].Default; got != "9.9.9.9" {
			t.Errorf("traceroute defaults to %q; the resolved probes.yml did not reach the checks", got)
		}
	}
}

// A nil Deps.Run is the documented "give me the real one", and it has to work:
// `check --list` and doctor both want to know what this build can check without
// having any interest in running a subprocess.
func TestANilRunnerMeansTheRealOne(t *testing.T) {
	if got := (Deps{}).runner(); got == nil {
		t.Fatal("Deps{}.runner() is nil; the collectors would panic on their first call")
	}
	// Same for Look, which the default container resolver searches PATH with. A
	// nil one there would panic inside the memo, on the first host that has docker.
	if got := (Deps{}).looker(); got == nil {
		t.Fatal("Deps{}.looker() is nil; detecting the container runtime would panic")
	}

	checks := Checks(Deps{})
	if len(checks) == 0 {
		t.Fatal("Checks(Deps{}) returned nothing")
	}
	for _, c := range checks {
		if err := c.Spec().Validate(); err != nil {
			t.Errorf("%s: %v", c.Spec().ID, err)
		}
	}
}

// TestTheStagedRunnerReachesTheChecks is the property the Deps struct exists for.
// A collector that closed over cmdrun.New() itself would pass every test above and
// still shell out to the real df in a unit test.
func TestTheStagedRunnerReachesTheChecks(t *testing.T) {
	fake := cmdrun.NewFake()
	reg := Registry(Deps{Run: fake})

	selected, err := reg.Select([]string{string(disk.UsageID)}, nil)
	if err != nil {
		t.Fatalf("selecting %s failed: %v", disk.UsageID, err)
	}
	if len(selected) != 1 {
		t.Fatalf("selected %d checks, want 1", len(selected))
	}

	// The staged runner answers nothing, so the check reports a failure rather
	// than a reading. What is under test is which runner it reached, not the
	// verdict -- internal/check/builtin/disk owns the verdict.
	selected[0].Run(t.Context(), check.Input{
		Params:     map[string]string{},
		Progress:   check.Discard,
		Thresholds: threshold.Defaults(),
	})
	if len(fake.Calls()) == 0 {
		t.Fatal("the staged runner was never called; the checks hold their own cmdrun.New()")
	}
	if calls := strings.Join(fake.Calls(), " "); !strings.Contains(calls, "df") {
		t.Errorf("calls = %q, want the disk collector's df among them", calls)
	}
}

func TestTheDockerDomainIsRegistered(t *testing.T) {
	reg := Registry(Deps{})

	registered := map[check.ID]bool{}
	for _, spec := range reg.Specs() {
		registered[spec.ID] = true
	}
	for _, id := range []check.ID{docker.RuntimeID, docker.ContainersID, docker.DiskID} {
		if !registered[id] {
			t.Errorf("%s is not registered in this build", id)
		}
	}
}

// The runtime the front door resolved is the runtime the checks grade, which is
// the whole point of resolve.Resolved.Container being passed in rather than each
// domain finding its own: doctor describes one answer and the report is graded by
// the same one.
func TestTheSuppliedRuntimeReachesTheDockerChecks(t *testing.T) {
	staged := container.Runtime{
		Kind: container.Podman, Bin: "podman", Available: true,
		Reason: "podman daemon version 5.1.0",
	}
	reg := Registry(Deps{Container: func(context.Context) container.Runtime { return staged }})

	selected, err := reg.Select([]string{string(docker.RuntimeID)}, nil)
	if err != nil {
		t.Fatalf("selecting %s failed: %v", docker.RuntimeID, err)
	}
	res := selected[0].Run(t.Context(), check.Input{Params: map[string]string{}, Progress: check.Discard})

	if res.Summary != staged.Reason {
		t.Errorf("summary = %q, want the resolved runtime's own reason %q", res.Summary, staged.Reason)
	}
}

// Nil Container means detect-and-probe over this Deps' own seams -- so the
// subprocess it costs goes through the staged runner, and a test never asks the
// developer's own docker anything.
func TestTheDefaultRuntimeIsProbedOnceThroughTheStagedSeams(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Stdout("27.1.1\n", "docker", "version", "--format", "{{.Server.Version}}")
	fake.Stdout("", "docker", "ps", "-a", "--format", "{{json .}}")
	fake.Stdout("", "docker", "system", "df", "--format", "{{json .}}")

	checks := Checks(Deps{Run: fake, Look: cmdrun.NewFakeLooker("docker")})
	ran := 0
	for _, c := range checks {
		if c.Spec().Domain != "docker" {
			continue
		}
		ran++
		c.Run(t.Context(), check.Input{Params: map[string]string{}, Progress: check.Discard})
	}
	if ran != 3 {
		t.Fatalf("ran %d docker checks, want 3", ran)
	}

	// One probe for three checks. Memoised per Checks() call, so the cost of the
	// domain on a host that has docker is one `docker version`, not one per check.
	probes := 0
	for _, call := range fake.Calls() {
		if strings.Contains(call, "version") {
			probes++
		}
	}
	if probes != 1 {
		t.Errorf("the daemon was probed %d times, want 1; calls = %v", probes, fake.Calls())
	}
}

// And an invocation that runs no docker check costs nothing at all -- not the
// probe, not even the PATH lookup. Half a fleet runs no containers, and `--list`
// and doctor's inventory both build the full set without intending to touch one.
func TestBuildingTheRegistryProbesNoDaemon(t *testing.T) {
	fake := cmdrun.NewFake()
	var looks atomic.Int64
	look := lookCounter{Looker: cmdrun.NewFakeLooker("docker"), n: &looks}

	if got := len(Registry(Deps{Run: fake, Look: look}).Specs()); got == 0 {
		t.Fatal("the registry is empty")
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("calls = %v; assembling the registry asked the host something", fake.Calls())
	}
	if got := looks.Load(); got != 0 {
		t.Errorf("PATH was searched %d times to assemble a registry nobody ran", got)
	}
}

func TestTheServicesDomainIsRegistered(t *testing.T) {
	reg := Registry(Deps{})

	registered := map[check.ID]bool{}
	for _, spec := range reg.Specs() {
		registered[spec.ID] = true
	}
	for _, id := range []check.ID{services.FailedID, services.BootID} {
		if !registered[id] {
			t.Errorf("%s is not registered in this build", id)
		}
	}
}

// The domain takes nothing but the runner, so the staged one has to reach it --
// a collector holding its own cmdrun.New() would pass the registration test
// above and still ask the developer's own systemd what has failed.
func TestTheStagedRunnerReachesTheServicesChecks(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Stdout("myapp.service loaded failed failed My Application\n",
		"systemctl", "list-units", "--state=failed", "--no-legend", "--no-pager", "--plain")
	fake.Stdout("User=appuser\n", "systemctl", "show", "-p", "User", "myapp.service")
	reg := Registry(Deps{Run: fake})

	selected, err := reg.Select([]string{string(services.FailedID)}, nil)
	if err != nil {
		t.Fatalf("selecting %s failed: %v", services.FailedID, err)
	}
	res := selected[0].Run(t.Context(), check.Input{
		Params:     map[string]string{},
		Progress:   check.Discard,
		Thresholds: threshold.Defaults(),
	})

	if want := "1 failed unit: myapp.service"; res.Summary != want {
		t.Errorf("summary = %q, want %q -- the staged runner did not reach the check", res.Summary, want)
	}
}

func TestTheSystemDomainIsRegistered(t *testing.T) {
	reg := Registry(Deps{})

	registered := map[check.ID]bool{}
	for _, spec := range reg.Specs() {
		registered[spec.ID] = true
	}
	for _, id := range []check.ID{
		system.LoadID, system.MemoryID, system.ThermalID, system.UpdatesID, system.UptimeID,
	} {
		if !registered[id] {
			t.Errorf("%s is not registered in this build", id)
		}
	}
}

// The staged /proc reaches the checks, which is the property that keeps a unit
// test off the developer's own load average. A collector holding its own
// hostfs.New() would pass every registration test above and still read this
// machine.
func TestTheStagedSystemSourceReachesTheChecks(t *testing.T) {
	staged := system.Source{
		Host: hostfs.Host{
			Proc: fstest.MapFS{
				coresystem.ProcLoadavg: &fstest.MapFile{Data: []byte("0.50 0.40 0.30 1/234 5678\n")},
			},
			Sys: fstest.MapFS{},
		},
		Run:  cmdrun.NewFake(),
		CPUs: func() int { return 2 },
	}
	reg := Registry(Deps{System: &staged})

	selected, err := reg.Select([]string{string(system.LoadID)}, nil)
	if err != nil {
		t.Fatalf("selecting %s failed: %v", system.LoadID, err)
	}
	res := selected[0].Run(t.Context(), check.Input{
		Params:     map[string]string{},
		Progress:   check.Discard,
		Thresholds: threshold.Defaults(),
	})

	if want := "load 0.50 0.40 0.30 across 2 CPUs, 0.25 per CPU"; res.Summary != want {
		t.Errorf("summary = %q, want %q -- the staged /proc did not reach the check", res.Summary, want)
	}
}

// Nil System means the live host, and the one check in that domain that may
// shell out has to do it through the caller's runner rather than a second one
// of its own.
func TestTheSharedRunnerReachesTheDefaultSystemSource(t *testing.T) {
	fake := cmdrun.NewFake()

	src := Deps{Run: fake}.system()
	if src.Run != cmdrun.Runner(fake) {
		t.Error("the default system source did not take the supplied runner")
	}
	// And the rest is still the live host, which is what makes Run on its own a
	// usable configuration rather than half of one. A nil seam here is a panic on
	// the first host that reaches it.
	if src.Host.Proc == nil || src.Host.Sys == nil || src.CPUs == nil || src.ReadFile == nil {
		t.Error("the default system source is missing a seam nobody overrode")
	}
	if src.NotifierPath != system.DefaultNotifierPath {
		t.Errorf("the default system source reads the notifier from %q", src.NotifierPath)
	}
}

func TestTheLogsqueezeDomainIsRegistered(t *testing.T) {
	reg := Registry(Deps{})

	registered := map[check.ID]bool{}
	for _, spec := range reg.Specs() {
		registered[spec.ID] = true
	}
	if !registered[logsqueeze.CandidatesID] {
		t.Errorf("%s is not registered in this build", logsqueeze.CandidatesID)
	}
}

// The staged tree reaches the check, which is the property that keeps a unit
// test off the developer's own /var/log. A collector holding its own
// os.DirFS("/var/log") would pass the registration test above and still walk
// this machine.
func TestTheStagedLogTreeReachesTheCheck(t *testing.T) {
	staged := logsqueeze.Source{
		Root:     "/var/log",
		FS:       fstest.MapFS{"nginx/access.log": &fstest.MapFile{Data: make([]byte, 3000)}},
		MinBytes: 1000,
	}
	reg := Registry(Deps{Logs: &staged})

	selected, err := reg.Select([]string{string(logsqueeze.CandidatesID)}, nil)
	if err != nil {
		t.Fatalf("selecting %s failed: %v", logsqueeze.CandidatesID, err)
	}
	res := selected[0].Run(t.Context(), check.Input{
		Params:     map[string]string{},
		Progress:   check.Discard,
		Thresholds: threshold.Defaults(),
	})

	want := "1 uncompressed log holding 2.9 KB, largest is /var/log/nginx/access.log at 2.9 KB"
	if res.Summary != want {
		t.Errorf("summary = %q, want %q -- the staged tree did not reach the check", res.Summary, want)
	}
}

// Nil Logs is the live host at v1's floor, rather than the zero Source's
// filesystem-less walk.
func TestTheDefaultLogSourceIsTheLiveVarLog(t *testing.T) {
	src := Deps{}.logs()

	if src.Root != logsqueeze.DefaultRoot {
		t.Errorf("root = %q, want %q", src.Root, logsqueeze.DefaultRoot)
	}
	if src.MinBytes != logsqueeze.DefaultMinBytes {
		t.Errorf("min = %d, want %d", src.MinBytes, logsqueeze.DefaultMinBytes)
	}
}

func TestTheStorageDomainIsRegistered(t *testing.T) {
	reg := Registry(Deps{})

	registered := map[check.ID]bool{}
	for _, spec := range reg.Specs() {
		registered[spec.ID] = true
	}
	for _, id := range []check.ID{storage.StaleID, storage.EnvID} {
		if !registered[id] {
			t.Errorf("%s is not registered in this build", id)
		}
	}
}

// Registered but not run by a bare `fleetfix check`. Both need a path nobody can
// guess, and the whole argument for making them parameters rather than config is
// that the default run leaves them alone -- so the default set is where that has
// to be asserted, not in the domain's own spec test.
func TestTheStorageChecksAreOutOfTheDefaultSet(t *testing.T) {
	reg := Registry(Deps{})

	byDefault, err := reg.Select(nil, nil)
	if err != nil {
		t.Fatalf("selecting the default set failed: %v", err)
	}
	for _, c := range byDefault {
		if id := c.Spec().ID; id == storage.StaleID || id == storage.EnvID {
			t.Errorf("%s runs by default, but it has no path to run against", id)
		}
	}

	// Still reachable when named, which is the other half of the contract.
	named, err := reg.Select([]string{"storage"}, nil)
	if err != nil {
		t.Fatalf("selecting the storage domain failed: %v", err)
	}
	if len(named) != 2 {
		t.Errorf("--check storage selects %d checks, want 2", len(named))
	}
}

// The operator's parameter reaches the registered check. A collector that read a
// root from somewhere else -- a package variable, the invoking user's home --
// would pass the registration test above and scan the wrong machine.
func TestTheOperatorsRootReachesTheStaleCheck(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-400 * 24 * time.Hour)
	dump := filepath.Join(root, "backup.sql")
	if err := os.WriteFile(dump, make([]byte, 2048), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(dump, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	reg := Registry(Deps{})
	selected, err := reg.Select([]string{string(storage.StaleID)}, nil)
	if err != nil {
		t.Fatalf("selecting %s failed: %v", storage.StaleID, err)
	}
	res := selected[0].Run(t.Context(), check.Input{
		Params: map[string]string{
			storage.RootParam:          root,
			storage.OlderThanDaysParam: "30",
		},
		Progress:   check.Discard,
		Thresholds: threshold.Defaults(),
	})

	want := "1 stale file holding 2.0 KB under " + root + ", largest is " + dump + " at 2.0 KB"
	if res.Summary != want {
		t.Errorf("summary = %q, want %q -- the parameter did not reach the check", res.Summary, want)
	}
}

type lookCounter struct {
	cmdrun.Looker
	n *atomic.Int64
}

func (l lookCounter) Look(bin string) (string, error) {
	l.n.Add(1)
	return l.Looker.Look(bin)
}
