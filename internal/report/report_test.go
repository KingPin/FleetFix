package report

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// at is a fixed instant, so a golden compares the whole document rather than
// everything except the parts that move.
var at = time.Date(2026, 7, 31, 22, 14, 5, 987_654_321, time.UTC)

func meta() Meta {
	return Meta{
		Version:     "2.0.0",
		GeneratedAt: at,
		Duration:    1843 * time.Millisecond,
		Host: Host{
			Hostname: "host-01",
			OS:       "linux",
			Arch:     "amd64",
			Kernel:   "6.1.0-18-amd64",
			Distro:   "Debian GNU/Linux 12 (bookworm)",
			BootID:   "2f1a6c7e-0b1d-4a3f-9c2e-8d5b7a4f1e60",
		},
		Operator:  Operator{UnixUser: "opsuser", SourceIP: "203.0.113.7"},
		Privilege: Privilege{UID: 1000, CanTier2: true},
	}
}

func results() []check.Result {
	warn := check.Result{
		ID:      "disk.usage",
		Status:  check.StatusWarn,
		Summary: "/ is 88% full",
		Trips: []threshold.Trip{{
			Rule: threshold.DiskUsedPct, Status: "warn",
			Value: 88, Bound: 85, Unit: "%", Subject: "/",
		}},
		Metrics: []check.Metric{{
			Name: "fleetfix_disk_used_pct", Value: 88, Unit: "%",
			Labels: map[string]string{"mount": "/"},
			Kind:   check.Gauge, Help: "Percentage of the filesystem in use.",
		}},
		Data:  map[string]any{"mount": "/", "used_pct": 88},
		Steps: []check.Event{{Text: "read /proc/mounts", Status: check.StatusOK}},
	}
	return []check.Result{
		{ID: "net.ladder", Status: check.StatusOK, Summary: "every rung answered"},
		warn,
	}
}

// A consumer pins on the schema string, so it is a value in the contract and not
// a detail that tracks the binary. A 2.4.0 that added a check still emits v1.
func TestTheSchemaIsNotTheBinaryVersion(t *testing.T) {
	rep := New(meta(), nil)
	if rep.Schema != "fleetfix.check/v1" {
		t.Errorf("schema = %q", rep.Schema)
	}
	if rep.Schema == rep.FleetFixVersion {
		t.Error("the schema tracks the binary version, so a patch release breaks every pinned consumer")
	}
}

// One place decides what a set of results means, so status, exit_code and counts
// cannot disagree with each other inside one document.
func TestTheVerdictIsDerivedAndInternallyConsistent(t *testing.T) {
	for name, tc := range map[string]struct {
		results []check.Result
		status  check.Status
		code    int
	}{
		"nothing ran":       {nil, check.StatusOK, 0},
		"all clear":         {[]check.Result{{ID: "a.one", Status: check.StatusOK}}, check.StatusOK, 0},
		"a warning":         {results(), check.StatusWarn, 1},
		"an absent tool":    {[]check.Result{{ID: "a.one", Status: check.StatusUnavailable}}, check.StatusUnavailable, 0},
		"a broken checker":  {[]check.Result{{ID: "a.one", Status: check.StatusError}}, check.StatusError, 3},
		"crit beats a warn": {[]check.Result{{ID: "a.one", Status: check.StatusWarn}, {ID: "b.two", Status: check.StatusCrit}}, check.StatusCrit, 2},
	} {
		t.Run(name, func(t *testing.T) {
			rep := New(meta(), tc.results)
			if rep.Status != tc.status {
				t.Errorf("status = %s, want %s", rep.Status, tc.status)
			}
			if rep.ExitCode != tc.code {
				t.Errorf("exit_code = %d, want %d", rep.ExitCode, tc.code)
			}
			total := rep.Counts.OK + rep.Counts.Warn + rep.Counts.Crit +
				rep.Counts.Skipped + rep.Counts.Unavailable + rep.Counts.Error
			if total != len(tc.results) {
				t.Errorf("counts sum to %d for %d checks", total, len(tc.results))
			}
		})
	}
}

// Two runs against the same host state must produce the same bytes, or every
// consumer diffing yesterday's report against today's sees noise. Ordering is the
// only thing that can drift here, because a runner returns checks as they finish.
func TestTheDocumentIsByteStableAcrossRuns(t *testing.T) {
	forward := New(meta(), results())

	shuffled := results()
	shuffled[0], shuffled[1] = shuffled[1], shuffled[0]
	reversed := New(meta(), shuffled)

	var a, b bytes.Buffer
	if err := WriteJSON(&a, forward); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(&b, reversed); err != nil {
		t.Fatal(err)
	}
	if a.String() != b.String() {
		t.Errorf("completion order changed the document:\n%s\n---\n%s", a.String(), b.String())
	}
}

// The two wire rules, asserted on the marshalled bytes rather than on the structs:
// this is what the consumer sees, and it is where the difference shows up.
func TestNoKeyIsAbsentAndNoSliceIsNull(t *testing.T) {
	// The emptiest report there is -- no checks, no warnings, no identity.
	var doc map[string]any
	raw, err := json.Marshal(New(Meta{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"schema", "fleetfix_version", "generated_at", "duration_ms",
		"host", "operator", "privilege",
		"status", "exit_code", "error", "counts", "checks", "config_warnings",
	}
	for _, key := range want {
		if _, ok := doc[key]; !ok {
			t.Errorf("key %q is absent, and absent is not the same document as null", key)
		}
	}
	if len(doc) != len(want) {
		t.Errorf("the envelope has %d keys, the contract names %d: %v", len(doc), len(want), keys(doc))
	}

	for _, key := range []string{"checks", "config_warnings"} {
		if _, ok := doc[key].([]any); !ok {
			t.Errorf("%s marshalled to %#v, want []", key, doc[key])
		}
	}
	// The nested blocks are objects even when nothing filled them in, because a
	// consumer writing .host.hostname should get "" and not an error.
	for _, key := range []string{"host", "operator", "privilege", "counts"} {
		if _, ok := doc[key].(map[string]any); !ok {
			t.Errorf("%s marshalled to %#v, want an object", key, doc[key])
		}
	}
}

// Every field on every nested wire struct, checked by reflection so a field added
// later cannot quietly ship with an omitempty on it.
func TestNoWireStructUsesOmitempty(t *testing.T) {
	for _, v := range []any{
		Report{},
		Host{},
		Operator{},
		Privilege{},
		check.Result{},
		check.Metric{},
		check.Event{},
		check.Counts{},
		threshold.Trip{},
	} {
		typ := reflect.TypeOf(v)
		for i := range typ.NumField() {
			tag := typ.Field(i).Tag.Get("json")
			if strings.Contains(tag, "omitempty") {
				t.Errorf("%s.%s is tagged %q", typ.Name(), typ.Field(i).Name, tag)
			}
		}
	}
}

// A caller that assembled results some other way -- doctor, a replay, a test --
// must not be able to put a null into checks[].trips.
func TestResultsAreNormalisedEvenWhenTheyDidNotComeFromTheRunner(t *testing.T) {
	rep := New(meta(), []check.Result{{ID: "disk.usage", Status: check.StatusOK}})
	if rep.Checks[0].Trips == nil || rep.Checks[0].Metrics == nil || rep.Checks[0].Steps == nil {
		t.Errorf("a nil slice survived into the document: %+v", rep.Checks[0])
	}
}

// One timestamp format across the tool means one thing for a log pipeline to
// parse. Truncated, not rounded, so it never names a millisecond the run had not
// reached: .987654321 is .987, not .988.
func TestTheTimestampMatchesTheAuditLogsFormat(t *testing.T) {
	if got := Timestamp(at); got != "2026-07-31T22:14:05.987Z" {
		t.Errorf("Timestamp = %q", got)
	}
	// A non-UTC input is converted rather than formatted with the wrong offset
	// stamped as Z.
	east := time.FixedZone("UTC+2", 2*60*60)
	if got := Timestamp(at.In(east)); got != "2026-07-31T22:14:05.987Z" {
		t.Errorf("a non-UTC instant formatted as %q", got)
	}
	if _, err := time.Parse(time.RFC3339, Timestamp(at)); err != nil {
		t.Errorf("the timestamp does not parse as RFC 3339: %v", err)
	}
}

// stdout always carries a valid document of this schema, including when the run
// never happened. A cron job's parser breaking is how a real failure becomes a
// silence nobody investigates.
func TestACatastrophicFailureIsStillAValidDocument(t *testing.T) {
	rep := Failed(meta(), `no check or domain named "dsk"`)

	if rep.Schema != Schema {
		t.Errorf("schema = %q", rep.Schema)
	}
	if rep.Status != check.StatusError || rep.ExitCode != 3 {
		t.Errorf("status = %s, exit = %d, want error and 3", rep.Status, rep.ExitCode)
	}
	if !strings.Contains(rep.Error, "dsk") {
		t.Errorf("error = %q, want the reason", rep.Error)
	}
	// The reason goes in error, not in config_warnings: a selector typo is not a
	// config problem, and conflating them sends the operator to the wrong file.
	if len(rep.ConfigWarnings) != 0 {
		t.Errorf("config_warnings = %v, want empty", rep.ConfigWarnings)
	}
	var doc map[string]any
	if err := json.Unmarshal(mustJSON(t, rep), &doc); err != nil {
		t.Fatalf("the failure report is not valid JSON: %v", err)
	}
}

// A threshold that was ignored for a typo is exactly the thing that makes a fleet
// quietly stop alerting, so it travels in the document rather than on stderr,
// which a cron job discards.
func TestConfigWarningsTravelWithTheReport(t *testing.T) {
	m := meta()
	m.ConfigWarnings = []string{"thresholds.yml: no rule named \"disk.pct\", ignoring it"}
	rep := New(m, nil)
	if len(rep.ConfigWarnings) != 1 || !strings.Contains(rep.ConfigWarnings[0], "disk.pct") {
		t.Errorf("config_warnings = %v", rep.ConfigWarnings)
	}
	// And they do not change the verdict: an ignored override is a warning about
	// the config, not about the host.
	if rep.Status != check.StatusOK {
		t.Errorf("a config warning graded the host %s", rep.Status)
	}
}

// Go escapes <, > and & by default, which would turn an ordinary path or a shell
// redirect in a cmdline into > for the benefit of a browser this never
// reaches.
func TestOutputIsNotHTMLEscaped(t *testing.T) {
	rep := New(meta(), []check.Result{{
		ID: "procs.top", Status: check.StatusOK, Summary: "sh -c 'tail -f x > /dev/null && echo ok'",
	}})
	var buf bytes.Buffer
	if err := WriteJSON(&buf, rep); err != nil {
		t.Fatal(err)
	}
	// The six-character sequences encoding/json writes when escaping is on, not
	// the characters themselves -- the summary is full of those on purpose.
	for _, escape := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if strings.Contains(buf.String(), escape) {
			t.Errorf("the document is HTML-escaped (%s):\n%s", escape, buf.String())
		}
	}
	if !strings.Contains(buf.String(), "> /dev/null && echo ok") {
		t.Errorf("the summary did not survive:\n%s", buf.String())
	}
}

func TestWriteJSONEndsInExactlyOneNewline(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, New(meta(), results())); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	if !strings.HasSuffix(s, "}\n") {
		t.Errorf("the document does not end in a single newline: %q", s[len(s)-8:])
	}
}

// A streaming consumer must be able to route on the envelope before the results
// arrive, which is the entire reason the envelope goes first.
func TestNDJSONLeadsWithTheEnvelopeAndThenOneLinePerCheck(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteNDJSON(&buf, New(meta(), results())); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines for an envelope and two checks:\n%s", len(lines), buf.String())
	}

	var envelope map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &envelope); err != nil {
		t.Fatalf("line 1 is not JSON: %v", err)
	}
	if envelope["schema"] != Schema {
		t.Errorf("line 1 is not the envelope: %v", envelope["schema"])
	}
	// Empty rather than absent, and empty rather than repeated: the checks are the
	// lines that follow, and shipping them twice doubles a fleet's log volume.
	if c, ok := envelope["checks"].([]any); !ok || len(c) != 0 {
		t.Errorf("the envelope carries checks: %#v", envelope["checks"])
	}

	for i, want := range []string{"disk.usage", "net.ladder"} {
		var res map[string]any
		if err := json.Unmarshal([]byte(lines[i+1]), &res); err != nil {
			t.Fatalf("line %d is not JSON: %v", i+2, err)
		}
		if res["id"] != want {
			t.Errorf("line %d is %v, want %s", i+2, res["id"], want)
		}
	}
}

// Every line must stand alone, or a `while read` consumer that splits on newlines
// gets a fragment of a document.
func TestNDJSONNeverWrapsALine(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteNDJSON(&buf, New(meta(), results())); err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		var any1 any
		if err := json.Unmarshal([]byte(line), &any1); err != nil {
			t.Errorf("line %d does not stand alone: %v", i+1, err)
		}
	}
}

// A closed pipe, a full disk, an SSH session that went away mid-write. The caller
// puts this on stderr, so it has to say which part of the document was lost --
// "the envelope" and "check disk.usage" send the operator to different places.
func TestAFailedWriteIsReportedAndSaysWhatWasLost(t *testing.T) {
	rep := New(meta(), results())

	if err := WriteJSON(failAfter(0), rep); err == nil {
		t.Error("WriteJSON swallowed a failed write")
	}
	if err := WriteNDJSON(failAfter(0), rep); err == nil || !strings.Contains(err.Error(), "envelope") {
		t.Errorf("WriteNDJSON on a dead writer = %v, want an error naming the envelope", err)
	}
	// The envelope got out and the first check did not: the operator has a
	// truncated stream and needs to know where it stops.
	err := WriteNDJSON(failAfter(1), rep)
	if err == nil || !strings.Contains(err.Error(), "disk.usage") {
		t.Errorf("a mid-stream failure = %v, want an error naming the check", err)
	}
}

// failAfter accepts n writes and then refuses.
type failWriter struct{ left int }

func failAfter(n int) *failWriter { return &failWriter{left: n} }

func (w *failWriter) Write(p []byte) (int, error) {
	if w.left <= 0 {
		return 0, io.ErrClosedPipe
	}
	w.left--
	return len(p), nil
}

func mustJSON(t *testing.T, rep Report) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteJSON(&buf, rep); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
