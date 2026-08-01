package check

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/exitcode"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// allStatuses is every value of the frozen enum. Tests enumerate this rather than
// restating the list, so adding a seventh status fails the tests that have an
// opinion about all of them instead of quietly skipping it.
var allStatuses = []Status{
	StatusOK, StatusWarn, StatusCrit, StatusSkipped, StatusUnavailable, StatusError,
}

// The strings are the schema. A consumer switches on them, so a change here is a
// breaking change to every parser in the fleet -- which is why they are written
// out literally rather than derived from the constants.
func TestTheStatusEnumIsFrozen(t *testing.T) {
	want := []string{"ok", "warn", "crit", "skipped", "unavailable", "error"}
	if len(allStatuses) != len(want) {
		t.Fatalf("the enum has %d values and the contract names %d; a status was added or removed", len(allStatuses), len(want))
	}
	for i, s := range allStatuses {
		if string(s) != want[i] {
			t.Errorf("status %d is %q, want %q", i, s, want[i])
		}
		if !s.Valid() {
			t.Errorf("%s does not consider itself valid", s)
		}
	}
	if Status("degraded").Valid() {
		t.Error("an invented status validated")
	}
}

// The ranking is a policy, so it is asserted as one rather than left to whatever
// order the switch happens to be written in.
func TestWorseRanksUnknownAboveMildAndDefiniteAboveBoth(t *testing.T) {
	// Ascending. Each is worse than everything before it.
	ladder := []Status{
		StatusOK, StatusSkipped, StatusUnavailable, StatusWarn, StatusError, StatusCrit,
	}
	for i, worse := range ladder {
		for _, milder := range ladder[:i] {
			if got := Worse(milder, worse); got != worse {
				t.Errorf("Worse(%s, %s) = %s, want %s", milder, worse, got, worse)
			}
			if got := Worse(worse, milder); got != worse {
				t.Errorf("Worse(%s, %s) = %s, want %s", worse, milder, got, worse)
			}
		}
	}
	// An unrecognised status outranks everything rather than being ignored: it is
	// itself a defect, and ranking it below ok would hide it.
	if got := Worse(StatusCrit, Status("degraded")); got != Status("degraded") {
		t.Errorf("Worse(crit, degraded) = %s, want the unrecognised value to win", got)
	}
}

// A host with no docker is not an unhealthy host. If an absence exited non-zero,
// every cron job on every docker-less host would page forever.
func TestAnAbsenceExitsZeroAndAnUnknownExitsThree(t *testing.T) {
	for status, want := range map[Status]int{
		StatusOK:          exitcode.OK,
		StatusSkipped:     exitcode.OK,
		StatusUnavailable: exitcode.OK,
		StatusWarn:        exitcode.Warn,
		StatusCrit:        exitcode.Crit,
		StatusError:       exitcode.Unknown,
		"degraded":        exitcode.Unknown,
	} {
		if got := status.ExitCode(); got != want {
			t.Errorf("%s exits %d, want %d", status, got, want)
		}
	}
}

// threshold's three severities are exactly the three gradeable statuses, and this
// is the only place that correspondence is written down.
func TestFromSeverityCoversEverySeverity(t *testing.T) {
	for sev, want := range map[threshold.Severity]Status{
		threshold.OK:   StatusOK,
		threshold.Warn: StatusWarn,
		threshold.Crit: StatusCrit,
	} {
		if got := FromSeverity(sev); got != want {
			t.Errorf("FromSeverity(%s) = %s, want %s", sev, got, want)
		}
	}
	// A severity from outside the ladder grades ok rather than crit: an unknown
	// severity is not evidence of a problem, and treating it as one would page on
	// a bug in the grader.
	if got := FromSeverity(threshold.Severity(99)); got != StatusOK {
		t.Errorf("an unknown severity became %s", got)
	}
}

func TestSpecValidateRejectsWhatWouldBeInvisible(t *testing.T) {
	good := Spec{ID: "disk.usage", Title: "Disk usage", Domain: "disk", InDefault: true}
	if err := good.Validate(); err != nil {
		t.Fatalf("a good spec was rejected: %v", err)
	}
	for name, tc := range map[string]struct {
		spec Spec
		want string
	}{
		"no id":        {Spec{Title: "T", Domain: "d"}, "no id"},
		"not dotted":   {Spec{ID: "disk", Title: "T", Domain: "disk"}, "dotted"},
		"upper case":   {Spec{ID: "Disk.Usage", Title: "T", Domain: "Disk"}, "dotted"},
		"a space":      {Spec{ID: "disk usage.x", Title: "T", Domain: "disk usage"}, "dotted"},
		"trailing dot": {Spec{ID: "disk.", Title: "T", Domain: "disk"}, "dotted"},
		"double dot":   {Spec{ID: "disk..usage", Title: "T", Domain: "disk"}, "dotted"},
		"no title":     {Spec{ID: "disk.usage", Domain: "disk"}, "no title"},
		"no domain":    {Spec{ID: "disk.usage", Title: "T"}, "no domain"},
		// Selection is by dotted prefix, so this check would be unselectable by
		// domain -- silently, and only for this one check.
		"domain does not match the id": {
			Spec{ID: "disk.usage", Title: "T", Domain: "storage"},
			"does not start with its domain",
		},
		"negative budget": {
			Spec{ID: "disk.usage", Title: "T", Domain: "disk", Budget: -time.Second},
			"negative budget",
		},
		"a parameter with no name": {
			Spec{ID: "disk.usage", Title: "T", Domain: "disk", Params: []ParamSpec{{Description: "d"}}},
			"parameter has no name",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.spec.Validate()
			if err == nil {
				t.Fatalf("%+v validated", tc.spec)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not explain %q", err, tc.want)
			}
		})
	}
}

// The rule a check author cannot be expected to remember, applied on their behalf.
// nil marshals to null and an empty slice marshals to []; the two are different
// documents to a consumer, and the difference is invisible in Go.
func TestNormalizeNeverLeavesANilSlice(t *testing.T) {
	r := Result{ID: "disk.usage", Status: StatusOK}.Normalize()
	blob, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"trips":[]`, `"metrics":[]`, `"steps":[]`} {
		if !strings.Contains(string(blob), key) {
			t.Errorf("marshalled result is missing %s:\n%s", key, blob)
		}
	}
	if strings.Contains(string(blob), "null") && !strings.Contains(string(blob), `"data":null`) {
		t.Errorf("something other than data marshalled to null:\n%s", blob)
	}
	// A metric's labels are the same hazard one level down.
	r = Result{Metrics: []Metric{{Name: "disk.used_pct", Value: 1}}}.Normalize()
	if r.Metrics[0].Labels == nil {
		t.Error("a metric kept nil labels")
	}
}

// A check that reports findings and forgets to set a status is a real mistake --
// the zero value of a string type is "", which is not a valid status and would
// reach the wire. Deriving it from the trips is both the safe answer and the one
// the author meant.
func TestNormalizeDerivesTheStatusFromTheWorstTrip(t *testing.T) {
	r := Result{Trips: []threshold.Trip{
		{Rule: "a", Severity: threshold.Warn},
		{Rule: "b", Severity: threshold.Crit},
		{Rule: "c", Severity: threshold.Warn},
	}}.Normalize()
	if r.Status != StatusCrit {
		t.Errorf("status = %s, want crit (the worst trip)", r.Status)
	}
	if got := (Result{}).Normalize().Status; got != StatusOK {
		t.Errorf("a result with no trips and no status = %s, want ok", got)
	}
	// An explicit status is never overwritten: unavailable with no trips is a
	// perfectly ordinary answer and must not become ok.
	if got := (Result{Status: StatusUnavailable}).Normalize().Status; got != StatusUnavailable {
		t.Errorf("an explicit status became %s", got)
	}
}

// Every field present even at zero: "crit": 0 is what lets a dashboard draw a green
// panel, and an absent key would make it draw nothing.
func TestCountsAlwaysCarryEveryStatus(t *testing.T) {
	blob, err := json.Marshal(Counts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range allStatuses {
		if !strings.Contains(string(blob), `"`+string(s)+`":0`) {
			t.Errorf("an empty tally omits %s:\n%s", s, blob)
		}
	}
}

func TestTallyCountsEveryStatusAndTakesTheWorst(t *testing.T) {
	var results []Result
	for _, s := range allStatuses {
		results = append(results, Result{Status: s}, Result{Status: s})
	}
	counts, worst := Tally(results)
	want := Counts{OK: 2, Warn: 2, Crit: 2, Skipped: 2, Unavailable: 2, Error: 2}
	if counts != want {
		t.Errorf("counts = %+v, want %+v", counts, want)
	}
	if worst != StatusCrit {
		t.Errorf("worst = %s, want crit", worst)
	}

	// An unrecognised status is counted as an error rather than dropped: a result
	// that appears in checks[] and in no count would make the two disagree.
	counts, _ = Tally([]Result{{Status: "degraded"}})
	if counts.Error != 1 {
		t.Errorf("an unrecognised status was not counted: %+v", counts)
	}

	// A deliberately narrow --check is not a broken run. "No checks selected" is
	// the runner's own condition to report, not a status.
	counts, worst = Tally(nil)
	if counts != (Counts{}) || worst != StatusOK {
		t.Errorf("an empty run = %+v / %s, want a zero tally and ok", counts, worst)
	}
}

// Checks run concurrently and finish in whatever order they finish in. Without a
// sort the report would differ between consecutive runs on an unchanged host,
// which is the property the byte-stability test asserts.
func TestSortResultsIsByID(t *testing.T) {
	results := []Result{{ID: "net.ladder"}, {ID: "disk.usage"}, {ID: "disk.inodes"}}
	SortResults(results)
	want := []ID{"disk.inodes", "disk.usage", "net.ladder"}
	for i, id := range want {
		if results[i].ID != id {
			t.Errorf("results[%d] = %s, want %s", i, results[i].ID, id)
		}
	}
}

// Progress is promised non-nil, and Discard is what a caller that wants none is
// handed. A check emitting into it must not panic and must not be slowed by a
// nil check it should not have to write.
func TestDiscardSwallowsEvents(t *testing.T) {
	Discard.Emit(Event{Text: "anything", Status: StatusOK})

	var got []Event
	var e Emitter = EmitterFunc(func(ev Event) { got = append(got, ev) })
	e.Emit(Event{Text: "one", Status: StatusWarn})
	if len(got) != 1 || got[0].Text != "one" || got[0].Status != StatusWarn {
		t.Errorf("EmitterFunc did not pass the event through: %+v", got)
	}
}

// The runner applies every spec default before Run, so a check reading a parameter
// it declared always gets a value. Reading one it did not declare is a bug in the
// check, and an empty string is the answer that makes it show up as an empty field
// rather than a panic in a cron job.
func TestParamReadsWhatTheRunnerApplied(t *testing.T) {
	in := Input{Params: map[string]string{"path": "/var/log"}}
	if got := in.Param("path"); got != "/var/log" {
		t.Errorf("Param(path) = %q", got)
	}
	if got := in.Param("nope"); got != "" {
		t.Errorf("Param on an undeclared name = %q, want empty", got)
	}
	if got := (Input{}).Param("path"); got != "" {
		t.Errorf("Param on a nil map = %q, want empty", got)
	}
}

// Text is never markup. v1's ProbeOutput.verdict carried Textual markup, which is
// why it could not be reused headlessly and why network.py had to escape all tool
// output. An Event carrying only text and a status is what makes the ladder one
// implementation instead of two.
func TestAnEventCarriesNoMarkup(t *testing.T) {
	blob, err := json.Marshal(Event{Text: "[1] 10.0.0.1  1.2 ms", Status: StatusOK})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"text":"[1] 10.0.0.1  1.2 ms","status":"ok"}`
	if string(blob) != want {
		t.Errorf("marshalled event = %s, want %s", blob, want)
	}
}

// No omitempty anywhere: an absent key and a null key are different documents to a
// consumer. A zero-valued Result must still carry every key in checks[].
func TestNoFieldIsEverOmitted(t *testing.T) {
	blob, err := json.Marshal(Result{}.Normalize())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "status", "summary", "trips", "metrics", "data", "steps", "error"} {
		if !strings.Contains(string(blob), `"`+key+`":`) {
			t.Errorf("a zero result omits %q:\n%s", key, blob)
		}
	}
	// Duration is deliberately not on the wire: the envelope carries one
	// duration_ms, and a per-check time would make every consecutive run differ.
	if strings.Contains(string(blob), "duration") {
		t.Errorf("a per-check duration reached the wire:\n%s", blob)
	}
}

func TestAMetricCarriesEveryKeyAPromScrapeNeeds(t *testing.T) {
	m := Metric{
		Name: "disk.used_pct", Value: 82.5, Unit: "%", Kind: Gauge,
		Labels: map[string]string{"mount": "/"}, Help: "filesystem capacity used",
	}
	blob, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"disk.used_pct","value":82.5,"unit":"%","labels":{"mount":"/"},"kind":"gauge","help":"filesystem capacity used"}`
	if string(blob) != want {
		t.Errorf("marshalled metric =\n%s\nwant\n%s", blob, want)
	}
}

// Reusing a retired id for a different measurement is the worst kind of breaking
// change, because nothing errors: the operator's alert keeps firing on a name that
// now means something else.
func TestRetiredIsEmptyUntilSomethingIsRetired(t *testing.T) {
	for id, reason := range Retired {
		if reason == "" {
			t.Errorf("%s is retired with no reason, so nobody can tell what it used to mean", id)
		}
		if !validID(string(id)) {
			t.Errorf("%s is not a well-formed id", id)
		}
	}
}
