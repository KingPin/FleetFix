package logsqueeze

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// errFS refuses every open with one error, which is how the three ways a root
// can fail are staged: gone, denied, and broken.
type errFS struct{ err error }

func (e errFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: e.err}
}

// A host with three oversized logs among the ordinary contents of /var/log.
func staged() map[string]int {
	return map[string]int{
		"syslog":                   4000, // not a log by name, which is v1's answer
		"syslog.log":               3000,
		"nginx/access.log":         5000,
		"nginx/access.log.1":       2000,
		"nginx/access.log.2.gz":    9000, // already compressed
		"apt/history.log":          10,   // under the floor
		"journal/system.journal":   8000, // binary, and not a name this matches
		"nginx/access.log.partial": 7000,
	}
}

func TestCandidatesFindsTheOversizedUncompressedLogs(t *testing.T) {
	res, _ := run(t, source(staged(), 1000))

	if res.Status != check.StatusOK {
		t.Fatalf("status = %s, want ok: %+v", res.Status, res)
	}
	want := []string{
		"/var/log/nginx/access.log",
		"/var/log/syslog.log",
		"/var/log/nginx/access.log.1",
	}
	if got := paths(t, res); !equal(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
	if got := metricNamed(t, res, BytesMetric).Value; got != 10000 {
		t.Errorf("%s = %v, want 10000", BytesMetric, got)
	}
	if got := metricNamed(t, res, FilesMetric).Value; got != 3 {
		t.Errorf("%s = %v, want 3", FilesMetric, got)
	}
	if got := metricNamed(t, res, LargestMetric).Value; got != 5000 {
		t.Errorf("%s = %v, want 5000", LargestMetric, got)
	}
	want0 := "3 uncompressed logs holding 9.8 KB, largest is /var/log/nginx/access.log at 4.9 KB"
	if res.Summary != want0 {
		t.Errorf("summary = %q, want %q", res.Summary, want0)
	}
}

// This check grades nothing -- there is no rule for it and disk.usage already
// grades the fullness a big log contributes to -- so it never trips and never
// downgrades the host.
func TestCandidatesGradeNothing(t *testing.T) {
	res, steps := run(t, source(staged(), 1000))

	if len(res.Trips) != 0 {
		t.Errorf("trips = %+v; this check grades nothing", res.Trips)
	}
	for _, s := range steps {
		if s.Status != check.StatusOK {
			t.Errorf("step %q has status %s, want ok", s.Text, s.Status)
		}
	}
}

// And it says so with an empty policy, rather than reporting a rule it needs.
// Nothing here reads Thresholds, so a Runner wired with no rules at all still
// gets the same answer.
func TestCandidatesNeedNoGradingPolicy(t *testing.T) {
	src := source(staged(), 1000)
	res := Checks(src)[0].Run(t.Context(), check.Input{
		Params:     map[string]string{},
		Progress:   check.Discard,
		Thresholds: threshold.Set{},
	}).Normalize()

	if res.Status != check.StatusOK {
		t.Errorf("status = %s with no rules loaded, want ok: %+v", res.Status, res)
	}
}

// Largest-first, with the path breaking ties. v1 leaned on Python's stable sort
// and therefore on the order the kernel handed the directory back in, so two
// equally-sized logs could swap places between runs of an unchanged host and
// cost the document its byte-stability.
func TestEquallySizedLogsAreOrderedByPath(t *testing.T) {
	res, _ := run(t, source(map[string]int{
		"c.log": 2000,
		"a.log": 2000,
		"b.log": 2000,
	}, 1000))

	want := []string{"/var/log/a.log", "/var/log/b.log", "/var/log/c.log"}
	if got := paths(t, res); !equal(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
}

// A symlink is skipped rather than followed: squeezing through one would replace
// the target and leave the link dangling. Directories are skipped whatever they
// are called.
func TestSymlinksAndDirectoriesAreNotCandidates(t *testing.T) {
	fsys := fstest.MapFS{
		"real.log":         &fstest.MapFile{Data: make([]byte, 3000)},
		"link.log":         &fstest.MapFile{Data: []byte("real.log"), Mode: fs.ModeSymlink},
		"old.log.d/keep":   &fstest.MapFile{Data: make([]byte, 3000)},
		"old.log.d/in.log": &fstest.MapFile{Data: make([]byte, 3000)},
	}

	res, _ := run(t, Source{Root: DefaultRoot, FS: fsys, MinBytes: 1000})

	want := []string{"/var/log/old.log.d/in.log", "/var/log/real.log"}
	if got := paths(t, res); !equal(got, want) {
		t.Errorf("paths = %v, want %v -- the symlink, the directory and the non-log inside it are not candidates", got, want)
	}
}

// A host with nothing over the floor is ok and says so. This is the one status
// in the domain that means "no action here", and it has to be reachable or the
// check is noise on every healthy host.
func TestAHostWithNothingToSqueezeIsOK(t *testing.T) {
	res, steps := run(t, source(map[string]int{"apt/history.log": 10}, 1000))

	if res.Status != check.StatusOK {
		t.Fatalf("status = %s, want ok", res.Status)
	}
	if want := "no uncompressed logs over 1000 B under /var/log"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if len(steps) != 0 {
		t.Errorf("steps = %v, want none", texts(steps))
	}
	// The metrics are still emitted, at zero. An absent series is not the same as a
	// zero one: a dashboard that stopped receiving this cannot tell a clean host
	// from a check that stopped running.
	for _, name := range []string{BytesMetric, FilesMetric, LargestMetric} {
		if got := metricNamed(t, res, name).Value; got != 0 {
			t.Errorf("%s = %v on a clean host, want 0", name, got)
		}
	}
	if found, ok := res.Data.([]Candidate); !ok || found == nil {
		t.Errorf("data = %#v, want an empty slice rather than null", res.Data)
	}
}

// The largest metric carries no label. Labelling it by path would mint a new
// Prometheus series every time the biggest log changed and leave the old one
// stale -- which file it is belongs in the summary and in data[].
func TestNoMetricIsLabelledByPath(t *testing.T) {
	res, _ := run(t, source(staged(), 1000))

	for _, m := range res.Metrics {
		if len(m.Labels) != 0 {
			t.Errorf("%s carries labels %v, want none", m.Name, m.Labels)
		}
	}
}

// A host with more candidates than steps[] should carry says how many it left
// out. Truncating quietly would read as "these are all of them".
func TestTheStepCapNamesWhatItLeftOut(t *testing.T) {
	files := map[string]int{}
	for i := range stepCap + 2 {
		files[fmt.Sprintf("unit%02d.log", i)] = 2000 + i
	}

	res, steps := run(t, source(files, 1000))

	if got := metricNamed(t, res, FilesMetric).Value; got != float64(stepCap+2) {
		t.Errorf("%s = %v, want every candidate counted", FilesMetric, got)
	}
	if len(steps) != stepCap+1 {
		t.Fatalf("steps = %d, want the cap plus the remainder line", len(steps))
	}
	if want := "and 2 more logs over 1000 B"; steps[stepCap].Text != want {
		t.Errorf("last step = %q, want %q", steps[stepCap].Text, want)
	}
	// Every candidate is still in data[], which is where an operator triaging a
	// full disk goes for the whole ladder.
	if got := paths(t, res); len(got) != stepCap+2 {
		t.Errorf("data holds %d candidates, want %d", len(got), stepCap+2)
	}
}

// Exactly at the cap there is nothing left over, so no remainder line.
func TestNoRemainderLineWhenEveryCandidateFits(t *testing.T) {
	files := map[string]int{}
	for i := range stepCap {
		files[fmt.Sprintf("unit%02d.log", i)] = 2000 + i
	}

	_, steps := run(t, source(files, 1000))

	if len(steps) != stepCap {
		t.Fatalf("steps = %d, want exactly the cap", len(steps))
	}
	if strings.Contains(steps[stepCap-1].Text, "more logs") {
		t.Errorf("last step = %q, want a candidate rather than a remainder", steps[stepCap-1].Text)
	}
}

// The steps name the file and its size, in the same rendering the summary uses.
func TestEachCandidateGetsALineNamingItsSize(t *testing.T) {
	_, steps := run(t, source(map[string]int{"nginx/access.log": 5000}, 1000))

	if len(steps) != 1 {
		t.Fatalf("steps = %v, want one", texts(steps))
	}
	if want := "/var/log/nginx/access.log is 4.9 KB"; steps[0].Text != want {
		t.Errorf("step = %q, want %q", steps[0].Text, want)
	}
}

// An unreadable subtree can hold the largest log on the host, so the total is a
// floor rather than a measurement -- and the reader has to be told which they are
// looking at, in the summary as well as in steps[].
func TestAnUnreadableSubtreeIsCountedAndDeclared(t *testing.T) {
	src := Source{
		Root:     DefaultRoot,
		FS:       denyFS{FS: tree(map[string]int{"open.log": 3000, "private/secret.log": 9000}), deny: map[string]bool{"private": true}},
		MinBytes: 1000,
	}

	res, steps := run(t, src)

	if len(steps) == 0 {
		t.Fatal("nothing was said about the unreadable subtree")
	}
	want := "1 path under /var/log could not be read, so this total is a floor"
	if steps[0].Text != want {
		t.Errorf("first step = %q, want %q", steps[0].Text, want)
	}
	if steps[0].Status != check.StatusWarn {
		t.Errorf("the note has status %s, want warn", steps[0].Status)
	}
	if !strings.HasSuffix(res.Summary, "; 1 path could not be read") {
		t.Errorf("summary = %q, want the gap named in it", res.Summary)
	}
	// And the walk carried on: the readable half is still reported.
	if got := paths(t, res); !equal(got, []string{"/var/log/open.log"}) {
		t.Errorf("paths = %v, want the readable log", got)
	}
}

// The status stays ok, which is disk.usage's shape for df's complaint and is
// deliberate: /var/log/private and /var/log/audit refuse an unprivileged reader
// on a stock Debian, so downgrading would put a permanent warn on every non-root
// run and teach an operator to ignore this check.
func TestAnUnreadableSubtreeDoesNotDowngradeTheHost(t *testing.T) {
	src := Source{
		Root:     DefaultRoot,
		FS:       denyFS{FS: tree(map[string]int{"open.log": 3000, "private/secret.log": 9000}), deny: map[string]bool{"private": true}},
		MinBytes: 1000,
	}

	res, _ := run(t, src)

	if res.Status != check.StatusOK {
		t.Errorf("status = %s, want ok", res.Status)
	}
}

// And a host where nothing was found *and* something refused does not read as a
// clean bill of health.
func TestACleanLookingHostSaysWhenItCouldNotLookEverywhere(t *testing.T) {
	src := Source{
		Root:     DefaultRoot,
		FS:       denyFS{FS: tree(map[string]int{"private/secret.log": 9000}), deny: map[string]bool{"private": true}},
		MinBytes: 1000,
	}

	res, _ := run(t, src)

	want := "no uncompressed logs over 1000 B under /var/log; 1 path could not be read"
	if res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
}

// A file that refuses an lstat -- denied, or gone between the readdir and the
// stat -- is counted the same way, and only when its name was one this check
// would have looked at.
func TestAFileThatRefusesAStatIsCountedToo(t *testing.T) {
	src := Source{
		Root: DefaultRoot,
		FS: denyFS{
			FS:   tree(map[string]int{"open.log": 3000, "vanished.log": 9000, "ignored.txt": 9000}),
			deny: map[string]bool{"vanished.log": true, "ignored.txt": true},
		},
		MinBytes: 1000,
	}

	_, steps := run(t, src)

	if len(steps) == 0 {
		t.Fatal("nothing was said about the file that refused")
	}
	// One, not two: the .txt is not a name this check would have read, so its
	// refusal is not a gap in the reading.
	if want := "1 path under /var/log could not be read, so this total is a floor"; steps[0].Text != want {
		t.Errorf("first step = %q, want %q", steps[0].Text, want)
	}
}

// A host that has no /var/log has answered the question. unavailable, not error:
// a scratch container with nothing under it is not a broken machine.
func TestAMissingRootIsUnavailable(t *testing.T) {
	res, _ := run(t, Source{Root: DefaultRoot, FS: errFS{err: fs.ErrNotExist}, MinBytes: 1000})

	if res.Status != check.StatusUnavailable {
		t.Fatalf("status = %s, want unavailable: %+v", res.Status, res)
	}
	if want := "this host has no /var/log"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
}

// Any other refusal from the root is a fault rather than a fact about the host,
// and it is reported with whatever the filesystem said -- there is nothing more
// specific to say about an I/O error on a directory.
func TestAnUnexplainedRootFailureIsAnError(t *testing.T) {
	res, _ := run(t, Source{Root: DefaultRoot, FS: errFS{err: errors.New("input/output error")}, MinBytes: 1000})

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error: %+v", res.Status, res)
	}
	if want := "could not read /var/log"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if !strings.Contains(res.Error, "input/output error") {
		t.Errorf("error = %q, want the filesystem's own words", res.Error)
	}
}

// A Source pointed at a file rather than a directory is a misconfiguration, and
// walking it would report zero logs -- which is indistinguishable from a clean
// host and therefore the wrong answer.
func TestARootThatIsNotADirectoryIsAnError(t *testing.T) {
	src := Source{
		Root:     "/var/log/syslog",
		FS:       fstest.MapFS{".": &fstest.MapFile{Data: []byte("not a directory")}},
		MinBytes: 1000,
	}

	res, _ := run(t, src)

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error: %+v", res.Status, res)
	}
	if want := "/var/log/syslog is not a directory"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
}

// A root this process may not read has not answered, and reporting zero logs
// because we could not look is the permanently-green failure this layer exists
// to prevent.
func TestADeniedRootIsUnavailableRatherThanEmpty(t *testing.T) {
	src := Source{
		Root:     DefaultRoot,
		FS:       denyFS{FS: tree(map[string]int{"open.log": 3000}), deny: map[string]bool{".": true}},
		MinBytes: 1000,
	}

	res, _ := run(t, src)

	if res.Status != check.StatusUnavailable {
		t.Fatalf("status = %s, want unavailable: %+v", res.Status, res)
	}
	if want := "/var/log is not readable by this user"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if len(res.Metrics) != 0 {
		t.Errorf("metrics = %v, want none for a reading that did not happen", res.Metrics)
	}
}

// A walk cut off partway has counted an unknown fraction of the tree, and a
// total that is wrong by an unknown amount is worse than none: an operator
// reading it cannot tell it from the whole answer.
func TestACancelledWalkIsAnErrorRatherThanAPartialTotal(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res := Checks(source(staged(), 1000))[0].Run(ctx, check.Input{
		Params:     map[string]string{},
		Progress:   check.Discard,
		Thresholds: threshold.Defaults(),
	}).Normalize()

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error: %+v", res.Status, res)
	}
	if want := "the walk of /var/log did not finish"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if !strings.Contains(res.Error, "context canceled") {
		t.Errorf("error = %q, want the cancellation named", res.Error)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
