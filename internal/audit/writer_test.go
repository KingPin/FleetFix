package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/identity"
)

// A writer whose clock and id generator are fixed, so a test can assert on the
// exact bytes rather than on a shape.
func fixedWriter(t *testing.T, path string) *Writer {
	t.Helper()
	n := 0
	w, err := New(Options{
		Path:     path,
		Operator: identity.Operator{UnixUser: "opsadmin", SourceIP: "10.0.0.9"},
		Version:  "2.0.0",
		Host:     "web-01",
		Now:      func() time.Time { return time.Date(2026, 5, 16, 10, 32, 11, 482_000_000, time.UTC) },
		NewID: func() string {
			n++
			return fmt.Sprintf("id-%d", n)
		},
		SessionID: "sess-1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// The bytes, in full. Every other test in this file asserts on one property;
// this one is the contract, and it is written out so that a change to any field
// name, any separator or any ordering fails here with a diff a reader can see.
func TestEventWritesV1sExactBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	w := fixedWriter(t, path)

	if err := w.Event("fleetfix.launch", Fields{{Key: "mode", Value: "cli"}}); err != nil {
		t.Fatalf("Event: %v", err)
	}

	want := `{"ts": "2026-05-16T10:32:11.482Z", "host": "web-01", "session_id": "sess-1", ` +
		`"call_id": "id-1", "seq": 1, "phase": "event", ` +
		`"operator": {"unix_user": "opsadmin", "auth_principal": null, "source_ip": "10.0.0.9"}, ` +
		`"inspect_target": null, "action": "fleetfix.launch", "target": {"mode": "cli"}, ` +
		`"result": null, "fleetfix_version": "2.0.0"}`
	if got := lines(t, path)[0]; got != want {
		t.Fatalf("record bytes differ\n got: %s\nwant: %s", got, want)
	}
}

// The pair, and the two properties that make it worth writing two lines: the
// intent goes down before the action runs, and both lines carry one call_id.
func TestDoWritesIntentBeforeTheActionAndResultAfter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	w := fixedWriter(t, path)

	var duringAction []string
	err := w.Do("updater.apply", Fields{{Key: "version_to", Value: "2.1.0"}}, func(c *Call) error {
		duringAction = lines(t, path)
		c.SetResult("bytes_installed", int64(9_437_184))
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	if len(duringAction) != 1 {
		t.Fatalf("the trail held %d lines while the action ran; the intent must be down before it starts", len(duringAction))
	}
	if !strings.Contains(duringAction[0], `"phase": "intent"`) {
		t.Fatalf("the line written before the action is not an intent: %s", duringAction[0])
	}

	got := lines(t, path)
	if len(got) != 2 {
		t.Fatalf("want an intent and a result, got %d lines", len(got))
	}
	intent, result := decode(t, got[0]), decode(t, got[1])
	if intent["call_id"] != result["call_id"] {
		t.Fatalf("the pair does not share a call_id: %v vs %v", intent["call_id"], result["call_id"])
	}
	if intent["seq"] == result["seq"] {
		t.Fatalf("both lines carry seq %v; the sequence has to advance", intent["seq"])
	}
	if !strings.Contains(got[1], `"result": {"ok": true, "error": null, "bytes_installed": 9437184}`) {
		t.Fatalf("result payload is wrong: %s", got[1])
	}
}

// A returned error becomes ok=false with the error's own text -- not a Go type
// name in front of it. The installer's messages ("download failed: ...") are
// what an operator reads out of the trail, and v1 wrote them bare.
func TestDoRecordsAReturnedErrorWithoutDecoratingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	w := fixedWriter(t, path)

	want := errors.New("sha256 mismatch (expected abc, got def)")
	if err := w.Do("updater.apply", nil, func(*Call) error { return want }); !errors.Is(err, want) {
		t.Fatalf("Do returned %v, want the action's own error back", err)
	}

	result := lines(t, path)[1]
	if !strings.Contains(result, `"result": {"ok": false, "error": "sha256 mismatch (expected abc, got def)"}`) {
		t.Fatalf("result payload is wrong: %s", result)
	}
}

// v1's installer reports failures with set_result(ok=False, ...) inside a block
// that then returns normally, so the fields override the context manager's
// ok=True *in place*. A port that appended instead would write two "ok" keys.
func TestSetResultOverridesOkAndErrorInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	w := fixedWriter(t, path)

	err := w.Do("updater.apply", nil, func(c *Call) error {
		c.SetResult("ok", false)
		c.SetResult("error", "download failed: connection reset")
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	result := lines(t, path)[1]
	if !strings.Contains(result, `"result": {"ok": false, "error": "download failed: connection reset"}`) {
		t.Fatalf("override did not land in place: %s", result)
	}
	if strings.Count(result, `"ok"`) != 1 {
		t.Fatalf("the record carries more than one ok key: %s", result)
	}
}

// A panic must not swallow the attempt. The trail documents it, then the panic
// carries on -- a recovered panic would turn a bug into a silent wrong answer.
func TestDoRecordsAPanicAndRepanics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	w := fixedWriter(t, path)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("the panic did not propagate past Do")
			}
		}()
		_ = w.Do("procs.kill", nil, func(*Call) error { panic("nil map write") })
	}()

	got := lines(t, path)
	if len(got) != 2 {
		t.Fatalf("want an intent and a result after a panic, got %d lines", len(got))
	}
	if !strings.Contains(got[1], `"error": "panic: nil map write"`) {
		t.Fatalf("the panic was not recorded: %s", got[1])
	}
}

// One sequence per writer, monotonic, with no gaps and no repeats -- under
// concurrency, because the seq is what orders a trail whose timestamps have only
// millisecond resolution.
func TestSeqIsMonotonicUnderConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	w := fixedWriter(t, path)

	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.Event("fleetfix.probe", Fields{{Key: "i", Value: i}})
		}()
	}
	wg.Wait()

	seen := map[float64]bool{}
	got := lines(t, path)
	if len(got) != n {
		t.Fatalf("wrote %d lines, want %d; a line was interleaved or lost", len(got), n)
	}
	for _, line := range got {
		seq, ok := decode(t, line)["seq"].(float64)
		if !ok {
			t.Fatalf("no seq on %s", line)
		}
		if seen[seq] {
			t.Fatalf("seq %v was issued twice", seq)
		}
		seen[seq] = true
	}
	for i := 1; i <= n; i++ {
		if !seen[float64(i)] {
			t.Fatalf("seq %d is missing; the sequence has a gap", i)
		}
	}
}

// The sink is best-effort in both directions: what it does cannot fail a local
// write, and it must not run while the lock is held.
func TestSinkFailureDoesNotFailTheLocalWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	var reported []string
	w, err := New(Options{
		Path:        path,
		Sink:        func(Record) error { return errors.New("collector unreachable") },
		OnSinkError: func(e error) { reported = append(reported, e.Error()) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if werr := w.Event("fleetfix.launch", nil); werr != nil {
		t.Fatalf("a failing sink failed the local write: %v", werr)
	}
	if len(lines(t, path)) != 1 {
		t.Fatal("the record did not reach the local file")
	}
	if len(reported) != 1 || !strings.Contains(reported[0], "collector unreachable") {
		t.Fatalf("the sink failure was not reported: %v", reported)
	}
}

func TestSinkPanicDoesNotFailTheLocalWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	var reported []string
	w, err := New(Options{
		Path:        path,
		Sink:        func(Record) error { panic("exporter closed") },
		OnSinkError: func(e error) { reported = append(reported, e.Error()) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if werr := w.Event("fleetfix.exit", nil); werr != nil {
		t.Fatalf("a panicking sink failed the local write: %v", werr)
	}
	if len(lines(t, path)) != 1 {
		t.Fatal("the record did not reach the local file")
	}
	if len(reported) != 1 || !strings.Contains(reported[0], "exporter closed") {
		t.Fatalf("the sink panic was not reported: %v", reported)
	}
}

// The constraint stated in CLAUDE.md, asserted rather than commented: a slow
// collector must not hold up the next local write. The sink here blocks until a
// second write has already completed, which can only happen if it is running
// outside the lock.
func TestSinkRunsOutsideTheWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	released := make(chan struct{})
	secondDone := make(chan struct{})
	var calls atomic.Int64

	w, err := New(Options{
		Path: path,
		// Only the first record's sink blocks. sync.Once would not do: its Do
		// makes the second caller wait for the first, which is the very
		// serialisation this test is trying to rule out.
		Sink: func(Record) error {
			if calls.Add(1) == 1 {
				<-released
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	go func() {
		_ = w.Event("fleetfix.launch", nil) // blocks in the sink
	}()
	go func() {
		// Give the first write time to reach the sink, then prove the second is
		// not queued behind it.
		time.Sleep(20 * time.Millisecond)
		_ = w.Event("fleetfix.exit", nil)
		close(secondDone)
	}()

	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		close(released)
		t.Fatal("the second write was blocked by the first record's sink; the sink is inside the lock")
	}
	close(released)
}

// New has to fail loudly. A destructive action that discovers its trail is
// unwritable after it has run has already done the thing nobody can now attest
// to, which is why the caller checks this before it acts.
func TestNewRefusesAnUnwritablePath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory is still writable")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := New(Options{Path: filepath.Join(dir, "audit.log")})
	if err == nil {
		t.Fatal("New accepted a path it cannot write")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("error %q does not say the trail is unwritable", err)
	}
}

// The file is created at construction, before anything is recorded, so the
// permission problem surfaces at startup rather than at the first destructive
// action of the day.
func TestNewCreatesTheTrailBeforeAnyRecordExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	if _, err := New(Options{Path: path}); err != nil {
		t.Fatalf("New: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the trail was not created: %v", err)
	}
	if fi.Size() != 0 {
		t.Fatalf("New wrote %d bytes; it must only create the file", fi.Size())
	}
}

// An existing trail is appended to, never truncated. This is the single most
// destructive mistake this package could make, and it would be invisible.
func TestNewAppendsToAnExistingTrail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	const existing = `{"seq": 1, "action": "fleetfix.launch"}` + "\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	w := fixedWriter(t, path)
	if err := w.Event("fleetfix.exit", nil); err != nil {
		t.Fatalf("Event: %v", err)
	}

	got := lines(t, path)
	if len(got) != 2 || got[0] != strings.TrimSuffix(existing, "\n") {
		t.Fatalf("the existing record did not survive: %q", got)
	}
}

// A writer with no explicit session id issues one, and every record it writes
// carries the same one -- that is what joins a launch to its exit.
func TestOneSessionIDSpansEveryRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	w, err := New(Options{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if w.SessionID() == "" {
		t.Fatal("no session id was issued")
	}
	_ = w.Event("fleetfix.launch", nil)
	_ = w.Event("fleetfix.exit", nil)

	got := lines(t, path)
	first, second := decode(t, got[0]), decode(t, got[1])
	if first["session_id"] != w.SessionID() || second["session_id"] != w.SessionID() {
		t.Fatalf("session ids differ: %v, %v, writer says %q", first["session_id"], second["session_id"], w.SessionID())
	}
	if first["call_id"] == second["call_id"] {
		t.Fatal("two events share a call_id; each is its own call")
	}
}

func TestPathReportsWhereTheWriterAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	if got := fixedWriter(t, path).Path(); got != path {
		t.Fatalf("Path() = %q, want %q", got, path)
	}
}

// The inspect target is stamped on every record, not just the ones that touch
// the target's files, so "who was this done to" is answerable from any line.
func TestInspectTargetIsStampedOnEveryRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	w, err := New(Options{Path: path, InspectTarget: "appuser"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = w.Event("fleetfix.launch", nil)
	_ = w.Do("storage.delete", nil, func(*Call) error { return nil })

	for i, line := range lines(t, path) {
		if !strings.Contains(line, `"inspect_target": "appuser"`) {
			t.Fatalf("line %d does not name the inspect target: %s", i, line)
		}
	}
}

// A write failure is not fn's failure. The caller asked whether the action
// worked; "the disk filled while recording that it did" is a different question,
// and it goes to OnSinkError.
func TestDoReturnsTheActionsErrorEvenWhenTheTrailCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the trail stays writable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	w := fixedWriter(t, path)

	var reported []string
	w.opts.OnSinkError = func(e error) { reported = append(reported, e.Error()) }
	// Remove before the chmod, not after: an existing file the process owns stays
	// openable for append inside a read-only directory, so it is the O_CREATE that
	// has to fail.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	sentinel := errors.New("prune refused")
	if err := w.Do("docker.prune", nil, func(*Call) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("Do returned %v, want the action's own error", err)
	}
	if len(reported) == 0 {
		t.Fatal("the write failure was not reported anywhere")
	}
}

func decode(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("record is not JSON: %v\n%s", err, line)
	}
	return m
}

// A UUID has to be unique and has to look like one, because it goes into a
// trail that other tools join on.
func TestNewUUID4IsWellFormedAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id := newUUID4()
		if len(id) != 36 {
			t.Fatalf("uuid %q is %d characters, want 36", id, len(id))
		}
		if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
			t.Fatalf("uuid %q is not hyphenated in the canonical places", id)
		}
		if id[14] != '4' {
			t.Fatalf("uuid %q is not version 4", id)
		}
		if !strings.ContainsRune("89ab", rune(id[19])) {
			t.Fatalf("uuid %q does not carry the RFC 4122 variant", id)
		}
		if seen[id] {
			t.Fatalf("uuid %q was issued twice", id)
		}
		seen[id] = true
	}
}

// v1 built the timestamp from microsecond // 1000, which truncates. Rounding
// would let 999_600 microseconds render as ".1000", which is not a time.
func TestISOMillisTruncatesRatherThanRounds(t *testing.T) {
	got := isoMillis(time.Date(2026, 5, 16, 10, 32, 11, 999_600_000, time.UTC))
	if want := "2026-05-16T10:32:11.999Z"; got != want {
		t.Fatalf("isoMillis = %q, want %q", got, want)
	}
}

func TestISOMillisConvertsToUTC(t *testing.T) {
	east := time.FixedZone("UTC+2", 2*60*60)
	got := isoMillis(time.Date(2026, 5, 16, 12, 32, 11, 482_000_000, east))
	if want := "2026-05-16T10:32:11.482Z"; got != want {
		t.Fatalf("isoMillis = %q, want %q", got, want)
	}
}

// The call id the wrapped function sees is the one in the trail, so an action
// that logs or reports elsewhere can name the record it will be judged by.
func TestCallIDIsTheOneWrittenToTheTrail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	w := fixedWriter(t, path)

	var seen string
	if err := w.Do("docker.prune", nil, func(c *Call) error {
		seen = c.ID()
		return nil
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}

	got := lines(t, path)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want an intent and a result", len(got))
	}
	for i, line := range got {
		if id := decode(t, line)["call_id"]; id != seen {
			t.Errorf("line %d has call_id %v, the call reported %q", i, id, seen)
		}
	}
}
