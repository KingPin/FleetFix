package docker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	coredocker "github.com/KingPin/FleetFix/v2/internal/core/docker"
)

// RestartLoopThreshold and RestartLoopWindow are v1's, read off
// modules/docker/dashboard.py:21 rather than picked here: more than three restarts
// whose most recent start is inside ten minutes is a container coming up and dying
// in a cycle, as opposed to one that has been restarted three times this year.
//
// Exported because they are the rule this check grades by, and internal/threshold
// has no docker entry to look them up in -- an operator reading a crit about a
// restart loop has to be able to find out what one is.
const (
	RestartLoopThreshold int64 = 3
	RestartLoopWindow          = 10 * time.Minute
)

// psArgs and inspectFormat are v1's, and the format string is load-bearing: it is
// the grammar internal/core/docker.ParseInspectFields was ported against and the
// shape the differential corpus was captured in.
var (
	psArgs        = []string{"ps", "-a", "--format", "{{json .}}"}
	inspectFormat = "{{.RestartCount}}|{{.LogPath}}|{{.State.StartedAt}}|{{.State.Status}}"
)

// containersBudget covers one ps and one inspect.
//
// Fixed, which it could not be under v1's shape: v1 inspected each container in its
// own subprocess with its own 5s timeout, so a host with forty containers had a
// worst case of over three minutes and no way to declare it in advance. One inspect
// call for every id costs the same on a host with forty containers as on one with
// two, which is what makes a budget the runner can enforce possible at all.
const containersBudget = 25 * time.Second

// A Container is one row of the dashboard: what ps said, joined to what inspect
// added. This is the shape of data[] for docker.containers, and best-effort like
// every data[] -- consumers pin on metrics[].
type Container struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Image        string `json:"image"`
	State        string `json:"state"`
	Status       string `json:"status"`
	Ports        string `json:"ports"`
	RestartCount int64  `json:"restart_count"`
	StartedAt    string `json:"started_at"`
	LogPath      string `json:"log_path"`

	// RestartLoop, Unhealthy and Dead are this check's verdicts, kept on the row so
	// the TUI and the JSON agree about which container was flagged and why.
	RestartLoop bool `json:"restart_loop"`
	Unhealthy   bool `json:"unhealthy"`
	Dead        bool `json:"dead"`
}

// Label is what to call this container in front of a person: v1's `c.name or
// c.id[:12]`, which is docker's own convention for an unnamed container.
func (c Container) Label() string {
	if c.Name != "" {
		return c.Name
	}
	if len(c.ID) > 12 {
		return c.ID[:12]
	}
	return c.ID
}

type containers struct {
	run     cmdrun.Runner
	runtime Runtime

	// now is the clock the restart-loop window is measured against. Nil means
	// time.Now; a test sets it, because a rule about "the last ten minutes" cannot
	// be asserted against a fixture otherwise.
	now func() time.Time
}

func (containers) Spec() check.Spec {
	return check.Spec{
		ID:        ContainersID,
		Title:     "Containers",
		Domain:    "docker",
		Budget:    containersBudget,
		InDefault: true,
	}
}

func (c containers) Run(ctx context.Context, in check.Input) check.Result {
	rt := c.runtime(ctx)
	if !rt.Available {
		return unavailableBecause(rt)
	}

	out, err := c.run.Run(ctx, rt.Bin, psArgs...)
	switch {
	case err != nil:
		return check.Result{
			Status:  check.StatusError,
			Summary: rt.Bin + " ps did not run",
			Error:   err.Error(),
		}
	case !out.OK():
		return check.Result{
			Status:  check.StatusError,
			Summary: rt.Bin + " ps failed",
			Error:   firstLine(out.Combined(), fmt.Sprintf("exited %d", out.ExitCode)),
		}
	}

	rows := c.rowsFrom(coredocker.ParsePSJSONLines(out.Stdout))
	if len(rows) == 0 {
		// ok, not unavailable. A daemon that answered and holds no containers is a
		// working host, and it is a very common one -- a fleet where every machine
		// runs docker for one occasional job would be permanently amber otherwise.
		return check.Result{
			Status:  check.StatusOK,
			Summary: "no containers on this host",
			Data:    []Container{},
			Metrics: []check.Metric{
				tally(ContainersMetric, 0, "containers, running and stopped"),
				tally(RunningMetric, 0, "containers in the running state"),
				tally(RestartLoopsMetric, 0, "containers restarting in a loop"),
			},
		}
	}

	c.inspect(ctx, rt.Bin, rows, in.Progress)
	c.grade(rows)
	return c.report(rows, in.Progress)
}

// rowsFrom turns what ps printed into rows.
//
// A line that decoded to something other than an object is skipped. v1 calls
// row["ID"] on whatever json.loads returned, so a line holding `123` or `null`
// raises out of list_containers and the whole pane reports nothing; skipping the
// line reports every container that did parse, which is the answer an operator can
// act on. Neither case is reachable from output docker produces.
func (c containers) rowsFrom(decoded []any) []Container {
	rows := make([]Container, 0, len(decoded))
	for _, v := range decoded {
		obj, ok := v.(map[string]any)
		if !ok {
			continue
		}
		// A row with no ID cannot be inspected and cannot be named. v1 raises
		// KeyError here, which is the same information delivered as a crash.
		id := text(obj, "ID")
		if id == "" {
			continue
		}
		rows = append(rows, Container{
			ID:     id,
			Name:   text(obj, "Names"),
			Image:  text(obj, "Image"),
			State:  text(obj, "State"),
			Status: text(obj, "Status"),
			Ports:  text(obj, "Ports"),
		})
	}
	return rows
}

// inspect fills in the three fields ps does not expose, for every row at once.
//
// One subprocess rather than v1's one per container. docker inspect takes every id
// and prints one formatted line each, in the order asked, so the join is by
// position -- and a reply with the wrong number of lines is refused wholesale
// rather than matched up hopefully, because a restart count attributed to the wrong
// container is worse than no restart count at all.
func (c containers) inspect(ctx context.Context, bin string, rows []Container, to check.Emitter) {
	args := append([]string{"inspect", "--format", inspectFormat}, ids(rows)...)
	out, err := c.run.Run(ctx, bin, args...)
	if err != nil {
		c.noInspect(to, err.Error())
		return
	}

	lines := nonEmptyLines(out.Stdout)
	if len(lines) != len(rows) {
		// Non-zero exit lands here too when it cost us lines: inspect exits 1 having
		// printed the containers it could read when one id has gone away between the
		// ps and the inspect, which is a race a busy host loses routinely.
		c.noInspect(to, fmt.Sprintf("%s inspect described %d of %d containers", bin, len(lines), len(rows)))
		return
	}
	for i, line := range lines {
		fields := coredocker.ParseInspectFields(line)
		rows[i].RestartCount = fields.RestartCount
		rows[i].LogPath = fields.LogPath
		if fields.StartedAt != nil {
			rows[i].StartedAt = *fields.StartedAt
		}
	}
}

// noInspect records that the restart counts are missing.
//
// A step rather than a status. ps answered, so the states and the images are real
// and worth reporting; what is lost is the restart-loop verdict, and saying so is
// the difference between "no loops" and "nobody looked". v1 reported zero here
// and drew a green pane.
func (c containers) noInspect(to check.Emitter, why string) {
	to.Emit(check.Event{
		Text:   "restart counts are unavailable, so no restart loop can be detected: " + why,
		Status: check.StatusWarn,
	})
}

// grade applies the three verdicts. Each is the runtime's own assessment rather
// than a bound picked here -- see the package doc on why the domain grades boolean.
func (c containers) grade(rows []Container) {
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	at := now()
	for i := range rows {
		rows[i].RestartLoop = restartLoop(rows[i], at)
		// docker appends its healthcheck's verdict to the human status line, which
		// is the only place ps reports it: "Up 2 hours (unhealthy)".
		rows[i].Unhealthy = strings.Contains(rows[i].Status, "(unhealthy)")
		// dead is docker's own word for a container whose filesystem it could not
		// tear down. It never clears on its own.
		rows[i].Dead = rows[i].State == "dead"
	}
}

// restartLoop is v1's Container.is_restart_loop (dashboard.py:38), with one
// departure: a start time docker wrote without a timezone is not a loop here, where
// v1 raises TypeError comparing an aware now to a naive reading and takes the whole
// pane down with it. Docker writes Z-suffixed timestamps, so neither behaviour is
// reachable from a real daemon.
//
// The strictly-greater comparison is v1's too, so exactly three restarts is not a
// loop; and a start time in the future counts, because a host whose clock jumped is
// a host whose restart window cannot be trusted to exclude anything.
func restartLoop(c Container, now time.Time) bool {
	if c.RestartCount <= RestartLoopThreshold {
		return false
	}
	parsed, ok := coredocker.ParseISO(c.StartedAt)
	if !ok {
		return false
	}
	started, ok := parsed.Instant()
	if !ok {
		return false
	}
	return now.Sub(started) <= RestartLoopWindow
}

// report turns the graded rows into the check's answer.
func (c containers) report(rows []Container, to check.Emitter) check.Result {
	res := check.Result{Data: rows}

	var running, loops, unhealthy, dead int
	for _, row := range rows {
		if row.State == "running" {
			running++
		}
		if row.RestartLoop {
			loops++
		}
		if row.Unhealthy {
			unhealthy++
		}
		if row.Dead {
			dead++
		}
		res.Metrics = append(res.Metrics, counter(
			RestartsMetric, float64(row.RestartCount), "count",
			map[string]string{"container": row.Label()},
			"times the daemon has restarted this container",
		))
		// A step per flagged container, and none for the quiet ones. The full
		// inventory is in data[] -- a hundred-container host would otherwise put a
		// hundred lines in steps[] and bury the two that matter.
		//
		// Emitted rather than appended to res.Steps: the runner keeps what a check
		// emits, and building the slice here would silently drop the warning
		// inspect may already have emitted above it.
		if line, status, flagged := flag(row); flagged {
			to.Emit(check.Event{Text: line, Status: status})
		}
	}

	res.Metrics = append(
		res.Metrics,
		tally(ContainersMetric, len(rows), "containers, running and stopped"),
		tally(RunningMetric, running, "containers in the running state"),
		tally(RestartLoopsMetric, loops, "containers restarting in a loop"),
	)
	res.Status = worst(loops, unhealthy, dead)
	res.Summary = summarize(len(rows), running, loops, unhealthy, dead)
	return res
}

// flag describes a container worth looking at, or reports that there is nothing to
// say about it.
func flag(c Container) (string, check.Status, bool) {
	switch {
	case c.RestartLoop:
		return fmt.Sprintf("%s has restarted %d times, most recently at %s",
			c.Label(), c.RestartCount, c.StartedAt), check.StatusCrit, true
	case c.Dead:
		return fmt.Sprintf("%s is dead; the daemon could not tear it down", c.Label()), check.StatusWarn, true
	case c.Unhealthy:
		return fmt.Sprintf("%s is up but failing its healthcheck (%s)", c.Label(), c.Status), check.StatusWarn, true
	default:
		return "", "", false
	}
}

// worst is the check's status.
//
// A restart loop is crit and the other two are warn, and the difference is whether
// the container is currently in the cycle: a loop is a service that is not up now
// and will not be a minute from now, while an unhealthy or dead container is a
// steady wrong state somebody has to attend to, not a moving one.
func worst(loops, unhealthy, dead int) check.Status {
	switch {
	case loops > 0:
		return check.StatusCrit
	case unhealthy > 0 || dead > 0:
		return check.StatusWarn
	default:
		return check.StatusOK
	}
}

// summarize is v1's dashboard line -- "12 containers, 9 running" (screens/docker.py:130)
// -- with the fault counts appended only when there are any. v1 printed "0 in
// restart loop" on every healthy host, which is a sentence that trains an operator
// to stop reading the end of the line.
func summarize(total, running, loops, unhealthy, dead int) string {
	line := fmt.Sprintf("%s, %d running", plural(total, "container"), running)
	for _, part := range []struct {
		n    int
		noun string
	}{
		{loops, "in a restart loop"},
		{unhealthy, "unhealthy"},
		{dead, "dead"},
	} {
		if part.n > 0 {
			line += fmt.Sprintf(", %d %s", part.n, part.noun)
		}
	}
	return line
}

func tally(name string, n int, help string) check.Metric {
	return gauge(name, float64(n), "count", map[string]string{}, help)
}

func ids(rows []Container) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.ID
	}
	return out
}

// text reads a string field, defaulting to "". Only a string is a value: a present
// null or a number reaches v1's dataclass unvalidated and renders as itself in the
// table, which is a fact about the JSON rather than about the container.
func text(obj map[string]any, key string) string {
	s, _ := obj[key].(string)
	return s
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func firstLine(s, fallback string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return fallback
}
