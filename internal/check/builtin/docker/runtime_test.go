package docker

import (
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
)

func runtimeOf(t *testing.T, rt Runtime) (check.Result, *recorder) {
	t.Helper()
	return run(t, find(t, Checks(cmdrun.NewFake(), rt), RuntimeID))
}

// The summary is the resolver's sentence, not one written here. container.go builds
// it with the daemon's own version and, where one is set, the DOCKER_HOST it is
// talking to -- and a check that paraphrased it would drop the endpoint, which is
// the fact that resolves "everything works but it is about a different machine".
func TestTheRuntimeCheckReportsTheResolversOwnSentence(t *testing.T) {
	res, rec := runtimeOf(t, liveDocker())

	if res.Status != check.StatusOK {
		t.Errorf("status = %s, want ok", res.Status)
	}
	if res.Summary != "docker daemon version 27.1.1" {
		t.Errorf("summary = %q", res.Summary)
	}
	if got := rec.texts(); len(got) != 1 || got[0] != res.Summary {
		t.Errorf("steps = %v, want the same sentence once", got)
	}
}

// The one crit in the domain. An installed docker whose daemon will not answer is a
// host that cannot start the containers someone put on it, and reporting it as
// unavailable would keep it out of the exit code on every run without --strict.
func TestAnInstalledDaemonThatWillNotAnswerIsCrit(t *testing.T) {
	res, rec := runtimeOf(t, deadDocker())

	if res.Status != check.StatusCrit {
		t.Fatalf("status = %s, want crit", res.Status)
	}
	// docker's own connection error, socket path and all: it is a better sentence
	// than anything written in advance.
	if !strings.Contains(res.Summary, "unix:///var/run/docker.sock") {
		t.Errorf("summary = %q, want the socket the daemon was tried on", res.Summary)
	}
	if got := rec.texts(); len(got) != 1 {
		t.Errorf("steps = %v, want the reason once", got)
	}
}

// Not a fault. A fleet where half the machines are databases would be half red on a
// verdict that means nothing about them.
func TestNoRuntimeIsUnavailableRatherThanAFault(t *testing.T) {
	res, rec := runtimeOf(t, noDocker())

	if res.Status != check.StatusUnavailable {
		t.Errorf("status = %s, want unavailable", res.Status)
	}
	if res.Summary != "no container runtime is installed" {
		t.Errorf("summary = %q", res.Summary)
	}
	// No step: there is no runtime to narrate. The summary already said so.
	if got := rec.texts(); len(got) != 0 {
		t.Errorf("steps = %v, want none", got)
	}
}

// The series a fleet alerts on. It has to exist on a host with nothing installed
// too -- a series that disappears looks exactly like a scrape that failed.
func TestDaemonUpIsOneOrZeroAndAlwaysPresent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime Runtime
		want    float64
		kind    string
	}{
		{"answering", liveDocker(), 1, "docker"},
		{"installed but silent", deadDocker(), 0, "docker"},
		{"nothing installed", noDocker(), 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := runtimeOf(t, tc.runtime)

			if len(res.Metrics) != 1 {
				t.Fatalf("metrics = %v, want just %s", res.Metrics, DaemonUpMetric)
			}
			m := res.Metrics[0]
			if m.Name != DaemonUpMetric || m.Value != tc.want {
				t.Errorf("%s = %v, want %v", m.Name, m.Value, tc.want)
			}
			if m.Labels["runtime"] != tc.kind {
				t.Errorf("runtime label = %q, want %q", m.Labels["runtime"], tc.kind)
			}
			if m.Kind != check.Gauge {
				t.Errorf("kind = %s, want a gauge", m.Kind)
			}
		})
	}
}

// data[] carries the whole resolved runtime, endpoint included, because that is
// what `fleetfix doctor` will print and the two have to be the same answer.
func TestTheResolvedRuntimeIsCarriedInData(t *testing.T) {
	res, _ := runtimeOf(t, liveDocker())

	rt, ok := res.Data.(interface{ Found() bool })
	if !ok {
		t.Fatalf("data = %T, want the resolved container.Runtime", res.Data)
	}
	if !rt.Found() {
		t.Error("data says no runtime was found on a host where one answered")
	}
}

// The budget has to cover a daemon that is starting rather than wedged. A host
// mid-reboot answering in four seconds must not be reported as broken.
func TestTheRuntimeCheckWaitsLongEnoughForAStartingDaemon(t *testing.T) {
	spec := find(t, Checks(cmdrun.NewFake(), liveDocker()), RuntimeID).Spec()

	if spec.Budget != runtimeBudget {
		t.Errorf("budget = %s, want %s", spec.Budget, runtimeBudget)
	}
}
