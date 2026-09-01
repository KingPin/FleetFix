package audit

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// trail writes lines to a file, creating it or appending to it.
func trail(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close() //nolint:errcheck // the write below is what can fail
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
}

// next reads one window and fails the test if it could not be read.
func next(t *testing.T, tl *Tailer) []any {
	t.Helper()
	got, err := tl.Next()
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	return got
}

func TestTheFirstWindowIsTheEndOfTheTrail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	trail(t, path, `{"seq":1}`, `{"seq":2}`, `{"seq":3}`)

	got := next(t, Follow(path, 2))
	if want := `[{"seq":2},{"seq":3}]`; marshal(t, got) != want {
		t.Errorf("got %s, want %s", marshal(t, got), want)
	}
}

func TestASecondWindowIsOnlyWhatWasAppended(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	trail(t, path, `{"seq":1}`, `{"seq":2}`)
	tl := Follow(path, 200)
	next(t, tl)

	trail(t, path, `{"seq":3}`)

	got := next(t, tl)
	if want := `[{"seq":3}]`; marshal(t, got) != want {
		t.Errorf("got %s, want %s", marshal(t, got), want)
	}
}

// The point of the whole type: an idle trail costs a stat and nothing else, where
// v1's read_recent re-read and re-decoded every line on every 2-second tick.
func TestAQuietTrailYieldsNothingRatherThanRepeatingItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	trail(t, path, `{"seq":1}`)
	tl := Follow(path, 200)
	next(t, tl)

	got := next(t, tl)
	if len(got) != 0 {
		t.Errorf("got %s, want no records", marshal(t, got))
	}
	// Not nil, for ReadRecent's reason: this marshals into a report and Python's
	// empty list is [], not null.
	if got == nil {
		t.Error("returned a nil slice; it must marshal as []")
	}
}

// logrotate's `create` mode: the file is moved aside and the writer's next open makes
// a new one at the same path. The offset from the old file would land somewhere
// arbitrary in the new one, so it has to reset.
func TestARotatedTrailIsReadFromItsStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	trail(t, path, `{"seq":1}`, `{"seq":2}`, `{"seq":3}`)
	tl := Follow(path, 200)
	next(t, tl)

	if err := os.Rename(path, filepath.Join(dir, "audit.log.1")); err != nil {
		t.Fatalf("rotating: %v", err)
	}
	trail(t, path, `{"seq":4}`)

	got := next(t, tl)
	if want := `[{"seq":4}]`; marshal(t, got) != want {
		t.Errorf("got %s, want %s", marshal(t, got), want)
	}
}

// logrotate's `copytruncate` mode: same inode, length reset. Nothing but the size
// says the trail restarted.
func TestATruncatedTrailIsReadFromItsStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	trail(t, path, `{"seq":1}`, `{"seq":2}`, `{"seq":3}`)
	tl := Follow(path, 200)
	next(t, tl)

	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncating: %v", err)
	}
	trail(t, path, `{"seq":4}`)

	got := next(t, tl)
	if want := `[{"seq":4}]`; marshal(t, got) != want {
		t.Errorf("got %s, want %s", marshal(t, got), want)
	}
}

// A rotation is a whole new file, so the limit has to bound that window too --
// otherwise the one tick after a rotation renders the entire new trail.
func TestTheLimitBoundsTheWindowAfterARotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	trail(t, path, `{"seq":1}`)
	tl := Follow(path, 2)
	next(t, tl)

	if err := os.Rename(path, filepath.Join(dir, "audit.log.1")); err != nil {
		t.Fatalf("rotating: %v", err)
	}
	trail(t, path, `{"seq":2}`, `{"seq":3}`, `{"seq":4}`)

	got := next(t, tl)
	if want := `[{"seq":3},{"seq":4}]`; marshal(t, got) != want {
		t.Errorf("got %s, want %s", marshal(t, got), want)
	}
}

// A crashed writer leaves a line with no newline. Consuming it would parse it as
// malformed, skip it, and advance past a record that was about to be complete.
func TestAPartialLineIsHeldUntilItsNewlineArrives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(path, []byte(`{"seq":1}`), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	tl := Follow(path, 200)

	if got := next(t, tl); len(got) != 0 {
		t.Fatalf("got %s, want the partial line held back", marshal(t, got))
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatalf("completing the line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	got := next(t, tl)
	if want := `[{"seq":1}]`; marshal(t, got) != want {
		t.Errorf("got %s, want %s", marshal(t, got), want)
	}
}

// The malformed line is skipped, but the offset still moves past it -- a torn record
// must not wedge the reader on the byte it stopped at.
func TestAMalformedLineIsSkippedWithoutStalling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	trail(t, path, `{"seq":1`, `{"seq":2}`)
	tl := Follow(path, 200)

	if want := `[{"seq":2}]`; marshal(t, next(t, tl)) != want {
		t.Errorf("first window: want %s", want)
	}
	trail(t, path, `{"seq":3}`)
	if want := `[{"seq":3}]`; marshal(t, next(t, tl)) != want {
		t.Errorf("second window: want %s", want)
	}
}

// Unlike ReadRecent, which swallows every read failure because v1 did. A reader on a
// timer cannot otherwise tell a quiet trail from one that stopped being readable.
func TestAMissingTrailIsAnErrorRatherThanSilence(t *testing.T) {
	_, err := Follow(filepath.Join(t.TempDir(), "nope.log"), 200).Next()
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("got %v, want a not-exist error", err)
	}
}

func TestAnUnreadableTrailIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0o000 file regardless of its mode")
	}
	path := filepath.Join(t.TempDir(), "audit.log")
	trail(t, path, `{"seq":1}`)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := Follow(path, 200).Next(); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("got %v, want a permission error", err)
	}
}

// A trail that appears after the reader started following it. The first Next fails,
// and the one after must still read the file from its start rather than treating the
// failed attempt as a position it reached.
func TestATrailThatAppearsLaterIsReadWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	tl := Follow(path, 200)
	if _, err := tl.Next(); err == nil {
		t.Fatal("reading a missing trail succeeded")
	}

	trail(t, path, `{"seq":1}`, `{"seq":2}`)

	got := next(t, tl)
	if want := `[{"seq":1},{"seq":2}]`; marshal(t, got) != want {
		t.Errorf("got %s, want %s", marshal(t, got), want)
	}
}

// The write side and the read side, over one file. Two byte-exact records go in
// through the writer and come back out through the tailer with their sequence
// intact, which is the pairing neither package's own tests exercise.
func TestTheTailerReadsWhatTheWriterWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	w, err := New(Options{Path: path, Host: "web-01", SessionID: "sess-1", Version: "2.0.0"})
	if err != nil {
		t.Fatalf("opening the writer: %v", err)
	}
	tl := Follow(path, 200)

	if err := w.Event("fleetfix.launch", Fields{{Key: "mode", Value: "cli"}}); err != nil {
		t.Fatalf("writing the launch event: %v", err)
	}
	got := next(t, tl)
	if len(got) != 1 {
		t.Fatalf("got %s, want the launch event", marshal(t, got))
	}
	if action := got[0].(map[string]any)["action"]; action != "fleetfix.launch" {
		t.Errorf("got action %v, want fleetfix.launch", action)
	}

	if err := w.Do("disk.delete", Fields{{Key: "path", Value: "/srv/x"}}, func(c *Call) error {
		c.SetResult("freed_bytes", 12)
		return nil
	}); err != nil {
		t.Fatalf("writing the action: %v", err)
	}

	got = next(t, tl)
	if len(got) != 2 {
		t.Fatalf("got %s, want the intent and result pair", marshal(t, got))
	}
	for i, want := range []string{PhaseIntent, PhaseResult} {
		if phase := got[i].(map[string]any)["phase"]; phase != want {
			t.Errorf("record %d: got phase %v, want %s", i, phase, want)
		}
	}
}

// A path that names a directory. The open succeeds, so this is not caught by the
// error above it -- it surfaces at the read, which is exactly where an operator who
// pointed paths.yml at a directory would otherwise get an empty trail and no reason.
func TestATrailThatIsADirectoryIsAnError(t *testing.T) {
	if _, err := Follow(t.TempDir(), 200).Next(); err == nil {
		t.Error("reading a directory succeeded")
	}
}
