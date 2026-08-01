package container

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
)

// env turns a table's environment into the lookup Detect takes. A missing key is
// the empty string, which is what os.Getenv reports and is the case most hosts
// are in.
func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func noEnv(string) string { return "" }

// The ordinary host: docker on PATH, no DOCKER_HOST.
func TestDetectFindsDocker(t *testing.T) {
	got := Detect(cmdrun.NewFakeLooker("docker"), noEnv)
	want := Runtime{Kind: Docker, Bin: "docker", Reason: "docker is installed"}
	if got != want {
		t.Errorf("Detect()\n got %+v\nwant %+v", got, want)
	}
	if !got.Found() {
		t.Error("Found() = false with docker on PATH")
	}
}

// A great many machines in a fleet run no containers at all. That is a supported
// host, not an error, and the zero Runtime says so.
func TestDetectFindsNothing(t *testing.T) {
	got := Detect(cmdrun.NewFakeLooker("systemctl", "smartctl"), noEnv)
	if got.Found() {
		t.Errorf("Found() = true on a host with no runtime: %+v", got)
	}
	if got.Kind != None || got.Bin != "" {
		t.Errorf("Detect() = %+v, want an empty kind and bin", got)
	}
	if got.Reason != "no container runtime is installed" {
		t.Errorf("reason = %q", got.Reason)
	}
}

// Detect finding the CLI says nothing about whether a daemon will answer it, and
// a Runtime that claimed otherwise would have every container check reporting ok
// against a stopped daemon.
func TestDetectNeverClaimsAvailable(t *testing.T) {
	if got := Detect(cmdrun.NewFakeLooker("docker"), noEnv); got.Available {
		t.Error("Detect set Available; only Probe may do that")
	}
}

// Rootless docker is reachable only through this variable: the daemon listens
// under the user's runtime directory and nothing finds it otherwise. Carrying it
// is what lets the operator see which socket the answer came from.
func TestDockerHostIsCarriedAndNamed(t *testing.T) {
	const sock = "unix:///run/user/1000/docker.sock"
	got := Detect(cmdrun.NewFakeLooker("docker"), env(map[string]string{DockerHostEnv: sock}))
	if got.Endpoint != sock {
		t.Errorf("endpoint = %q, want %q", got.Endpoint, sock)
	}
	if !strings.Contains(got.Reason, sock) {
		t.Errorf("reason = %q, which does not say which socket", got.Reason)
	}
}

// A DOCKER_HOST with no docker to use it is usually a shell profile that outlived
// the package. Saying so is the difference between the operator believing the
// host has no runtime and them finding the stale line.
func TestADockerHostWithNoDockerIsSaidOutLoud(t *testing.T) {
	const sock = "tcp://10.0.0.5:2375"
	got := Detect(cmdrun.NewFakeLooker(), env(map[string]string{DockerHostEnv: sock}))
	if got.Found() {
		t.Errorf("Found() = true from a variable alone: %+v", got)
	}
	if !strings.Contains(got.Reason, sock) {
		t.Errorf("reason = %q, which does not mention the stale %s", got.Reason, DockerHostEnv)
	}
}

// The podman dialect is deferred, not declined (issue #7). Until its parsers
// exist, a podman-only host must keep reporting the docker domain unavailable --
// v1's behaviour -- rather than being handed to parsers written for docker's
// output shape.
func TestPodmanIsReservedAndNotDetected(t *testing.T) {
	if got := Detect(cmdrun.NewFakeLooker("podman"), noEnv); got.Found() {
		t.Errorf("Detect() = %+v; the podman dialect does not exist yet", got)
	}
	if Podman == None || Podman == Docker {
		t.Error("the Podman kind must be a distinct reserved string")
	}
}

// Detect asks PATH and nothing else. It runs on every invocation including the
// ones that will not touch containers, so a subprocess here is a cost every
// `fleetfix check` pays for a question it did not ask.
func TestDetectLooksOnlyForWhatItSupports(t *testing.T) {
	look := cmdrun.NewFakeLooker("docker")
	Detect(look, noEnv)
	if got := look.Looked(); len(got) != 1 || got[0] != "docker" {
		t.Errorf("looked for %v, want exactly [docker]", got)
	}
}

// probe drives Probe against one registered docker response.
func probe(t *testing.T, r Runtime, setup func(*cmdrun.Fake)) Runtime {
	t.Helper()
	f := cmdrun.NewFake()
	setup(f)
	return r.Probe(t.Context(), f)
}

func detected() Runtime {
	return Runtime{Kind: Docker, Bin: "docker", Reason: found("docker", "")}
}

// The happy path, and the only one that may set Available.
func TestADaemonThatAnswersIsAvailable(t *testing.T) {
	got := probe(t, detected(), func(f *cmdrun.Fake) {
		f.Stdout("27.3.1\n", "docker", probeArgs...)
	})
	if !got.Available {
		t.Fatalf("Available = false after a clean version: %+v", got)
	}
	if !strings.Contains(got.Reason, "27.3.1") {
		t.Errorf("reason = %q, which does not name the version the daemon reported", got.Reason)
	}
}

// The single most common container-domain support call: the CLI is installed,
// the socket is not. Both facts have to survive, because "docker is missing" and
// "docker is stopped" are different things to fix.
func TestAStoppedDaemonIsFoundButNotAvailable(t *testing.T) {
	const msg = "Cannot connect to the Docker daemon at unix:///var/run/docker.sock."
	got := probe(t, detected(), func(f *cmdrun.Fake) {
		f.Exit(1, "", msg+"\nIs the docker daemon running?\n", "docker", probeArgs...)
	})
	if !got.Found() {
		t.Error("Found() = false; the CLI is still installed")
	}
	if got.Available {
		t.Error("Available = true against a daemon that refused")
	}
	if !strings.Contains(got.Reason, msg) {
		t.Errorf("reason = %q, which drops the daemon's own explanation", got.Reason)
	}
	// The second line is advice the operator staring at this line does not need
	// twice, and a two-line reason breaks the one-line-per-fact doctor output.
	if strings.Contains(got.Reason, "\n") {
		t.Errorf("reason spans lines:\n%s", got.Reason)
	}
	if strings.Contains(got.Reason, "Is the docker daemon running?") {
		t.Errorf("reason = %q, which kept the follow-up line", got.Reason)
	}
}

// A daemon behind a wedged socket answers nothing at all. Saying it did not
// answer is different from saying it refused, and only one of them is fixed by
// waiting.
func TestADaemonThatNeverAnswersSaysSo(t *testing.T) {
	got := probe(t, detected(), func(f *cmdrun.Fake) {
		f.Fail(context.DeadlineExceeded, "docker", probeArgs...)
	})
	if got.Available {
		t.Error("Available = true after a timeout")
	}
	if !got.Found() {
		t.Error("Found() = false; a timeout does not uninstall docker")
	}
	if !strings.Contains(got.Reason, "did not answer") {
		t.Errorf("reason = %q", got.Reason)
	}
}

// PATH differed between the lookup and the run, or the package was removed in
// between. Reporting it as absent is both true and the same answer Detect would
// have given a second later.
func TestADockerThatVanishedBetweenLookAndRunIsAbsent(t *testing.T) {
	const sock = "tcp://10.0.0.5:2375"
	r := detected()
	r.Endpoint = sock
	got := probe(t, r, func(f *cmdrun.Fake) { f.Missing("docker", probeArgs...) })
	if got.Found() {
		t.Errorf("Found() = true for a binary that is not there: %+v", got)
	}
	if got.Endpoint != sock {
		t.Errorf("endpoint = %q, want the variable to survive at %q", got.Endpoint, sock)
	}
	if !strings.Contains(got.Reason, sock) {
		t.Errorf("reason = %q, which loses the stale %s", got.Reason, DockerHostEnv)
	}
}

// Anything else the operating system refuses: a docker that is not executable, a
// fork that failed under memory pressure. Not a daemon problem, and not reported
// as one.
func TestAnOSRefusalIsReportedAsItself(t *testing.T) {
	got := probe(t, detected(), func(f *cmdrun.Fake) {
		f.Fail(errors.New("permission denied"), "docker", probeArgs...)
	})
	if got.Available {
		t.Error("Available = true after the command could not be run")
	}
	if !strings.Contains(got.Reason, "permission denied") {
		t.Errorf("reason = %q, which hides what the OS said", got.Reason)
	}
}

// Nothing to probe, and nothing to run. A host with no runtime must not produce
// a subprocess -- an unregistered call on a bare Fake is an error, which is the
// assertion.
func TestProbingNothingRunsNothing(t *testing.T) {
	before := Detect(cmdrun.NewFakeLooker(), noEnv)
	f := cmdrun.NewFake()
	if got := before.Probe(t.Context(), f); got != before {
		t.Errorf("Probe changed the answer: %+v", got)
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("ran %v with no runtime to run it", calls)
	}
}

// The cheapest question only a running daemon can answer. Pinned because the
// argv is the contract with docker, and a --format typo turns "the daemon is up"
// into "the daemon refused" on every host at once.
func TestProbeAsksForTheServerVersion(t *testing.T) {
	f := cmdrun.NewFake().Stdout("27.3.1\n", "docker", probeArgs...)
	detected().Probe(t.Context(), f)
	if !f.Called("docker", "version", "--format", "{{.Server.Version}}") {
		t.Errorf("ran %v, want the server-version query", f.Calls())
	}
}

// A daemon that answered without naming itself. It answered, which is the fact
// that matters -- reporting it unavailable over an empty string would take a
// working host offline in the report.
func TestADaemonThatAnswersWithoutAVersionIsStillAvailable(t *testing.T) {
	got := probe(t, detected(), func(f *cmdrun.Fake) { f.Stdout("\n", "docker", probeArgs...) })
	if !got.Available {
		t.Fatalf("Available = false: %+v", got)
	}
	if strings.Contains(got.Reason, "version ") {
		t.Errorf("reason = %q, which claims a version it does not have", got.Reason)
	}
}

// The endpoint has to survive the probe, for the same reason it is collected: a
// docker reporting on a different socket than the operator assumes is the
// hardest kind of wrong answer to spot.
func TestTheEndpointSurvivesASuccessfulProbe(t *testing.T) {
	const sock = "unix:///run/user/1000/docker.sock"
	r := detected()
	r.Endpoint = sock
	got := probe(t, r, func(f *cmdrun.Fake) { f.Stdout("27.3.1", "docker", probeArgs...) })
	if got.Endpoint != sock {
		t.Errorf("endpoint = %q, want %q", got.Endpoint, sock)
	}
	if !strings.Contains(got.Reason, sock) {
		t.Errorf("reason = %q, which does not say which socket answered", got.Reason)
	}
}

// Every outcome is the same false to a gate and an entirely different problem to
// a person. A duplicated line here means doctor tells two hosts with unrelated
// faults to do the same thing.
func TestEveryOutcomeHasItsOwnReason(t *testing.T) {
	reasons := map[string]string{
		"nothing installed": Detect(cmdrun.NewFakeLooker(), noEnv).Reason,
		"a stale variable": Detect(
			cmdrun.NewFakeLooker(), env(map[string]string{DockerHostEnv: "tcp://10.0.0.5:2375"}),
		).Reason,
		"installed, unprobed": detected().Reason,
		"running": probe(t, detected(), func(f *cmdrun.Fake) {
			f.Stdout("27.3.1", "docker", probeArgs...)
		}).Reason,
		"running, unnamed version": probe(t, detected(), func(f *cmdrun.Fake) {
			f.Stdout("", "docker", probeArgs...)
		}).Reason,
		"refused": probe(t, detected(), func(f *cmdrun.Fake) {
			f.Exit(1, "", "Cannot connect to the Docker daemon.", "docker", probeArgs...)
		}).Reason,
		"timed out": probe(t, detected(), func(f *cmdrun.Fake) {
			f.Fail(context.DeadlineExceeded, "docker", probeArgs...)
		}).Reason,
		"could not run": probe(t, detected(), func(f *cmdrun.Fake) {
			f.Fail(errors.New("permission denied"), "docker", probeArgs...)
		}).Reason,
	}

	seen := make(map[string]string, len(reasons))
	for name, reason := range reasons {
		if reason == "" {
			t.Errorf("%s produced no reason at all", name)
			continue
		}
		if first, dup := seen[reason]; dup {
			t.Errorf("%s and %s both say %q", first, name, reason)
			continue
		}
		seen[reason] = name
	}
}

func TestFirstLine(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"one line":            {"Cannot connect.", "Cannot connect."},
		"two lines":           {"Cannot connect.\nIs it running?", "Cannot connect."},
		"a leading blank":     {"\nCannot connect.\nIs it running?", "Cannot connect."},
		"trailing whitespace": {"Cannot connect.  \n", "Cannot connect."},
		"crlf":                {"Cannot connect.\r\nIs it running?\r\n", "Cannot connect."},
		"empty":               {"", ""},
		"whitespace only":     {"  \n\n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := firstLine(tc.in); got != tc.want {
				t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
