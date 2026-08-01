package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/check"
	corestorage "github.com/KingPin/FleetFix/v2/internal/core/storage"
)

// mkfifo makes a named pipe. Not wrapped for portability: the fleet this ships to
// is Linux, and a test that skipped here on every platform would assert nothing
// anywhere.
func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

// dotenv writes a file with the given contents and returns its path.
func dotenv(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func report(t *testing.T, res check.Result) EnvReport {
	t.Helper()
	got, ok := res.Data.(EnvReport)
	if !ok {
		t.Fatalf("data is %T, want EnvReport", res.Data)
	}
	return got
}

func TestACleanFileParsesOK(t *testing.T) {
	path := dotenv(t, "# a comment\nAPP_ENV=production\nexport PORT = 8080\n\n")

	res, steps := runEnv(t, path, "")

	if res.Status != check.StatusOK {
		t.Fatalf("status = %s (%s), want ok", res.Status, res.Summary)
	}
	if got := report(t, res).Keys; !slices.Equal(got, []string{"APP_ENV", "PORT"}) {
		t.Errorf("keys = %v, want [APP_ENV PORT]", got)
	}
	if len(steps) != 0 {
		t.Errorf("steps = %v, want none for a clean file", texts(steps))
	}
	if !strings.Contains(res.Summary, "parsed cleanly: 2 keys") {
		t.Errorf("summary = %q", res.Summary)
	}
}

// The whole point of the redaction, asserted against the marshalled document
// rather than against a field, because what matters is what leaves the process.
func TestNoValueEverReachesTheReport(t *testing.T) {
	const secret = "hunter2-s3cret"
	path := dotenv(t, "DB_PASSWORD="+secret+"\nSTRAY LINE "+secret+"\n")

	res, steps := runEnv(t, path, "DB_PASSWORD")

	encoded, err := json.Marshal(struct {
		Result check.Result  `json:"result"`
		Steps  []check.Event `json:"steps"`
	}{res, steps})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("the value reached the report: %s", encoded)
	}
	// The name is reported, because a name is what an operator needs and is not a
	// credential.
	if got := report(t, res).Keys; !slices.Equal(got, []string{"DB_PASSWORD"}) {
		t.Errorf("keys = %v, want [DB_PASSWORD]", got)
	}
}

func TestAMalformedLineIsAWarn(t *testing.T) {
	path := dotenv(t, "GOOD=1\nthis is not a setting\n")

	res, steps := runEnv(t, path, "")

	// warn, not crit: the consumer of this file may never read the line that is
	// wrong, so a bad line is something to look at rather than evidence of a fault.
	if res.Status != check.StatusWarn {
		t.Fatalf("status = %s (%s), want warn", res.Status, res.Summary)
	}
	issues := report(t, res).Issues
	if len(issues) != 1 || issues[0].LineNo != 2 {
		t.Fatalf("issues = %v, want one on line 2", issues)
	}
	if issues[0].Message != "not in KEY=value form" {
		t.Errorf("message = %q", issues[0].Message)
	}
	if got := texts(steps); !slices.Equal(got, []string{"line 2: not in KEY=value form"}) {
		t.Errorf("steps = %v", got)
	}
}

func TestADuplicateKeyIsAnIssue(t *testing.T) {
	path := dotenv(t, "PORT=1\nPORT=2\n")

	res, _ := runEnv(t, path, "")

	if res.Status != check.StatusWarn {
		t.Fatalf("status = %s, want warn", res.Status)
	}
	issues := report(t, res).Issues
	if len(issues) != 1 || issues[0].Message != "duplicate key: PORT" {
		t.Fatalf("issues = %v, want one duplicate complaint", issues)
	}
	// Reported once, not twice: one key, one name.
	if got := report(t, res).Keys; !slices.Equal(got, []string{"PORT"}) {
		t.Errorf("keys = %v, want [PORT]", got)
	}
}

func TestAMissingRequiredKeyIsCrit(t *testing.T) {
	path := dotenv(t, "APP_ENV=production\n")

	res, steps := runEnv(t, path, "APP_ENV, DB_URL ,SECRET_KEY")

	if res.Status != check.StatusCrit {
		t.Fatalf("status = %s (%s), want crit", res.Status, res.Summary)
	}
	// Split and trimmed, in the order the operator wrote them.
	if got := report(t, res).MissingRequired; !slices.Equal(got, []string{"DB_URL", "SECRET_KEY"}) {
		t.Errorf("missing = %v, want [DB_URL SECRET_KEY]", got)
	}
	if !strings.Contains(res.Summary, "missing 2 required keys: DB_URL, SECRET_KEY") {
		t.Errorf("summary = %q", res.Summary)
	}
	if got := texts(steps); !slices.Equal(got, []string{
		"required key DB_URL is not set",
		"required key SECRET_KEY is not set",
	}) {
		t.Errorf("steps = %v", got)
	}
}

// crit outranks the warn a malformed line would have earned, and the summary
// still says both -- an operator fixing the missing key wants to know there is a
// second thing wrong before they close the ticket.
func TestAMissingKeyOutranksABadLineAndBothAreSaid(t *testing.T) {
	path := dotenv(t, "APP_ENV=production\nnonsense\n")

	res, _ := runEnv(t, path, "DB_URL")

	if res.Status != check.StatusCrit {
		t.Fatalf("status = %s, want crit", res.Status)
	}
	if !strings.Contains(res.Summary, "1 line could not be parsed") {
		t.Errorf("summary = %q, and the bad line is unmentioned", res.Summary)
	}
}

func TestAMissingFileIsCritAndCountsEveryRequiredKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.env")

	res, _ := runEnv(t, path, "APP_ENV,DB_URL")

	// crit rather than unavailable, which is docker.runtime's shape: an operator
	// names a dotenv file because something depends on it, and unavailable exits
	// zero without --strict.
	if res.Status != check.StatusCrit {
		t.Fatalf("status = %s (%s), want crit", res.Status, res.Summary)
	}
	got := report(t, res)
	if got.Exists {
		t.Error("exists is true for a file that is not there")
	}
	if !slices.Equal(got.MissingRequired, []string{"APP_ENV", "DB_URL"}) {
		t.Errorf("missing = %v, want both", got.MissingRequired)
	}
	if !strings.Contains(res.Summary, "there is no file at "+path) {
		t.Errorf("summary = %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "APP_ENV, DB_URL") {
		t.Errorf("summary does not name what depended on it: %q", res.Summary)
	}
}

func TestAMissingFileWithNothingRequiredSaysOnlyThat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.env")

	res, _ := runEnv(t, path, "")

	if res.Summary != "there is no file at "+path {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestADirectoryIsAnError(t *testing.T) {
	res, _ := runEnv(t, t.TempDir(), "")

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error", res.Status)
	}
	if !strings.Contains(res.Summary, "is a directory") {
		t.Errorf("summary = %q", res.Summary)
	}
}

// A fifo is why the type is checked before anything opens the path: os.ReadFile
// on one blocks until somebody writes, and the runner's deadline cuts off the
// result rather than the read.
func TestAFifoIsAnErrorRatherThanAHang(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe.env")
	if err := mkfifo(path); err != nil {
		t.Skipf("cannot make a fifo here: %v", err)
	}

	res, _ := runEnv(t, path, "")

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error", res.Status)
	}
	if !strings.Contains(res.Summary, "not a regular file") {
		t.Errorf("summary = %q", res.Summary)
	}
}

// Stat is answered by the parent directory, so this fails before the file's own
// mode is ever consulted -- a different refusal from the one below, and it has to
// read as a fact about this user rather than as an absent file.
func TestAnUnsearchableParentIsUnavailable(t *testing.T) {
	skipAsRoot(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	res, _ := runEnv(t, path, "")

	if res.Status != check.StatusUnavailable {
		t.Fatalf("status = %s (%s), want unavailable", res.Status, res.Summary)
	}
	if !strings.Contains(res.Summary, "cannot be reached by this user") {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestAPathBeneathAFileIsAnError(t *testing.T) {
	path := filepath.Join(dotenv(t, "A=1\n"), "under.env")

	res, _ := runEnv(t, path, "")

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error", res.Status)
	}
	if !strings.Contains(res.Summary, "could not stat "+path) {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestAnUnreadableFileIsUnavailable(t *testing.T) {
	skipAsRoot(t)
	path := dotenv(t, "APP_ENV=production\n")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	res, _ := runEnv(t, path, "")

	// unavailable, not crit: being refused is a fact about how this invocation was
	// started, not about the file.
	if res.Status != check.StatusUnavailable {
		t.Fatalf("status = %s (%s), want unavailable", res.Status, res.Summary)
	}
	if !strings.Contains(res.Summary, "not readable by this user") {
		t.Errorf("summary = %q", res.Summary)
	}
}

// The other half of the same branch in CheckEnvFile, and the reason it is worth
// one extra open to tell them apart: this one is a fact about the file.
func TestAFileThatIsNotTextIsCrit(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte{'A', '=', 0xff, 0xfe, '\n'}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, _ := runEnv(t, path, "")

	if res.Status != check.StatusCrit {
		t.Fatalf("status = %s (%s), want crit", res.Status, res.Summary)
	}
	if !strings.Contains(res.Summary, "could not be parsed as text") {
		t.Errorf("summary = %q", res.Summary)
	}
	if res.Error == "" {
		t.Error("no error text, so nobody can tell why")
	}
	got := report(t, res)
	if !got.Exists || got.Readable {
		t.Errorf("exists=%v readable=%v, want exists and not readable", got.Exists, got.Readable)
	}
}

func TestTheIssueNarrationIsCapped(t *testing.T) {
	var b strings.Builder
	for i := range 13 {
		b.WriteString("bad line " + strconv.Itoa(i) + "\n")
	}
	path := dotenv(t, b.String())

	res, steps := runEnv(t, path, "")

	if len(steps) != stepCap+1 {
		t.Fatalf("%d steps, want %d", len(steps), stepCap+1)
	}
	if got := steps[stepCap].Text; got != "and 3 more issues" {
		t.Errorf("remainder = %q", got)
	}
	if got := len(report(t, res).Issues); got != 13 {
		t.Errorf("data holds %d issues, want 13", got)
	}
}

func TestTheMetricsAreLabelledByPath(t *testing.T) {
	path := dotenv(t, "A=1\nB=2\nbad\n")

	res, _ := runEnv(t, path, "MISSING")

	for _, m := range res.Metrics {
		if m.Labels["path"] != path {
			t.Errorf("%s is labelled %v, want path=%s", m.Name, m.Labels, path)
		}
	}
	if got := metricNamed(t, res, DotenvKeysMetric).Value; got != 2 {
		t.Errorf("%s = %v, want 2", DotenvKeysMetric, got)
	}
	if got := metricNamed(t, res, DotenvIssuesMetric).Value; got != 1 {
		t.Errorf("%s = %v, want 1", DotenvIssuesMetric, got)
	}
	if got := metricNamed(t, res, DotenvMissingMetric).Value; got != 1 {
		t.Errorf("%s = %v, want 1", DotenvMissingMetric, got)
	}
}

func TestTheDomainGradesNoThresholds(t *testing.T) {
	path := dotenv(t, "A=1\n")

	res, _ := runEnv(t, path, "")

	if len(res.Trips) != 0 {
		t.Errorf("trips = %v, want none: the domain grades boolean", res.Trips)
	}
}

func TestThePathIsRequired(t *testing.T) {
	for _, p := range (env{}).Spec().Params {
		if p.Name == PathParam && !p.Required {
			t.Error("path is optional, so a bare run would parse a file nobody named")
		}
		if p.Name == RequiredKeysParam && p.Required {
			t.Error("required_keys is required, so the check cannot be run without one")
		}
	}
}

// CheckEnvFile never leaves a slice nil today, but data[] does not go through
// Result.Normalize -- nothing downstream would turn a nil into []. The guard is
// what keeps a change on the parser's side from putting null on the wire.
func TestRedactNeverProducesANullSlice(t *testing.T) {
	got := redact(corestorage.EnvCheckResult{})

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "null") {
		t.Errorf("a zero result marshals to %s", encoded)
	}
}

func TestFirstMessageIsEmptyWhenThereIsNothingToSay(t *testing.T) {
	if got := firstMessage(EnvReport{}); got != "" {
		t.Errorf("firstMessage of an empty report = %q", got)
	}
}

func TestSplitKeysDropsTheEmptyPieces(t *testing.T) {
	if got := splitKeys(""); len(got) != 0 {
		t.Errorf("splitKeys(\"\") = %v, want empty", got)
	}
	if got := splitKeys(" A , ,B, "); !slices.Equal(got, []string{"A", "B"}) {
		t.Errorf("splitKeys = %v, want [A B]", got)
	}
}
