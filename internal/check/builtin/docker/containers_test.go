package docker

import (
	"errors"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
)

// now is a fixed instant a few minutes after the fixture's containers started, so
// "within the last ten minutes" has a definite answer.
const now = "2026-07-30T10:05:00Z"

func containersOn(fake *cmdrun.Fake) containers {
	return containers{run: fake, runtime: liveDocker(), now: at(now)}
}

// The join is the whole check: ps knows the image and the state, inspect knows the
// restart count and the start time, and neither is useful on its own.
func TestContainersJoinPsToInspect(t *testing.T) {
	res, _ := run(t, containersOn(healthyHost()))

	rows, ok := res.Data.([]Container)
	if !ok || len(rows) != 2 {
		t.Fatalf("data = %#v, want two containers", res.Data)
	}
	web := rows[0]
	if web.Name != "web" || web.Image != "nginx:1.27" || web.State != "running" {
		t.Errorf("ps fields did not arrive: %+v", web)
	}
	if web.RestartCount != 2 || web.LogPath != "/var/lib/docker/containers/a1b2/a1b2-json.log" {
		t.Errorf("inspect fields did not arrive: %+v", web)
	}
	if web.StartedAt != "2026-07-30T10:00:00.123456789Z" {
		t.Errorf("started at %q", web.StartedAt)
	}
	// The second row's inspect line has an empty LogPath and docker's zero start
	// time, which is what a container that has never run looks like.
	if db := rows[1]; db.Name != "db" || db.RestartCount != 0 || db.LogPath != "" {
		t.Errorf("second row = %+v", db)
	}
}

// One inspect for every id, not one each. v1 spent a subprocess and a 5s timeout
// per container, which is why it could not declare a budget; this is what makes
// containersBudget a number the runner can enforce.
func TestEveryContainerIsInspectedInOneCall(t *testing.T) {
	fake := healthyHost()
	run(t, containersOn(fake))

	if got := len(fake.Calls()); got != 2 {
		t.Fatalf("calls = %v, want one ps and one inspect", fake.Calls())
	}
	if !fake.Called("docker", psArgs...) {
		t.Errorf("ps was not run; calls = %v", fake.Calls())
	}
	if !fake.Called("docker", "inspect", "--format", inspectFormat, "a1b2c3d4e5f6a7b8", "b2c3d4e5f6a7b8c9") {
		t.Errorf("inspect argv = %v", fake.Calls())
	}
}

func TestAHealthyHostIsOkAndCountsWhatIsRunning(t *testing.T) {
	res, rec := run(t, containersOn(healthyHost()))

	if res.Status != check.StatusOK {
		t.Errorf("status = %s, want ok", res.Status)
	}
	if res.Summary != "2 containers, 1 running" {
		t.Errorf("summary = %q", res.Summary)
	}
	// No steps: nothing was flagged, and a line per quiet container would bury the
	// ones that matter on a host running eighty of them.
	if got := rec.texts(); len(got) != 0 {
		t.Errorf("steps = %v, want none on a healthy host", got)
	}
}

// v1's rule, read off dashboard.py:38 -- more than three restarts whose most recent
// start is inside ten minutes.
func TestARestartLoopIsCritAndNamesTheContainer(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Stdout(psOne("deadbeefcafe0001", "worker", "acme/worker:3", "restarting", "Restarting (1) 2 seconds ago"),
		"docker", psArgs...)
	fake.Stdout(inspectOne(9, "/var/log/w.log", "2026-07-30T10:04:30Z", "restarting"),
		"docker", "inspect", "--format", inspectFormat, "deadbeefcafe0001")

	res, rec := run(t, containersOn(fake))

	if res.Status != check.StatusCrit {
		t.Fatalf("status = %s, want crit", res.Status)
	}
	if res.Summary != "1 container, 0 running, 1 in a restart loop" {
		t.Errorf("summary = %q", res.Summary)
	}
	want := "worker has restarted 9 times, most recently at 2026-07-30T10:04:30Z"
	if got := rec.texts(); len(got) != 1 || got[0] != want {
		t.Errorf("steps = %v, want [%q]", got, want)
	}
	if rec.events[0].Status != check.StatusCrit {
		t.Errorf("step status = %s, want crit", rec.events[0].Status)
	}
	if rows := res.Data.([]Container); !rows[0].RestartLoop {
		t.Error("the row does not carry the verdict the summary reported")
	}
}

// The threshold is strictly greater, which is v1's `<=` returning early. Exactly
// three restarts inside the window is a container someone bounced, not a loop.
func TestTheRestartLoopThresholdIsStrictlyGreater(t *testing.T) {
	for _, tc := range []struct {
		restarts int
		want     check.Status
	}{
		{3, check.StatusOK},
		{4, check.StatusCrit},
	} {
		fake := cmdrun.NewFake()
		fake.Stdout(psOne("aaaa0000bbbb1111", "svc", "svc:1", "running", "Up 4 seconds"), "docker", psArgs...)
		fake.Stdout(inspectOne(tc.restarts, "", "2026-07-30T10:04:56Z", "running"),
			"docker", "inspect", "--format", inspectFormat, "aaaa0000bbbb1111")

		res, _ := run(t, containersOn(fake))
		if res.Status != tc.want {
			t.Errorf("%d restarts = %s, want %s", tc.restarts, res.Status, tc.want)
		}
	}
}

// Outside the window it is a container that has been restarted a lot over its life,
// which is a different fact and not one to wake anybody for.
func TestManyRestartsLongAgoAreNotALoop(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Stdout(psOne("aaaa0000bbbb1111", "svc", "svc:1", "running", "Up 3 hours"), "docker", psArgs...)
	fake.Stdout(inspectOne(400, "", "2026-07-30T07:00:00Z", "running"),
		"docker", "inspect", "--format", inspectFormat, "aaaa0000bbbb1111")

	res, rec := run(t, containersOn(fake))

	if res.Status != check.StatusOK {
		t.Errorf("status = %s, want ok; 400 restarts three hours ago is history", res.Status)
	}
	if got := rec.texts(); len(got) != 0 {
		t.Errorf("steps = %v", got)
	}
}

// A start time ahead of now means the host's clock moved, and a restart window that
// cannot be trusted to exclude anything should not exclude this. v1 arrives at the
// same answer by subtraction.
func TestAStartTimeInTheFutureStillCounts(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Stdout(psOne("aaaa0000bbbb1111", "svc", "svc:1", "running", "Up 1 second"), "docker", psArgs...)
	fake.Stdout(inspectOne(20, "", "2026-07-30T11:30:00Z", "running"),
		"docker", "inspect", "--format", inspectFormat, "aaaa0000bbbb1111")

	if res, _ := run(t, containersOn(fake)); res.Status != check.StatusCrit {
		t.Errorf("status = %s, want crit", res.Status)
	}
}

// A start time with no timezone: v1 raises TypeError comparing it to an aware now
// and takes the whole pane down. Not a loop here, and the rest of the host is still
// reported -- neither behaviour is reachable from a real daemon, which writes Z.
func TestAStartTimeWithNoTimezoneIsNotALoop(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Stdout(psOne("aaaa0000bbbb1111", "svc", "svc:1", "running", "Up 4 seconds"), "docker", psArgs...)
	fake.Stdout(inspectOne(50, "", "2026-07-30T10:04:56", "running"),
		"docker", "inspect", "--format", inspectFormat, "aaaa0000bbbb1111")

	res, _ := run(t, containersOn(fake))

	if res.Status != check.StatusOK {
		t.Errorf("status = %s, want ok", res.Status)
	}
	if res.Summary != "1 container, 1 running" {
		t.Errorf("summary = %q; the container is still reported", res.Summary)
	}
}

// Docker's zero start time is "never started", so a container with a restart count
// and no start has nothing to measure a window against.
func TestAContainerThatNeverStartedIsNotALoop(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Stdout(psOne("aaaa0000bbbb1111", "svc", "svc:1", "created", "Created"), "docker", psArgs...)
	fake.Stdout(inspectOne(99, "", "0001-01-01T00:00:00Z", "created"),
		"docker", "inspect", "--format", inspectFormat, "aaaa0000bbbb1111")

	if res, _ := run(t, containersOn(fake)); res.Status != check.StatusOK {
		t.Errorf("status = %s, want ok", res.Status)
	}
}

// The two steady wrong states, both warn: docker's own healthcheck verdict, and
// docker's own word for a container whose filesystem it could not tear down.
func TestTheSteadyWrongStatesAreWarn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  string
		status string
		want   string
	}{
		{
			"unhealthy", "running", "Up 2 hours (unhealthy)",
			"svc is up but failing its healthcheck (Up 2 hours (unhealthy))",
		},
		{
			"dead", "dead", "Dead",
			"svc is dead; the daemon could not tear it down",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := cmdrun.NewFake()
			fake.Stdout(psOne("aaaa0000bbbb1111", "svc", "svc:1", tc.state, tc.status), "docker", psArgs...)
			fake.Stdout(inspectOne(0, "", "2026-07-30T08:00:00Z", tc.state),
				"docker", "inspect", "--format", inspectFormat, "aaaa0000bbbb1111")

			res, rec := run(t, containersOn(fake))

			if res.Status != check.StatusWarn {
				t.Errorf("status = %s, want warn", res.Status)
			}
			if got := rec.texts(); len(got) != 1 || got[0] != tc.want {
				t.Errorf("steps = %v, want [%q]", got, tc.want)
			}
			if rec.events[0].Status != check.StatusWarn {
				t.Errorf("step status = %s", rec.events[0].Status)
			}
		})
	}
}

// A loop outranks the other two, and the summary names all of them: an operator
// triaging one host wants to know it has three problems, not its worst one.
func TestEveryFaultIsCountedAndTheWorstDecidesTheStatus(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Stdout(
		psOne("1111111111111111", "loop", "a:1", "restarting", "Restarting (1) 1 second ago")+
			psOne("2222222222222222", "sick", "b:1", "running", "Up 3 hours (unhealthy)")+
			psOne("3333333333333333", "gone", "c:1", "dead", "Dead")+
			psOne("4444444444444444", "fine", "d:1", "running", "Up 3 hours"),
		"docker", psArgs...,
	)
	fake.Stdout(
		inspectOne(7, "", "2026-07-30T10:04:59Z", "restarting")+
			inspectOne(0, "", "2026-07-30T07:00:00Z", "running")+
			inspectOne(0, "", "2026-07-30T07:00:00Z", "dead")+
			inspectOne(0, "", "2026-07-30T07:00:00Z", "running"),
		"docker", "inspect", "--format", inspectFormat,
		"1111111111111111", "2222222222222222", "3333333333333333", "4444444444444444",
	)

	res, rec := run(t, containersOn(fake))

	if res.Status != check.StatusCrit {
		t.Errorf("status = %s, want crit", res.Status)
	}
	want := "4 containers, 2 running, 1 in a restart loop, 1 unhealthy, 1 dead"
	if res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if got := rec.texts(); len(got) != 3 {
		t.Errorf("steps = %v, want one per flagged container", got)
	}
}

// The restart count is a counter per container, which is what an alert on "this
// service is flapping" is actually written against.
func TestEachContainersRestartCountIsACounter(t *testing.T) {
	res, _ := run(t, containersOn(healthyHost()))

	m := metricNamed(t, res, RestartsMetric, "web")
	if m.Value != 2 || m.Kind != check.Counter {
		t.Errorf("%s{web} = %v (%s), want 2 as a counter", m.Name, m.Value, m.Kind)
	}
	for _, name := range []string{ContainersMetric, RunningMetric, RestartLoopsMetric} {
		if got := metricNamed(t, res, name, "").Kind; got != check.Gauge {
			t.Errorf("%s is a %s, want a gauge", name, got)
		}
	}
	if got := metricNamed(t, res, RunningMetric, "").Value; got != 1 {
		t.Errorf("%s = %v, want 1", RunningMetric, got)
	}
}

// v1's `c.name or c.id[:12]`, which is docker's own convention.
func TestAnUnnamedContainerIsCalledByItsShortId(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Stdout(psOne("abcdef0123456789", "", "svc:1", "dead", "Dead"), "docker", psArgs...)
	fake.Stdout(inspectOne(0, "", "2026-07-30T08:00:00Z", "dead"),
		"docker", "inspect", "--format", inspectFormat, "abcdef0123456789")

	_, rec := run(t, containersOn(fake))

	if got := rec.texts(); len(got) != 1 || !strings.HasPrefix(got[0], "abcdef012345 is dead") {
		t.Errorf("steps = %v, want the twelve-character id", got)
	}
}

// A short id is used whole rather than sliced past its end.
func TestAShortIdIsNotTruncated(t *testing.T) {
	if got := (Container{ID: "abc"}).Label(); got != "abc" {
		t.Errorf("Label() = %q", got)
	}
}

// A daemon that answered and holds nothing is a working host, and a very common
// one. The metrics are still emitted, at zero, so the series does not vanish.
func TestNoContainersIsOkWithZeroesRatherThanSilence(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Stdout("", "docker", psArgs...)

	res, _ := run(t, containersOn(fake))

	if res.Status != check.StatusOK || res.Summary != "no containers on this host" {
		t.Errorf("status = %s, summary = %q", res.Status, res.Summary)
	}
	if len(res.Metrics) != 3 {
		t.Fatalf("metrics = %v, want the three tallies at zero", res.Metrics)
	}
	for _, m := range res.Metrics {
		if m.Value != 0 {
			t.Errorf("%s = %v on an empty host", m.Name, m.Value)
		}
	}
	// Nothing to inspect, so nothing was asked.
	if len(fake.Calls()) != 1 {
		t.Errorf("calls = %v, want just the ps", fake.Calls())
	}
}

// A reply with the wrong number of lines is refused wholesale. Matching them up
// hopefully would attribute one container's restart count to another, which reads
// as a confident wrong answer rather than a missing one.
func TestAMismatchedInspectIsRefusedRatherThanMisaligned(t *testing.T) {
	fake := healthyHost()
	// One line back for two containers: the second id went away between the calls.
	fake.Stdout(inspectOne(2, "/var/log/a.log", "2026-07-30T10:00:00Z", "running"),
		"docker", "inspect", "--format", inspectFormat, "a1b2c3d4e5f6a7b8", "b2c3d4e5f6a7b8c9")

	res, rec := run(t, containersOn(fake))

	rows := res.Data.([]Container)
	if rows[0].RestartCount != 0 || rows[1].RestartCount != 0 {
		t.Errorf("restart counts survived a mismatched inspect: %+v", rows)
	}
	want := "restart counts are unavailable, so no restart loop can be detected: " +
		"docker inspect described 1 of 2 containers"
	if got := rec.texts(); len(got) != 1 || got[0] != want {
		t.Errorf("steps = %v, want [%q]", got, want)
	}
	if rec.events[0].Status != check.StatusWarn {
		t.Errorf("step status = %s, want warn", rec.events[0].Status)
	}
	// ps answered, so the states and images are real and still worth reporting.
	if res.Status != check.StatusOK || res.Summary != "2 containers, 1 running" {
		t.Errorf("status = %s, summary = %q", res.Status, res.Summary)
	}
}

// The difference between "no loops" and "nobody looked". v1 reported zero and drew
// a green pane.
func TestAFailedInspectIsAStepNotAStatus(t *testing.T) {
	fake := healthyHost()
	fake.Fail(errors.New("signal: killed"), "docker",
		"inspect", "--format", inspectFormat, "a1b2c3d4e5f6a7b8", "b2c3d4e5f6a7b8c9")

	res, rec := run(t, containersOn(fake))

	if res.Status != check.StatusOK {
		t.Errorf("status = %s; ps answered, so the inventory is real", res.Status)
	}
	if got := rec.texts(); len(got) != 1 || !strings.HasSuffix(got[0], "signal: killed") {
		t.Errorf("steps = %v, want the reason inspect gave", got)
	}
}

// ps is the check. Without it there is no inventory and nothing to say about one.
func TestAFailedPsIsAnError(t *testing.T) {
	t.Run("did not run", func(t *testing.T) {
		fake := cmdrun.NewFake()
		fake.Fail(cmdrun.ErrNotFound, "docker", psArgs...)

		res, _ := run(t, containersOn(fake))

		if res.Status != check.StatusError || res.Summary != "docker ps did not run" {
			t.Errorf("status = %s, summary = %q", res.Status, res.Summary)
		}
		if !strings.Contains(res.Error, cmdrun.ErrNotFound.Error()) {
			t.Errorf("error = %q", res.Error)
		}
	})

	t.Run("exited non-zero", func(t *testing.T) {
		fake := cmdrun.NewFake()
		fake.Exit(1, "", "permission denied while trying to connect to the Docker daemon socket\nsee 'docker run --help'",
			"docker", psArgs...)

		res, _ := run(t, containersOn(fake))

		if res.Status != check.StatusError || res.Summary != "docker ps failed" {
			t.Errorf("status = %s, summary = %q", res.Status, res.Summary)
		}
		// One line. docker's second line is advice the operator does not need.
		if res.Error != "permission denied while trying to connect to the Docker daemon socket" {
			t.Errorf("error = %q", res.Error)
		}
	})

	t.Run("exited non-zero saying nothing", func(t *testing.T) {
		fake := cmdrun.NewFake()
		fake.Exit(125, "", "", "docker", psArgs...)

		if res, _ := run(t, containersOn(fake)); res.Error != "exited 125" {
			t.Errorf("error = %q, want the exit code when there is no message", res.Error)
		}
	})
}

// v1 calls row["ID"] on whatever json.loads returned, so one malformed line takes
// the whole pane down. Every container that did parse is reported here.
func TestALineThatIsNotAContainerIsSkipped(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Stdout(
		"123\n"+ // a bare scalar
			`{"Image":"orphan:1","State":"running","Status":"Up"}`+"\n"+ // no ID
			"not json at all\n"+
			psOne("aaaa0000bbbb1111", "svc", "svc:1", "running", "Up 3 hours"),
		"docker", psArgs...,
	)
	fake.Stdout(inspectOne(0, "", "2026-07-30T07:00:00Z", "running"),
		"docker", "inspect", "--format", inspectFormat, "aaaa0000bbbb1111")

	res, _ := run(t, containersOn(fake))

	if res.Summary != "1 container, 1 running" {
		t.Errorf("summary = %q, want only the container that parsed", res.Summary)
	}
}

// The other two checks stay quiet when the runtime check has the verdict, and they
// pass through the daemon's own explanation rather than writing one.
func TestNoDaemonMeansUnavailableWithTheDaemonsReason(t *testing.T) {
	fake := cmdrun.NewFake()
	c := containers{run: fake, runtime: deadDocker(), now: at(now)}

	res, _ := run(t, c)

	if res.Status != check.StatusUnavailable {
		t.Errorf("status = %s, want unavailable", res.Status)
	}
	if !strings.Contains(res.Summary, "unix:///var/run/docker.sock") {
		t.Errorf("summary = %q", res.Summary)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("calls = %v; a dead daemon was asked for its containers", fake.Calls())
	}
}

// The default clock. Every other test pins one, so without this the live branch
// would never run -- and a nil clock is a panic on the first real host.
func TestTheClockDefaultsToTheRealOne(t *testing.T) {
	fake := healthyHost()
	c := containers{run: fake, runtime: liveDocker()}

	res, _ := run(t, c)

	// The fixture's restart count is 2, under the threshold, so today's date cannot
	// make this a loop however far it is from the fixture's timestamps.
	if res.Status != check.StatusOK {
		t.Errorf("status = %s, want ok", res.Status)
	}
}
