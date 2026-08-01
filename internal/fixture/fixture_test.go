package fixture

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// The property that matters: Root is the module root, not whichever directory the
// test happened to run in. go.mod is the marker because it is the one file
// guaranteed to sit there and nowhere below it.
func TestRootIsTheModuleRoot(t *testing.T) {
	root := Root()
	if !filepath.IsAbs(root) {
		t.Fatalf("Root() = %q, want an absolute path", root)
	}
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("no go.mod at Root() = %q: %v", root, err)
	}
	if want := "module github.com/KingPin/FleetFix/v2"; !strings.Contains(string(b), want) {
		t.Errorf("go.mod at %q does not declare %q", root, want)
	}
}

func TestPathIsUnderTestdata(t *testing.T) {
	got := Path("df/usage_mixed.txt")
	want := filepath.Join(Root(), "testdata", "df", "usage_mixed.txt")
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

// Slash-separated input regardless of host separator: the manifest names fixtures
// with forward slashes and that spelling has to keep working.
func TestPathAcceptsManifestSpelling(t *testing.T) {
	if _, err := read("proc/net/dev.txt"); err != nil {
		t.Errorf("manifest-style path did not resolve: %v", err)
	}
}

func TestTextReturnsTheBytesUntouched(t *testing.T) {
	got := Text(t, "df/usage_mount_with_spaces.txt")
	want := "Filesystem     1024-blocks      Used Available Capacity Mounted on\n" +
		"/dev/sdd1          1000000    500000    500000      50% /mnt/with space\n"
	if got != want {
		t.Errorf("Text() = %q,\nwant %q", got, want)
	}
}

func TestReadReportsAMissingFixture(t *testing.T) {
	if _, err := read("df/no_such_capture.txt"); err == nil {
		t.Fatal("read() of a missing fixture returned no error")
	}
}

func TestTreeBuildsTheDirectoriesAndFiles(t *testing.T) {
	fsys := Tree(t, "thermal/zones.json")

	got, err := fs.ReadFile(fsys, "thermal_zone0/type")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "x86_pkg_temp\n"; string(got) != want {
		t.Errorf("thermal_zone0/type = %q, want %q", got, want)
	}

	// A string is a file, so a name that holds one must not read as a directory.
	if _, err := fs.ReadDir(fsys, "thermal_zone8"); err == nil {
		t.Error("thermal_zone8 holds a string and read as a directory")
	}

	// And an empty object is a directory that exists and is empty -- the shape a
	// flat name-to-contents map cannot express, and the reason Tree writes the
	// directory entries out rather than letting MapFS synthesise them.
	entries, err := fs.ReadDir(fsys, "thermal_zone11")
	if err != nil {
		t.Fatalf("thermal_zone11 is not a directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("thermal_zone11 holds %d entries, want none", len(entries))
	}
}

// The tree must satisfy fs.FS's own contract, not just answer the reads this
// package's callers happen to make -- fstest.TestFS is what catches a synthesised
// directory disagreeing with its own entries.
func TestTreeIsAValidFS(t *testing.T) {
	fsys := Tree(t, "thermal/zones.json")
	if err := fstest.TestFS(fsys, "thermal_zone0/temp", "thermal_zone8"); err != nil {
		t.Error(err)
	}
}

func TestWalkTreeRejectsAValueThatIsNeither(t *testing.T) {
	err := walkTree(fstest.MapFS{}, "", map[string]any{"thermal_zone0": 42.0})
	if err == nil {
		t.Fatal("walkTree() accepted a number")
	}
	if !strings.Contains(err.Error(), "thermal_zone0") {
		t.Errorf("walkTree() error = %v, want it to name the offending path", err)
	}
}
