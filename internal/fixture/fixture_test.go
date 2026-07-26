package fixture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
