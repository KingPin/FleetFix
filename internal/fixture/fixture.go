// Package fixture locates the captured command output under testdata/.
//
// The corpus harvested in M0 is the contract between the two implementations, so
// the Go table tests and the Python oracle must read the same bytes from the same
// files. A test that reaches for them with a relative path works only from its own
// directory and silently changes meaning when the package moves; this resolves
// against the repository root instead, which is fixed.
//
// The root is derived from this file's own compile-time path rather than from the
// working directory. `go test` sets the working directory to the package under
// test, so a "../../testdata" that is correct in internal/core/disk is wrong in
// internal/core/net, and every new package would re-derive the same constant with
// a different number of parents.
//
// Test-only. It imports testing, which registers the -test.* flags into whatever
// binary links it, so importing it from shipping code would put test flags in
// fleetfix. A depguard rule in .golangci.yml enforces that rather than trusting
// the doc comment -- the same posture as the os/exec and tview boundaries.
package fixture

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Root returns the absolute path of the repository root.
func Root() string {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		panic("fixture: runtime.Caller failed; the binary was built without file info")
	}
	// self is <root>/internal/fixture/fixture.go.
	return filepath.Dir(filepath.Dir(filepath.Dir(self)))
}

// Path returns the absolute path of a fixture named relative to testdata/, as the
// manifest names it: fixture.Path("df/usage_mixed.txt").
func Path(rel string) string {
	return filepath.Join(Root(), "testdata", filepath.FromSlash(rel))
}

// Bytes reads a fixture, failing the test if it is missing.
//
// Fatal rather than a returned error on purpose: a fixture that will not load is
// not a test failure to be reported alongside others, it is the test having
// nothing to say. The M0 corpus is checked in, so the only way here is a typo or a
// deletion, and both should stop the run at the point of the mistake.
func Bytes(tb testing.TB, rel string) []byte {
	tb.Helper()
	b, err := read(rel)
	if err != nil {
		tb.Fatalf("fixture %s: %v", rel, err)
	}
	return b
}

// read is split out so the missing-fixture path is testable. testing.TB cannot be
// implemented outside the testing package, so a Fatalf branch reached only through
// a real *testing.T is a branch no test can exercise without failing itself.
func read(rel string) ([]byte, error) {
	return os.ReadFile(Path(rel))
}

// Text reads a fixture as a string.
//
// No trimming, no line-ending normalisation. The captures are byte-for-byte what
// the tool emitted, and a parser that depends on a trailing newline being present
// -- or absent -- is exactly the kind of thing the differential harness exists to
// catch. Handing it anything other than the real bytes would hide that.
func Text(tb testing.TB, rel string) string {
	tb.Helper()
	return string(Bytes(tb, rel))
}
