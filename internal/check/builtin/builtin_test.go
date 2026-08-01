package builtin

import (
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/disk"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/network"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	corenet "github.com/KingPin/FleetFix/v2/internal/core/network"
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
