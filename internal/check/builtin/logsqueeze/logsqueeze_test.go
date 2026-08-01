package logsqueeze

import (
	"io/fs"
	"path"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// There is no captured corpus for this domain, and there could not be: what it
// reads is a directory tree rather than a tool's output, and v1 had no parser
// here to pin against. The fixtures are therefore staged filesystems, and the
// tests are about the layer the port adds -- which status, which metrics, and
// what the summary says.

// tree stages a filesystem whose files hold `size` bytes each, named by the map.
// The contents are irrelevant; only the size is read, so the tests set a small
// MinBytes rather than writing ten mebibytes of zeroes into a fixture.
func tree(files map[string]int) fstest.MapFS {
	out := fstest.MapFS{}
	for name, size := range files {
		out[name] = &fstest.MapFile{Data: make([]byte, size)}
	}
	return out
}

// source is a Source over a staged tree, rooted where a real one would be.
func source(files map[string]int, minBytes int64) Source {
	return Source{Root: DefaultRoot, FS: tree(files), MinBytes: minBytes}
}

func run(t *testing.T, src Source) (check.Result, []check.Event) {
	t.Helper()
	checks := Checks(src)
	if len(checks) != 1 {
		t.Fatalf("the domain has %d checks, want 1", len(checks))
	}
	var steps []check.Event
	res := checks[0].Run(t.Context(), check.Input{
		Params:     map[string]string{},
		Progress:   check.EmitterFunc(func(e check.Event) { steps = append(steps, e) }),
		Thresholds: threshold.Defaults(),
	})
	return res.Normalize(), steps
}

func metricNamed(t *testing.T, res check.Result, name string) check.Metric {
	t.Helper()
	for _, m := range res.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no %s metric among %v", name, res.Metrics)
	return check.Metric{}
}

func texts(steps []check.Event) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Text
	}
	return out
}

func paths(t *testing.T, res check.Result) []string {
	t.Helper()
	found, ok := res.Data.([]Candidate)
	if !ok {
		t.Fatalf("data is %T, want []Candidate", res.Data)
	}
	out := make([]string, len(found))
	for i, c := range found {
		out[i] = c.Path
	}
	return out
}

// denyFS refuses named paths, which fstest.MapFS has no way to express and which
// is the shape this check meets on every host it runs on unprivileged.
//
// A denied directory fails at Open, which is how the walk hears about it. A
// denied file fails at Info, which is how a file that vanished mid-walk would.
type denyFS struct {
	fs.FS
	deny map[string]bool
}

func (d denyFS) Open(name string) (fs.File, error) {
	if d.deny[name] {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	f, err := d.FS.Open(name)
	if err != nil {
		return nil, err
	}
	if dir, ok := f.(fs.ReadDirFile); ok {
		return &denyDir{ReadDirFile: dir, at: name, deny: d.deny}, nil
	}
	return f, nil
}

type denyDir struct {
	fs.ReadDirFile
	at   string
	deny map[string]bool
}

func (d *denyDir) ReadDir(n int) ([]fs.DirEntry, error) {
	entries, err := d.ReadDirFile.ReadDir(n)
	for i, e := range entries {
		if d.deny[path.Join(d.at, e.Name())] {
			entries[i] = failInfo{e}
		}
	}
	return entries, err
}

type failInfo struct{ fs.DirEntry }

func (f failInfo) Info() (fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "lstat", Path: f.Name(), Err: fs.ErrPermission}
}

// The domain's shape, asserted once.
func TestTheDomainShipsOneCheck(t *testing.T) {
	checks := Checks(New())

	if len(checks) != 1 {
		t.Fatalf("the domain has %d checks, want 1", len(checks))
	}
	spec := checks[0].Spec()
	if err := spec.Validate(); err != nil {
		t.Errorf("%s: %v", spec.ID, err)
	}
	if spec.ID != CandidatesID {
		t.Errorf("id = %s, want %s", spec.ID, CandidatesID)
	}
	if spec.Domain != "logsqueeze" {
		t.Errorf("domain = %q", spec.Domain)
	}
	if !spec.InDefault {
		t.Error("the check is not in the default run")
	}
	// No subprocess, so nothing to look up -- and nothing that can make this
	// unavailable on a host with a thin userland.
	if len(spec.NeedsBins) != 0 {
		t.Errorf("NeedsBins = %v, want none", spec.NeedsBins)
	}
	if spec.Tier2 {
		t.Error("the check claims Tier 2; it reads directory entries and needs no privilege")
	}
	if spec.Budget != budget {
		t.Errorf("budget = %s, want %s", spec.Budget, budget)
	}
}

// New is the live host: v1's root and v1's floor.
func TestNewReadsVarLogAtV1sFloor(t *testing.T) {
	src := New()

	if src.Root != DefaultRoot {
		t.Errorf("root = %q, want %q", src.Root, DefaultRoot)
	}
	if src.MinBytes != DefaultMinBytes {
		t.Errorf("min = %d, want %d", src.MinBytes, DefaultMinBytes)
	}
	if src.FS != nil {
		t.Error("New pins a filesystem; nil is what makes it the live one")
	}
	if got := src.fsys(); got == nil {
		t.Error("fsys() returned nil for the live host")
	}
}

// A Source built by a struct literal is the shipped behaviour, not a walk that
// reports every empty rotated log on the host.
func TestTheZeroSourceIsTheLiveDefault(t *testing.T) {
	var src Source

	if got := src.root(); got != DefaultRoot {
		t.Errorf("root = %q, want %q", got, DefaultRoot)
	}
	if got := src.minBytes(); got != DefaultMinBytes {
		t.Errorf("min = %d, want %d", got, DefaultMinBytes)
	}
}

// And a caller who really does want every match says so with a negative floor.
func TestANegativeFloorReportsEverythingTheNameTestMatched(t *testing.T) {
	res, _ := run(t, source(map[string]int{"tiny.log": 1}, -1))

	if got := metricNamed(t, res, FilesMetric).Value; got != 1 {
		t.Errorf("%s = %v, want the one-byte log counted", FilesMetric, got)
	}
}

func TestSqueezable(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"syslog.log", true},
		{"syslog.log.1", true},
		{"syslog.log.2026-05-16", true},
		// Already compressed, in all four spellings v1 knew.
		{"syslog.log.gz", false},
		{"syslog.log.xz", false},
		{"syslog.log.zst", false},
		{"syslog.log.bz2", false},
		// This domain's own leftover: the squeeze writes it and unlinks it on
		// failure, and a crash between the two must not leave a file the next scan
		// offers to compress.
		{"syslog.log.gz.partial", false},
		// Not a log at all.
		{"syslog", false},
		{"messages", false},
		{"logfile", false},
		{"catalog", false},
		// journald's own files are binary and not gzip candidates -- and they do not
		// match, which is v1's answer too.
		{"system.journal", false},
	} {
		if got := squeezable(tc.name); got != tc.want {
			t.Errorf("squeezable(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPlural(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{plural(1, "uncompressed log"), "1 uncompressed log"},
		{plural(0, "uncompressed log"), "0 uncompressed logs"},
		{plural(4, "path"), "4 paths"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{10 * 1024 * 1024, "10.0 MB"},
		{3 * 1024 * 1024 * 1024, "3.0 GB"},
		{2 << 50, "2.0 PB"},
	} {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// Every path in the report is absolute, because a relative one is meaningless to
// the operator who has to go and look at the file.
func TestReportedPathsAreRootedAtTheSource(t *testing.T) {
	src := Source{Root: "/srv/logs", FS: tree(map[string]int{"nginx/access.log": 300}), MinBytes: 100}

	res, _ := run(t, src)

	if got := paths(t, res); len(got) != 1 || got[0] != "/srv/logs/nginx/access.log" {
		t.Errorf("paths = %v, want the file under its root", got)
	}
	if !strings.Contains(res.Summary, "/srv/logs/nginx/access.log") {
		t.Errorf("summary = %q, want the rooted path", res.Summary)
	}
}
