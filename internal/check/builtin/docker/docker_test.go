package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/container"
)

// Fixtures. Captured shapes rather than invented ones: docker's JSON-lines output
// quotes its numbers, which is the detail a hand-written fixture gets wrong and
// which internal/core/docker's intOrZero exists to absorb.
const (
	psWebAndDB = `{"Command":"\"nginx -g 'daemon of…\"","CreatedAt":"2026-07-30 10:00:00 +0000 UTC","ID":"a1b2c3d4e5f6a7b8","Image":"nginx:1.27","Labels":"","LocalVolumes":"0","Mounts":"","Names":"web","Networks":"bridge","Ports":"0.0.0.0:80->80/tcp","RunningFor":"2 days ago","Size":"0B","State":"running","Status":"Up 2 days"}
{"Command":"\"postgres\"","CreatedAt":"2026-07-30 10:00:01 +0000 UTC","ID":"b2c3d4e5f6a7b8c9","Image":"postgres:16","Labels":"","LocalVolumes":"1","Mounts":"pgdata","Names":"db","Networks":"bridge","Ports":"5432/tcp","RunningFor":"2 days ago","Size":"0B","State":"exited","Status":"Exited (0) 3 hours ago"}
`

	// web restarted twice and has been up since; db has never run.
	inspectWebAndDB = `2|/var/lib/docker/containers/a1b2/a1b2-json.log|2026-07-30T10:00:00.123456789Z|running
0||0001-01-01T00:00:00Z|exited
`

	dfFourCategories = `{"Active":"2","Reclaimable":"1.2GB (45%)","Size":"2.67GB","TotalCount":"7","Type":"Images"}
{"Active":"1","Reclaimable":"0B (0%)","Size":"136B","TotalCount":"2","Type":"Containers"}
{"Active":"1","Reclaimable":"512MB","Size":"512MB","TotalCount":"3","Type":"Local Volumes"}
{"Active":"0","Reclaimable":"48.3MB","Size":"48.3MB","TotalCount":"11","Type":"Build Cache"}
`
)

// psOne renders a single ps row. Takes the fields the checks read and leaves the
// rest at docker's shape, so a test naming a state does not also have to restate
// eight columns it does not care about.
func psOne(id, name, image, state, status string) string {
	return `{"Command":"\"sh\"","CreatedAt":"2026-07-30 10:00:00 +0000 UTC","ID":"` + id +
		`","Image":"` + image + `","Labels":"","LocalVolumes":"0","Mounts":"","Names":"` + name +
		`","Networks":"bridge","Ports":"","RunningFor":"2 days ago","Size":"0B","State":"` + state +
		`","Status":"` + status + `"}` + "\n"
}

// inspectOne renders a single inspect line in the pipe-delimited format the check
// asks docker for.
func inspectOne(restarts int, logPath, startedAt, status string) string {
	return strings.Join([]string{itoa(restarts), logPath, startedAt, status}, "|") + "\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// liveDocker, deadDocker and noDocker are the three answers internal/container
// gives, in its own words -- the Reason strings are the ones container.go builds,
// because the checks pass them through and a test asserting on a sentence this file
// invented would not notice if that stopped being true.
func liveDocker() Runtime {
	return func(context.Context) container.Runtime {
		return container.Runtime{
			Kind: container.Docker, Bin: "docker", Available: true,
			Reason: "docker daemon version 27.1.1",
		}
	}
}

func deadDocker() Runtime {
	return func(context.Context) container.Runtime {
		return container.Runtime{
			Kind: container.Docker, Bin: "docker",
			Reason: "docker is installed but the daemon did not answer: " +
				"Cannot connect to the Docker daemon at unix:///var/run/docker.sock.",
		}
	}
}

func noDocker() Runtime {
	return func(context.Context) container.Runtime {
		return container.Runtime{Reason: "no container runtime is installed"}
	}
}

// recorder is the Emitter a test runs a check with. The real runner collects steps
// the same way, so what lands here is what lands in steps[].
type recorder struct{ events []check.Event }

func (r *recorder) Emit(e check.Event) { r.events = append(r.events, e) }

func (r *recorder) texts() []string {
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.Text
	}
	return out
}

// run executes a check and returns its normalised result alongside what it emitted.
// Normalize is applied because the runner applies it, and an assertion about
// metrics[] on an un-normalised result would be asserting about a shape no consumer
// ever sees.
func run(t *testing.T, c check.Check) (check.Result, *recorder) {
	t.Helper()
	rec := &recorder{}
	res := c.Run(t.Context(), check.Input{Params: map[string]string{}, Progress: rec}).Normalize()
	return res, rec
}

// at is a fixed clock. The restart-loop rule is "within the last ten minutes", so
// every test that touches it has to say when now is.
func at(s string) func() time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return t }
}

// healthyHost stages the two-container fixture end to end.
func healthyHost() *cmdrun.Fake {
	fake := cmdrun.NewFake()
	fake.Stdout(psWebAndDB, "docker", psArgs...)
	fake.Stdout(inspectWebAndDB, "docker",
		"inspect", "--format", inspectFormat, "a1b2c3d4e5f6a7b8", "b2c3d4e5f6a7b8c9")
	return fake
}

func metricNamed(t *testing.T, res check.Result, name, label string) check.Metric {
	t.Helper()
	for _, m := range res.Metrics {
		if m.Name != name {
			continue
		}
		if label == "" {
			return m
		}
		for _, v := range m.Labels {
			if v == label {
				return m
			}
		}
	}
	t.Fatalf("no %s metric labelled %q among %v", name, label, res.Metrics)
	return check.Metric{}
}

func find(t *testing.T, checks []check.Check, id check.ID) check.Check {
	t.Helper()
	for _, c := range checks {
		if c.Spec().ID == id {
			return c
		}
	}
	t.Fatalf("%s is not in this domain", id)
	return nil
}

// The domain's shape, asserted once: three checks, all in the default run, all
// selectable by `--check docker`.
func TestTheDomainShipsThreeChecks(t *testing.T) {
	checks := Checks(cmdrun.NewFake(), noDocker())

	if len(checks) != 3 {
		t.Fatalf("the domain has %d checks, want 3", len(checks))
	}
	for _, c := range checks {
		spec := c.Spec()
		if err := spec.Validate(); err != nil {
			t.Errorf("%s: %v", spec.ID, err)
		}
		if spec.Domain != "docker" {
			t.Errorf("%s is in domain %q", spec.ID, spec.Domain)
		}
		if !spec.InDefault {
			t.Errorf("%s is not in the default run", spec.ID)
		}
		// NeedsBins would have the runner report unavailable before any of these
		// ran, which is the answer docker.runtime exists to give in its own words
		// -- with the DOCKER_HOST line that explains the surprising cases.
		if len(spec.NeedsBins) != 0 {
			t.Errorf("%s declares NeedsBins %v; the runtime check owns that question", spec.ID, spec.NeedsBins)
		}
	}
}

// A host with no runtime must cost no subprocesses at all. Half a fleet runs no
// containers, and three `docker version` calls per invocation on every one of them
// is the cost this domain would quietly add.
func TestNothingIsRunOnAHostWithNoRuntime(t *testing.T) {
	fake := cmdrun.NewFake()

	for _, c := range Checks(fake, noDocker()) {
		res, _ := run(t, c)
		if res.Status != check.StatusUnavailable {
			t.Errorf("%s = %s, want unavailable", c.Spec().ID, res.Status)
		}
		if res.Summary != "no container runtime is installed" {
			t.Errorf("%s summary = %q", c.Spec().ID, res.Summary)
		}
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("calls = %v; a host with no docker was asked something", fake.Calls())
	}
}

// v1's _human_bytes, quirks included: the divisor is 1024 while docker's own sizes
// are decimal, and bytes are whole where every larger unit carries one decimal.
func TestHumanBytesMatchesV1(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TB"},
		// Past the last unit the loop stops dividing, so a petabyte reads in
		// thousands of terabytes rather than growing a unit v1 never had.
		{1024 * 1024 * 1024 * 1024 * 1024, "1024.0 TB"},
	} {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
