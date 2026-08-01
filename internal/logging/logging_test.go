package logging

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{in: "debug", want: slog.LevelDebug},
		{in: "info", want: slog.LevelInfo},
		{in: "warn", want: slog.LevelWarn},
		{in: "error", want: slog.LevelError},
		// Typed by people, so accepted.
		{in: "warning", want: slog.LevelWarn},
		{in: "WARN", want: slog.LevelWarn},
		{in: "  Debug  ", want: slog.LevelDebug},
		// Empty means "flag not given", so a caller passes it through unconditionally.
		{in: "", want: DefaultLevel},
		{in: "   ", want: DefaultLevel},
		{in: "verbose", wantErr: true},
		{in: "trace", wantErr: true},
		{in: "9", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseLevel(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseLevel(%q) = %v, want an error", tt.in, got)
				}
				// The message has to say what is acceptable, or the operator's next
				// move is another guess.
				for _, name := range LevelNames() {
					if !strings.Contains(err.Error(), name) {
						t.Errorf("error %q does not list the valid value %q", err, name)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLevel(%q) errored: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestDefaultLevelIsWarn(t *testing.T) {
	// Pinned deliberately. INFO by default would put lines on stderr for every cron
	// run of `check --json`, and the operator would learn to redirect stderr to
	// /dev/null -- losing the warnings too.
	if DefaultLevel != slog.LevelWarn {
		t.Errorf("DefaultLevel = %v, want WARN", DefaultLevel)
	}
}

func TestLevelNamesIsSortedAndComplete(t *testing.T) {
	got := LevelNames()
	want := []string{"debug", "error", "info", "warn", "warning"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("LevelNames() = %v, want %v", got, want)
	}
}

func TestDefaultsToStderr(t *testing.T) {
	var buf bytes.Buffer
	h, err := Init(Options{Stderr: &buf})
	if err != nil {
		t.Fatalf("Init errored: %v", err)
	}
	defer closeHandle(t, h)

	if h.Destination != DestStderr {
		t.Errorf("Destination = %q, want %q", h.Destination, DestStderr)
	}
	h.Logger.Warn("disk nearly full", "mount", "/var")
	if !strings.Contains(buf.String(), "disk nearly full") {
		t.Errorf("stderr = %q, want the record", buf.String())
	}
}

func TestBelowLevelRecordsAreDropped(t *testing.T) {
	var buf bytes.Buffer
	h, err := Init(Options{Stderr: &buf})
	if err != nil {
		t.Fatalf("Init errored: %v", err)
	}
	defer closeHandle(t, h)

	h.Logger.Info("scan finished")
	h.Logger.Debug("probe argv")
	if buf.Len() != 0 {
		t.Errorf("stderr = %q, want nothing below WARN", buf.String())
	}
	h.Logger.Error("collector failed")
	if !strings.Contains(buf.String(), "collector failed") {
		t.Errorf("stderr = %q, want the ERROR record", buf.String())
	}
}

func TestSourceLocationsOnlyAtDebug(t *testing.T) {
	var debugBuf, warnBuf bytes.Buffer

	dh, err := Init(Options{Level: "debug", Stderr: &debugBuf})
	if err != nil {
		t.Fatalf("Init errored: %v", err)
	}
	dh.Logger.Debug("probe argv")
	closeHandle(t, dh)
	if !strings.Contains(debugBuf.String(), "source=") {
		t.Errorf("debug output = %q, want a source location", debugBuf.String())
	}

	wh, err := Init(Options{Level: "warn", Stderr: &warnBuf})
	if err != nil {
		t.Fatalf("Init errored: %v", err)
	}
	wh.Logger.Warn("disk nearly full")
	closeHandle(t, wh)
	// Noise at the levels an operator actually reads.
	if strings.Contains(warnBuf.String(), "source=") {
		t.Errorf("warn output = %q, want no source location", warnBuf.String())
	}
}

func TestFileDestinationTakesPrecedenceOverStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleetfix.log")
	var buf bytes.Buffer

	h, err := Init(Options{File: path, Stderr: &buf})
	if err != nil {
		t.Fatalf("Init errored: %v", err)
	}
	h.Logger.Warn("wrote to the file")
	closeHandle(t, h)

	if h.Destination != path {
		t.Errorf("Destination = %q, want %q", h.Destination, path)
	}
	if buf.Len() != 0 {
		t.Errorf("stderr = %q, want nothing once a file is configured", buf.String())
	}
	if got := readFile(t, path); !strings.Contains(got, "wrote to the file") {
		t.Errorf("log file = %q, want the record", got)
	}
}

func TestFileIsAppendedNotTruncated(t *testing.T) {
	// A restarting agent must not erase the history that explains why it restarted.
	path := filepath.Join(t.TempDir(), "fleetfix.log")
	if err := os.WriteFile(path, []byte("earlier run\n"), 0o600); err != nil {
		t.Fatalf("seeding the file failed: %v", err)
	}

	h, err := Init(Options{File: path})
	if err != nil {
		t.Fatalf("Init errored: %v", err)
	}
	h.Logger.Warn("later run")
	closeHandle(t, h)

	got := readFile(t, path)
	if !strings.Contains(got, "earlier run") {
		t.Errorf("log file = %q, want the earlier content kept", got)
	}
	if !strings.Contains(got, "later run") {
		t.Errorf("log file = %q, want the new record", got)
	}
}

func TestANewFileIsNotWorldReadable(t *testing.T) {
	// A diagnostic line can carry paths and command lines from other users'
	// processes.
	path := filepath.Join(t.TempDir(), "fleetfix.log")
	h, err := Init(Options{File: path})
	if err != nil {
		t.Fatalf("Init errored: %v", err)
	}
	closeHandle(t, h)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %#o, want 0600", perm)
	}
}

func TestAnUnopenableFileIsAnErrorNotASilentDiscard(t *testing.T) {
	// The failure mode this prevents: a typo in --log-file, a logger that writes
	// nowhere, and an operator wondering where their diagnostics went.
	path := filepath.Join(t.TempDir(), "no-such-dir", "fleetfix.log")

	h, err := Init(Options{File: path})
	if err == nil {
		closeHandle(t, h)
		t.Fatal("Init accepted an unopenable log file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path", err)
	}
	if h != nil {
		t.Error("Init returned a Handle alongside an error")
	}
}

func TestABadLevelIsRejectedBeforeAnyFileIsCreated(t *testing.T) {
	// Level parsing comes first so a usage error stays a usage error and leaves
	// nothing behind on disk.
	path := filepath.Join(t.TempDir(), "fleetfix.log")

	if _, err := Init(Options{Level: "verbose", File: path}); err == nil {
		t.Fatal("Init accepted an unknown level")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a log file was created despite the usage error")
	}
}

func TestATUIWithNoFileWritesNowhere(t *testing.T) {
	// tcell draws by absolute cursor position, so a line on stderr does not scroll
	// past -- it scribbles over the interface and stays there. A missing diagnostic
	// is the better failure, and --log-file is the way out.
	var buf bytes.Buffer
	h, err := Init(Options{TUIAttached: true, Stderr: &buf})
	if err != nil {
		t.Fatalf("Init errored: %v", err)
	}
	defer closeHandle(t, h)

	h.Logger.Error("something went wrong")
	if buf.Len() != 0 {
		t.Errorf("stderr = %q, want nothing while a TUI owns the terminal", buf.String())
	}
	if h.Destination != DestDiscarded {
		t.Errorf("Destination = %q, want %q", h.Destination, DestDiscarded)
	}
}

func TestATUIWithAFileStillLogsToTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleetfix.log")
	var buf bytes.Buffer

	h, err := Init(Options{TUIAttached: true, File: path, Stderr: &buf})
	if err != nil {
		t.Fatalf("Init errored: %v", err)
	}
	h.Logger.Warn("visible in the file")
	closeHandle(t, h)

	if buf.Len() != 0 {
		t.Errorf("stderr = %q, want nothing while a TUI owns the terminal", buf.String())
	}
	if got := readFile(t, path); !strings.Contains(got, "visible in the file") {
		t.Errorf("log file = %q, want the record", got)
	}
}

func TestInitInstallsTheDefaultLogger(t *testing.T) {
	// The TUI case is why this matters: any package reaching for slog.Default() --
	// ours or a dependency's -- would otherwise write to the real stderr and corrupt
	// the interface.
	var buf bytes.Buffer
	h, err := Init(Options{TUIAttached: true, Stderr: &buf})
	if err != nil {
		t.Fatalf("Init errored: %v", err)
	}
	defer closeHandle(t, h)

	slog.Error("a stray record from somewhere else")
	if buf.Len() != 0 {
		t.Errorf("stderr = %q, want the default logger routed away from the terminal", buf.String())
	}

	var fileBuf bytes.Buffer
	h2, err := Init(Options{Level: "info", Stderr: &fileBuf})
	if err != nil {
		t.Fatalf("Init errored: %v", err)
	}
	defer closeHandle(t, h2)
	slog.Info("routed through the default")
	if !strings.Contains(fileBuf.String(), "routed through the default") {
		t.Errorf("output = %q, want the default logger to reach the new handler", fileBuf.String())
	}
}

func TestCloseIsSafeWithoutAFileAndWhenRepeated(t *testing.T) {
	h, err := Init(Options{Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("Init errored: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("Close with no file errored: %v", err)
	}

	path := filepath.Join(t.TempDir(), "fleetfix.log")
	fh, err := Init(Options{File: path})
	if err != nil {
		t.Fatalf("Init errored: %v", err)
	}
	if err := fh.Close(); err != nil {
		t.Errorf("first Close errored: %v", err)
	}
	// Idempotent so a caller can both defer Close and close early on a path that
	// needs the file flushed before it exits.
	if err := fh.Close(); err != nil {
		t.Errorf("second Close errored: %v", err)
	}

	var nilHandle *Handle
	if err := nilHandle.Close(); err != nil {
		t.Errorf("Close on a nil Handle errored: %v", err)
	}
}

// TestNothingInThisPackageTouchesStdout enforces the rule that makes
// `check --json` safe to pipe: JSON on stdout, everything else on stderr, always.
// A single stray os.Stdout or fmt.Println here would put a diagnostic line inside a
// consumer's JSON, and a cron job's parser would break on a host nobody is watching.
//
// Checked against the AST rather than by review, because "we don't do that" is
// exactly the kind of invariant that stops being true quietly.
func TestNothingInThisPackageTouchesStdout(t *testing.T) {
	// Globbed and parsed file by file rather than with parser.ParseDir, which is
	// deprecated for not honouring build tags. Build tags are also the reason a glob
	// is not quite equivalent -- a file excluded by a tag is still inspected here.
	// For a guard that must not have holes, inspecting one file too many is the
	// error to make.
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing the package failed: %v", err)
	}

	// Bare Print/Printf/Println on the fmt package write to stdout.
	stdoutFuncs := map[string]bool{"Print": true, "Printf": true, "Println": true}
	fset := token.NewFileSet()
	checked := 0

	for _, name := range names {
		// Test files are exempt: they legitimately print diagnostics.
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s failed: %v", name, err)
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			where := fset.Position(sel.Pos())
			if ident.Name == "os" && sel.Sel.Name == "Stdout" {
				t.Errorf("%s: os.Stdout is referenced; stdout belongs to check --json", where)
			}
			if ident.Name == "fmt" && stdoutFuncs[sel.Sel.Name] {
				t.Errorf("%s: fmt.%s writes to stdout; use the logger", where, sel.Sel.Name)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("inspected no files; the guard would pass vacuously")
	}
	t.Logf("checked %d non-test file(s)", checked)
}

func closeHandle(t *testing.T, h *Handle) {
	t.Helper()
	if err := h.Close(); err != nil {
		t.Errorf("Close errored: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s failed: %v", path, err)
	}
	return string(b)
}
