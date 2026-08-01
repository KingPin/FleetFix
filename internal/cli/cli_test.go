package cli

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cli/checkcmd"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/exitcode"
	"github.com/KingPin/FleetFix/v2/internal/report"
	"github.com/KingPin/FleetFix/v2/internal/version"
)

// run invokes Main with captured streams. Nothing in the test suite is allowed to
// let the real os.Stdout leak in, because "stdout carries only JSON" is the claim
// under test and a test that shared the process's stdout could not see a violation.
func run(t *testing.T, argv ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut strings.Builder
	code = Main(argv, &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestVersionPrintsTheV1CompatibleLine(t *testing.T) {
	stdout, stderr, code := run(t, "--version")

	if want := "fleetfix " + version.Version() + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if code != exitcode.OK {
		t.Errorf("exit code = %d, want %d", code, exitcode.OK)
	}
}

// TestVersionLineHasExactlyTwoFields pins the shape the release workflow parses.
// The asserted invariant is "the last whitespace-separated field is the version",
// which is what lets the workflow compare `--version` against the git tag minus its
// "v". A friendlier line with the build date in it would break that comparison.
func TestVersionLineHasExactlyTwoFields(t *testing.T) {
	stdout, _, _ := run(t, "--version")

	fields := strings.Fields(strings.TrimSuffix(stdout, "\n"))
	if len(fields) != 2 {
		t.Fatalf("version line %q has %d fields, want 2", stdout, len(fields))
	}
	if fields[0] != "fleetfix" {
		t.Errorf("first field = %q, want %q", fields[0], "fleetfix")
	}
	if strings.HasPrefix(fields[1], "v") {
		t.Errorf("version %q carries a leading v; v1.6.0 printed a bare version and the release compares against the tag minus its v", fields[1])
	}
}

func TestHelpGoesToStdoutAndSucceeds(t *testing.T) {
	for _, argv := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			stdout, stderr, code := run(t, argv...)

			if code != exitcode.OK {
				t.Errorf("exit code = %d, want %d", code, exitcode.OK)
			}
			if !strings.Contains(stdout, "Usage: fleetfix") {
				t.Errorf("stdout does not carry the usage text: %q", stdout)
			}
			if !strings.Contains(stdout, "check") {
				t.Errorf("usage does not mention the check command: %q", stdout)
			}
			// Help was asked for, so it is output, not a diagnostic.
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
		})
	}
}

func TestUsageErrorsExitUnknownAndWriteNothingToStdout(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string // a substring stderr must carry
	}{
		{"unknown flag", []string{"--nope"}, "nope"},
		{"unknown command", []string{"frobnicate"}, "unknown command"},
		{"no arguments", nil, "no interactive UI"},
		{"bad log level", []string{"--log-level", "verbose", "check"}, "unknown level"},
		{"check with a stray argument", []string{"check", "extra"}, "unexpected argument"},
		{"check with json disabled", []string{"check", "--json=false"}, "only output mode"},
		{"check with an unknown flag", []string{"check", "--prom"}, "prom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := run(t, tc.argv...)

			// Unknown, not critical. Exiting 2 on a typo -- which is what v1's argparse
			// did -- would page someone about a critical host that is fine.
			if code != exitcode.Unknown {
				t.Errorf("exit code = %d, want %d (unknown)", code, exitcode.Unknown)
			}
			// The hard part of the contract: a consumer's parser sees an empty stdout
			// and a non-zero code, never a half-written document.
			if stdout != "" {
				t.Errorf("stdout = %q, want empty on a usage error", stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tc.want)
			}
		})
	}
}

// decode insists stdout is exactly one report and returns it. Decoded from the
// whole of stdout, not searched within it: a trailing log line or a leading
// banner makes this fail, which is the point.
func decode(t *testing.T, stdout string) report.Report {
	t.Helper()
	var rep report.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	return rep
}

func TestCheckWritesOnlyJSONToStdout(t *testing.T) {
	stdout, stderr, code := run(t, "check", "--json")

	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d: this build registers no checks, so the state is unknown", code, exitcode.Unknown)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty at the default log level", stderr)
	}

	rep := decode(t, stdout)
	if rep.Schema != report.Schema {
		t.Errorf("schema = %q, want %q", rep.Schema, report.Schema)
	}
	// error, not unknown: "unknown" is not one of the six statuses, and the run
	// did not fail to reach a verdict -- it failed to happen at all.
	if rep.Status != check.StatusError {
		t.Errorf("status = %q, want %q", rep.Status, check.StatusError)
	}
	if rep.ExitCode != exitcode.Unknown {
		t.Errorf("exit_code in the document = %d, want %d", rep.ExitCode, exitcode.Unknown)
	}
	if rep.Error == "" {
		t.Error("a failed run carries no error; the document says nothing about why it is empty")
	}
	if rep.FleetFixVersion != version.Version() {
		t.Errorf("fleetfix_version = %q, want %q", rep.FleetFixVersion, version.Version())
	}
	if !strings.HasSuffix(stdout, "\n") {
		t.Error("stdout does not end in a newline; a line-oriented consumer would block")
	}
}

// TestCheckIsTheDefaultOutputMode covers the bare `fleetfix check`, which is what an
// operator types and what --ndjson and the coming --prom must not change the
// meaning of.
//
// Compared after dropping the two keys that describe the invocation rather than
// the host, which is also the byte-stability property M3 has to hold: two runs
// over an unchanged host differ in when they ran and nothing else.
func TestCheckIsTheDefaultOutputMode(t *testing.T) {
	withFlag, _, codeA := run(t, "check", "--json")
	bare, _, codeB := run(t, "check")

	if a, b := redact(t, bare), redact(t, withFlag); a != b {
		t.Errorf("`check` and `check --json` differ:\n%s\n%s", a, b)
	}
	if codeA != codeB {
		t.Errorf("exit codes differ: %d vs %d", codeA, codeB)
	}
}

// redact drops generated_at and duration_ms. Dropped rather than blanked, so a
// key that stopped being emitted at all would still show up as a difference.
func redact(t *testing.T, stdout string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	delete(doc, "generated_at")
	delete(doc, "duration_ms")
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestChecksIsAnEmptyArrayNotNull guards the []-versus-null distinction at the byte
// level. A nil slice marshals to null, which is a different document to a consumer
// doing `for c in doc["checks"]`, and Go gives no hint that it happened.
func TestChecksIsAnEmptyArrayNotNull(t *testing.T) {
	stdout, _, _ := run(t, "check")

	if !strings.Contains(stdout, `"checks": []`) {
		t.Errorf("checks is not an empty array; a nil slice marshals to null:\n%s", stdout)
	}
	if strings.Contains(stdout, "null") {
		t.Errorf("the document contains a null:\n%s", stdout)
	}
}

func TestNDJSONPutsTheEnvelopeOnItsOwnLine(t *testing.T) {
	stdout, stderr, code := run(t, "check", "--ndjson")

	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d; the format must not change the verdict", code, exitcode.Unknown)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want just the envelope on a build with no checks:\n%s", len(lines), stdout)
	}
	if rep := decode(t, lines[0]); rep.Schema != report.Schema {
		t.Errorf("the line is not the envelope: %q", lines[0])
	}
	// Compact, or a line-oriented reader would see one object as several records.
	if strings.Contains(stdout, "\n  ") {
		t.Errorf("the ndjson output is indented:\n%s", stdout)
	}
}

func TestTwoOutputModesAtOnceIsAUsageError(t *testing.T) {
	stdout, stderr, code := run(t, "check", "--json", "--ndjson")

	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty: neither mode was chosen", stdout)
	}
	if !strings.Contains(stderr, "pick one") {
		t.Errorf("stderr = %q, want it to say the two modes conflict", stderr)
	}
}

// --ndjson alone is not a conflict: --json defaults to true, so refusing the
// pair on the flag's value rather than on whether it was typed would make
// --ndjson impossible to use.
func TestNDJSONAloneIsNotAConflict(t *testing.T) {
	_, stderr, code := run(t, "check", "--ndjson")

	if strings.Contains(stderr, "pick one") {
		t.Errorf("--ndjson on its own was refused: %q", stderr)
	}
	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
}

func TestListPrintsWhatTheBuildCanCheck(t *testing.T) {
	stdout, stderr, code := run(t, "check", "--list")

	if code != exitcode.OK {
		t.Errorf("exit code = %d, want 0: --list says nothing about the host", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	var listing checkcmd.Listing
	if err := json.Unmarshal([]byte(stdout), &listing); err != nil {
		t.Fatalf("the listing is not a single JSON document: %v\n%s", err, stdout)
	}
	if listing.Schema != checkcmd.ListSchema {
		t.Errorf("schema = %q, want %q", listing.Schema, checkcmd.ListSchema)
	}
	if listing.FleetFixVersion != version.Version() {
		t.Errorf("fleetfix_version = %q, want %q", listing.FleetFixVersion, version.Version())
	}
}

// A selector naming nothing is loud and still a document: a warning here would
// run nothing, find nothing wrong and exit 0, which is a permanently green host.
func TestAnUnmatchedSelectorIsReportedInTheDocument(t *testing.T) {
	stdout, stderr, code := run(t, "check", "--check", "dsk.usage")

	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty: the complaint belongs in the document", stderr)
	}
	rep := decode(t, stdout)
	if !strings.Contains(rep.Error, "dsk.usage") {
		t.Errorf("error = %q, want it to quote the selector", rep.Error)
	}
}

func TestExitZeroReachesTheRun(t *testing.T) {
	stdout, _, code := run(t, "check", "--exit-zero")

	if code != exitcode.OK {
		t.Errorf("exit code = %d, want 0", code)
	}
	if rep := decode(t, stdout); rep.ExitCode != exitcode.Unknown {
		t.Errorf("document exit_code = %d, want %d: the verdict belongs in the document", rep.ExitCode, exitcode.Unknown)
	}
}

// A malformed pair is refused rather than ignored: an operator who typed
// `--param path /var/log` meant to inspect something, and a run that quietly
// skipped the check for want of a parameter it was given would be the least
// helpful possible answer.
func TestAMalformedParamIsAUsageError(t *testing.T) {
	for _, arg := range []string{"path", "=/var/log", " =x"} {
		t.Run(arg, func(t *testing.T) {
			stdout, stderr, code := run(t, "check", "--param", arg)

			if code != exitcode.Unknown {
				t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "K=V") {
				t.Errorf("stderr = %q, want it to state the expected form", stderr)
			}
		})
	}
}

func TestWellFormedParamsAreAccepted(t *testing.T) {
	stdout, stderr, _ := run(t, "check", "--param", "path=/var/log", "--param", "depth=2")

	if strings.Contains(stderr, "K=V") {
		t.Errorf("a well-formed pair was refused: %q", stderr)
	}
	// An empty value is a value: `--param path=` says "the operator supplied
	// nothing", which the runner already distinguishes from not asking.
	if _, _, code := run(t, "check", "--param", "path="); code != exitcode.Unknown {
		t.Errorf("exit code = %d, want the empty-registry answer %d", code, exitcode.Unknown)
	}
	decode(t, stdout)
}

// A repeatable flag that also splits on commas, because both spellings are how
// this gets used: a person types --check twice, an Ansible task templates one
// string. Accepting only one of them would silently do nothing useful with the
// other.
func TestSelectorsAreRepeatableAndCommaSeparated(t *testing.T) {
	cases := []struct {
		name string
		set  []string
		want []string
	}{
		{"repeated", []string{"disk", "net"}, []string{"disk", "net"}},
		{"comma separated", []string{"disk,net"}, []string{"disk", "net"}},
		{"both, with the whitespace an operator leaves in", []string{"disk, net", "docker"}, []string{"disk", "net", "docker"}},
		{"empty parts are dropped rather than matched", []string{"disk,,"}, []string{"disk"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var list stringList
			for _, v := range tc.set {
				if err := list.Set(v); err != nil {
					t.Fatalf("Set(%q) errored: %v", v, err)
				}
			}
			if strings.Join(list, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("selectors = %v, want %v", []string(list), tc.want)
			}
			if got := list.String(); got != strings.Join(tc.want, ",") {
				t.Errorf("String() = %q, want %q", got, strings.Join(tc.want, ","))
			}
		})
	}
}

// TestDebugLoggingNeverContaminatesStdout is the reason the logging package is
// wired the way it is. At the noisiest level the diagnostics still land on stderr
// and stdout still decodes as one document.
func TestDebugLoggingNeverContaminatesStdout(t *testing.T) {
	stdout, stderr, code := run(t, "--log-level", "debug", "check", "--json")

	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
	var rep report.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("debug logging contaminated stdout: %v\n%s", err, stdout)
	}
	if rep.Schema != report.Schema {
		t.Errorf("schema = %q, want %q", rep.Schema, report.Schema)
	}
	if !strings.Contains(stderr, "starting") {
		t.Errorf("stderr carries no debug record: %q", stderr)
	}
	if !strings.Contains(stderr, "log_destination=stderr") {
		t.Errorf("the debug record does not report its destination: %q", stderr)
	}
}

func TestLogFileTakesTheDiagnosticsOffStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleetfix.log")

	stdout, stderr, code := run(t, "--log-level", "debug", "--log-file", path, "check")

	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty with --log-file set", stderr)
	}
	decode(t, stdout)

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log file failed: %v", err)
	}
	if !strings.Contains(string(b), "starting") {
		t.Errorf("the log file carries no debug record: %q", b)
	}
}

func TestAnUnopenableLogFileIsAUsageError(t *testing.T) {
	// A directory that does not exist, which is what a typo'd --log-file looks like.
	path := filepath.Join(t.TempDir(), "no-such-dir", "fleetfix.log")

	stdout, stderr, code := run(t, "--log-file", path, "check")

	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
	// Refused before the check ran, so no half-answer reaches a consumer.
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, path) {
		t.Errorf("stderr does not name the unusable path: %q", stderr)
	}
}

// TestBadLogLevelIsRejectedBeforeAnyWork asserts the ordering, not just the code:
// the level is validated before the report is produced, so an operator learns about
// the typo immediately rather than after a probe ladder has spent its budget.
func TestBadLogLevelIsRejectedBeforeAnyWork(t *testing.T) {
	stdout, stderr, _ := run(t, "--log-level", "trace", "check", "--json")

	if stdout != "" {
		t.Errorf("stdout = %q, want empty: the check ran despite an invalid level", stdout)
	}
	if !strings.Contains(stderr, "trace") {
		t.Errorf("stderr does not quote the rejected level: %q", stderr)
	}
	for _, name := range []string{"debug", "info", "warn", "error"} {
		if !strings.Contains(stderr, name) {
			t.Errorf("stderr does not list %q as a valid level: %q", name, stderr)
		}
	}
}

// TestAnUnwritableStdoutIsReportedOnStderr covers `fleetfix check --json | head -1`:
// the consumer closes the pipe, the write fails, and the failure has to surface on
// the one stream that is still open. Reporting it on stdout would be both impossible
// and, if it worked, a violation of the contract it is complaining about.
func TestAnUnwritableStdoutIsReportedOnStderr(t *testing.T) {
	var stderr strings.Builder
	code := Main([]string{"check", "--json"}, failingWriter{err: errors.New("broken pipe")}, &stderr)

	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
	if !strings.Contains(stderr.String(), "broken pipe") {
		t.Errorf("stderr does not report the failed write: %q", stderr.String())
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestCheckHelpGoesToStdout(t *testing.T) {
	stdout, stderr, code := run(t, "check", "--help")

	if code != exitcode.OK {
		t.Errorf("exit code = %d, want %d", code, exitcode.OK)
	}
	if !strings.Contains(stdout, "Usage: fleetfix check") {
		t.Errorf("stdout does not carry the subcommand usage: %q", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

// TestNothingInThisPackageNamesTheProcessStreams keeps stream ownership at
// cmd/fleetfix. The moment a subcommand reaches for os.Stdout directly, the
// end-to-end assertions above stop being able to see what a consumer receives --
// they would capture an injected buffer while the real bytes went somewhere else.
func TestNothingInThisPackageNamesTheProcessStreams(t *testing.T) {
	banned := map[string]string{
		"Stdout": "stdout belongs to the report; write through the injected io.Writer",
		"Stderr": "write diagnostics through the injected io.Writer",
	}
	stdoutFuncs := map[string]bool{"Print": true, "Printf": true, "Println": true}

	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing the package failed: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0

	for _, name := range names {
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
			if ident.Name == "os" {
				if why, bad := banned[sel.Sel.Name]; bad {
					t.Errorf("%s: os.%s is referenced; %s", where, sel.Sel.Name, why)
				}
			}
			if ident.Name == "fmt" && stdoutFuncs[sel.Sel.Name] {
				t.Errorf("%s: fmt.%s writes to the process stdout; use Fprint* on the caller's writer", where, sel.Sel.Name)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("inspected no files; the guard would pass vacuously")
	}
}

// TestTheRealBinaryHonoursTheContract builds cmd/fleetfix and runs it, because
// everything above tests Main's return value while a consumer sees a process exit
// status. os.Exit is the one line no in-process test can cover, and getting it
// wrong -- exiting 0 on an unknown result -- is exactly the failure that would let
// a cron job report a host healthy.
//
// Built and run through internal/cmdrun rather than os/exec: depguard denies
// os/exec outside that package, in test files included, and dogfooding the seam
// here is the point of having it.
func TestTheRealBinaryHonoursTheContract(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skipping the build-and-run end-to-end check")
	}

	bin := filepath.Join(t.TempDir(), "fleetfix")
	runner := cmdrun.New()
	ctx := t.Context()

	build, err := runner.Run(ctx, "go", "build", "-o", bin, "../../cmd/fleetfix")
	if cmdrun.IsNotFound(err) {
		t.Skip("no go toolchain on PATH")
	}
	if err != nil {
		t.Fatalf("building the binary failed: %v", err)
	}
	if !build.OK() {
		t.Fatalf("go build exited %d: %s", build.ExitCode, build.Combined())
	}

	t.Run("version exits zero", func(t *testing.T) {
		res, err := runner.Run(ctx, bin, "--version")
		if err != nil {
			t.Fatalf("running the binary failed: %v", err)
		}
		if res.ExitCode != exitcode.OK {
			t.Errorf("exit status = %d, want %d", res.ExitCode, exitcode.OK)
		}
		if !strings.HasPrefix(res.Stdout, "fleetfix ") {
			t.Errorf("stdout = %q", res.Stdout)
		}
	})

	t.Run("check exits unknown with json on stdout", func(t *testing.T) {
		res, err := runner.Run(ctx, bin, "check", "--json")
		if err != nil {
			t.Fatalf("running the binary failed: %v", err)
		}
		if res.ExitCode != exitcode.Unknown {
			t.Errorf("exit status = %d, want %d", res.ExitCode, exitcode.Unknown)
		}
		if res.Stderr != "" {
			t.Errorf("stderr = %q, want empty", res.Stderr)
		}
		var rep report.Report
		if err := json.Unmarshal([]byte(res.Stdout), &rep); err != nil {
			t.Fatalf("the process's stdout is not a single JSON document: %v\n%s", err, res.Stdout)
		}
		if rep.Schema != report.Schema {
			t.Errorf("schema = %q, want %q", rep.Schema, report.Schema)
		}
	})

	t.Run("a usage error exits unknown with an empty stdout", func(t *testing.T) {
		res, err := runner.Run(ctx, bin, "--nope")
		if err != nil {
			t.Fatalf("running the binary failed: %v", err)
		}
		if res.ExitCode != exitcode.Unknown {
			t.Errorf("exit status = %d, want %d", res.ExitCode, exitcode.Unknown)
		}
		if res.Stdout != "" {
			t.Errorf("stdout = %q, want empty", res.Stdout)
		}
	})
}
