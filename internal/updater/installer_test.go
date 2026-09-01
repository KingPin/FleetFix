package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/audit"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
)

const binary = "#!/bin/sh\necho the new fleetfix\n"

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// installer stages a release into a writable target directory, with the network
// replaced by two closures serving `binary` and its digest.
func installer(t *testing.T) (*Installer, Release) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "fleetfix")
	return &Installer{
			Target:     target,
			AssetName:  asset,
			StagingDir: t.TempDir(),
			Download: func(_ context.Context, _, dest string) error {
				return os.WriteFile(dest, []byte(binary), 0o600)
			},
			FetchText: func(context.Context, string) (string, error) {
				return digest(binary) + "  " + asset + "\n", nil
			},
		}, Release{
			Tag:         "v2.1.0",
			Version:     "2.1.0",
			AssetURL:    "https://example.invalid/bin",
			ChecksumURL: "https://example.invalid/sum",
		}
}

// trailWriter is a Writer over a file the test can read back.
func trailWriter(t *testing.T) (*audit.Writer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	w, err := audit.New(audit.Options{Path: path})
	if err != nil {
		t.Fatalf("opening the trail: %v", err)
	}
	return w, path
}

// records returns the trail as decoded maps.
func records(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	var out []map[string]any
	for _, rec := range audit.ParseRecords(data, 100) {
		m, ok := rec.(map[string]any)
		if !ok {
			t.Fatalf("record is %T, want an object", rec)
		}
		out = append(out, m)
	}
	return out
}

func TestAVerifiedReleaseReplacesTheBinary(t *testing.T) {
	i, rel := installer(t)
	w, _ := trailWriter(t)

	res, err := i.Apply(t.Context(), w, rel)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Version != "2.1.0" || res.Target != i.Target {
		t.Errorf("got %+v, want 2.1.0 at %s", res, i.Target)
	}
	if res.Bytes != int64(len(binary)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(binary))
	}

	got, err := os.ReadFile(i.Target)
	if err != nil {
		t.Fatalf("reading the target: %v", err)
	}
	if string(got) != binary {
		t.Errorf("got %q, want the downloaded binary", got)
	}
	// World-executable, or the operator installs something they cannot run.
	fi, err := os.Stat(i.Target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode is %v, want 0755", fi.Mode().Perm())
	}
}

// An existing binary is replaced, not written beside. The rename is what makes a
// concurrent exec see either the whole old file or the whole new one.
func TestAnUpdateReplacesTheBinaryThatWasThere(t *testing.T) {
	i, rel := installer(t)
	if err := os.WriteFile(i.Target, []byte("the old fleetfix"), 0o755); err != nil { //nolint:gosec // the fixture is a binary
		t.Fatalf("seeding the target: %v", err)
	}

	if _, err := i.Apply(t.Context(), trailWriterOnly(t), rel); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(i.Target)
	if err != nil {
		t.Fatalf("reading the target: %v", err)
	}
	if string(got) != binary {
		t.Errorf("got %q, want the new binary", got)
	}
	// And nothing was left beside it.
	if _, err := os.Stat(i.Target + ".new"); !os.IsNotExist(err) {
		t.Errorf("%s.new exists (%v); the staging copy was not renamed away", i.Target, err)
	}
}

// The check the whole package exists for. A body that is not what the release
// published must not reach the install target under any circumstance.
func TestAMismatchedDigestLeavesTheBinaryAlone(t *testing.T) {
	i, rel := installer(t)
	if err := os.WriteFile(i.Target, []byte("the old fleetfix"), 0o755); err != nil { //nolint:gosec // the fixture is a binary
		t.Fatalf("seeding the target: %v", err)
	}
	i.Download = func(_ context.Context, _, dest string) error {
		return os.WriteFile(dest, []byte("something else answered the redirect"), 0o600)
	}
	w, path := trailWriter(t)

	_, err := i.Apply(t.Context(), w, rel)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("got %v, want ErrDigestMismatch", err)
	}
	got, err := os.ReadFile(i.Target)
	if err != nil {
		t.Fatalf("reading the target: %v", err)
	}
	if string(got) != "the old fleetfix" {
		t.Errorf("got %q; the running binary was replaced by an unverified download", got)
	}
	// And the refusal is in the trail, not only in the return value.
	last := records(t, path)[1]
	result, _ := last["result"].(map[string]any)
	if ok, _ := result["ok"].(bool); ok {
		t.Errorf("the trail records %v as a success", result)
	}
	if msg, _ := result["error"].(string); !strings.Contains(msg, "does not match") {
		t.Errorf("got %q, want the mismatch in the trail", msg)
	}
}

func TestAChecksumFileWithNoLineForTheAssetIsRefused(t *testing.T) {
	i, rel := installer(t)
	i.FetchText = func(context.Context, string) (string, error) {
		return digest(binary) + "  fleetfix-linux-riscv64\n", nil
	}

	if _, err := i.Apply(t.Context(), trailWriterOnly(t), rel); !errors.Is(err, ErrNoDigest) {
		t.Errorf("got %v, want ErrNoDigest", err)
	}
	if _, err := os.Stat(i.Target); !os.IsNotExist(err) {
		t.Errorf("the target exists (%v); an unverifiable download was installed", err)
	}
}

func trailWriterOnly(t *testing.T) *audit.Writer {
	t.Helper()
	w, _ := trailWriter(t)
	return w
}

func TestAFailedDownloadIsReportedAndInstallsNothing(t *testing.T) {
	i, rel := installer(t)
	i.Download = func(context.Context, string, string) error {
		return errors.New("the mirror answered 502")
	}

	_, err := i.Apply(t.Context(), trailWriterOnly(t), rel)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("got %v, want the mirror's answer", err)
	}
	if _, err := os.Stat(i.Target); !os.IsNotExist(err) {
		t.Errorf("the target exists (%v) after a failed download", err)
	}
}

func TestAFailedChecksumFetchIsReportedAndInstallsNothing(t *testing.T) {
	i, rel := installer(t)
	i.FetchText = func(context.Context, string) (string, error) {
		return "", errors.New("the checksum asset answered 404")
	}

	_, err := i.Apply(t.Context(), trailWriterOnly(t), rel)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("got %v, want the fetch failure", err)
	}
	if _, err := os.Stat(i.Target); !os.IsNotExist(err) {
		t.Errorf("the target exists (%v) after a failed checksum fetch", err)
	}
}

func TestAFailedSwapIsReported(t *testing.T) {
	i, rel := installer(t)
	i.Swap = func(context.Context, string, string) error {
		return errors.New("read-only file system")
	}

	if _, err := i.Apply(t.Context(), trailWriterOnly(t), rel); err == nil ||
		!strings.Contains(err.Error(), "read-only") {
		t.Errorf("got %v, want the swap's failure", err)
	}
}

// A staging directory that cannot be created -- a full or read-only /tmp.
func TestAnUnusableStagingDirectoryIsReported(t *testing.T) {
	i, rel := installer(t)
	i.StagingDir = filepath.Join(t.TempDir(), "does-not-exist")

	if _, err := i.Apply(t.Context(), trailWriterOnly(t), rel); err == nil ||
		!strings.Contains(err.Error(), "stage") {
		t.Errorf("got %v, want the staging failure", err)
	}
}

// The download is a binary sitting in a temporary directory. It must not be left
// there, whether the install worked or not.
func TestTheStagedDownloadIsRemovedEitherWay(t *testing.T) {
	for _, tt := range []struct {
		name   string
		break_ func(*Installer)
	}{
		{"after a successful install", func(*Installer) {}},
		{"after a failed one", func(i *Installer) {
			i.Swap = func(context.Context, string, string) error { return errors.New("no") }
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			i, rel := installer(t)
			tt.break_(i)

			_, _ = i.Apply(t.Context(), trailWriterOnly(t), rel)

			entries, err := os.ReadDir(i.StagingDir)
			if err != nil {
				t.Fatalf("reading the staging directory: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("left %v behind", entries)
			}
		})
	}
}

// The intent line is written before anything is downloaded, so a host that dies
// mid-update still records which version was going over which.
func TestTheTrailNamesBothVersionsAndTheTarget(t *testing.T) {
	i, rel := installer(t)
	w, path := trailWriter(t)

	if _, err := i.Apply(t.Context(), w, rel); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	recs := records(t, path)
	if len(recs) != 2 {
		t.Fatalf("got %d records, want an intent and a result", len(recs))
	}
	intent, result := recs[0], recs[1]
	if intent["phase"] != "intent" || result["phase"] != "result" {
		t.Fatalf("got phases %v and %v", intent["phase"], result["phase"])
	}
	if intent["call_id"] != result["call_id"] {
		t.Errorf("the pair does not share a call_id: %v and %v", intent["call_id"], result["call_id"])
	}
	if intent["action"] != "updater.apply" {
		t.Errorf("action is %v, want updater.apply", intent["action"])
	}

	target, _ := intent["target"].(map[string]any)
	if target["version_to"] != "2.1.0" {
		t.Errorf("version_to is %v, want 2.1.0", target["version_to"])
	}
	// v1 wrote the literal "unknown". This one names what is being replaced.
	if from, _ := target["version_from"].(string); from == "" || from == "unknown" {
		t.Errorf("version_from is %q, want the running version", from)
	}
	if target["target"] != i.Target {
		t.Errorf("target is %v, want %s", target["target"], i.Target)
	}

	res, _ := result["result"].(map[string]any)
	if ok, _ := res["ok"].(bool); !ok {
		t.Errorf("the trail records %v, want a success", res)
	}
	// Rendered rather than type-asserted: the decoder's spelling of an integer is
	// its own business, and the assertion is about the number.
	if got := fmt.Sprint(res["bytes_installed"]); got != fmt.Sprint(len(binary)) {
		t.Errorf("bytes_installed is %v, want %d", got, len(binary))
	}
}

func TestThePrivilegedSwapInstallsThenRenames(t *testing.T) {
	fake := cmdrun.NewFake().
		Stdout("", "sudo", "-n", "install", "-m", "0755", "/stage/new", "/usr/local/bin/fleetfix.new").
		Stdout("", "sudo", "-n", "mv", "-f", "/usr/local/bin/fleetfix.new", "/usr/local/bin/fleetfix")
	i := &Installer{Runner: fake}

	if err := i.sudoSwap(t.Context(), "/stage/new", "/usr/local/bin/fleetfix"); err != nil {
		t.Fatalf("sudoSwap: %v", err)
	}
	if calls := fake.Calls(); len(calls) != 2 {
		t.Errorf("ran %v, want an install and a mv", calls)
	}
}

// The message an operator sees when their sudo timestamp has expired. It has to
// carry sudo's own words, because they say what to do about it.
func TestARefusedSudoReportsWhatSudoSaid(t *testing.T) {
	fake := cmdrun.NewFake().
		Exit(1, "", "sudo: a password is required\n",
			"sudo", "-n", "install", "-m", "0755", "/stage/new", "/bin/fleetfix.new")
	i := &Installer{Runner: fake}

	err := i.sudoSwap(t.Context(), "/stage/new", "/bin/fleetfix")
	if err == nil || !strings.Contains(err.Error(), "a password is required") {
		t.Errorf("got %v, want sudo's message", err)
	}
	// And it stopped there rather than trying to move a file it never made.
	if fake.Called("sudo", "-n", "mv", "-f", "/bin/fleetfix.new", "/bin/fleetfix") {
		t.Error("the rename ran after the install failed")
	}
}

func TestASilentSudoFailureReportsItsExitCode(t *testing.T) {
	fake := cmdrun.NewFake().
		Exit(4, "", "", "sudo", "-n", "install", "-m", "0755", "/stage/new", "/bin/fleetfix.new")
	i := &Installer{Runner: fake}

	err := i.sudoSwap(t.Context(), "/stage/new", "/bin/fleetfix")
	if err == nil || !strings.Contains(err.Error(), "exited 4") {
		t.Errorf("got %v, want the exit code", err)
	}
}

func TestAHostWithNoSudoSaysSo(t *testing.T) {
	fake := cmdrun.NewFake().Missing("sudo", "-n", "install", "-m", "0755", "/stage/new", "/bin/fleetfix.new")
	i := &Installer{Runner: fake}

	err := i.sudoSwap(t.Context(), "/stage/new", "/bin/fleetfix")
	if err == nil || !strings.Contains(err.Error(), "sudo not found") {
		t.Errorf("got %v, want the absent sudo", err)
	}
}

func TestASudoThatCouldNotRunIsReported(t *testing.T) {
	fake := cmdrun.NewFake().
		Fail(errors.New("fork/exec: resource temporarily unavailable"),
			"sudo", "-n", "install", "-m", "0755", "/stage/new", "/bin/fleetfix.new")
	i := &Installer{Runner: fake}

	err := i.sudoSwap(t.Context(), "/stage/new", "/bin/fleetfix")
	if err == nil || !strings.Contains(err.Error(), "resource temporarily unavailable") {
		t.Errorf("got %v, want the reason sudo would not run", err)
	}
}

// A root-owned directory takes the privileged path even though the target file
// itself is missing -- what a first install onto /usr/local/bin looks like.
func TestAnUnwritableTargetDirectoryTakesThePrivilegedPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory regardless of its mode")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	target := filepath.Join(dir, "fleetfix")
	staged := filepath.Join(t.TempDir(), "new")
	if err := os.WriteFile(staged, []byte(binary), 0o600); err != nil {
		t.Fatalf("staging: %v", err)
	}
	fake := cmdrun.NewFake().
		Stdout("", "sudo", "-n", "install", "-m", "0755", staged, target+".new").
		Stdout("", "sudo", "-n", "mv", "-f", target+".new", target)
	i := &Installer{Runner: fake}

	if err := i.swap(t.Context(), staged, target); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if len(fake.Calls()) != 2 {
		t.Errorf("ran %v, want the privileged pair", fake.Calls())
	}
}

func TestCanWriteDirectlyAsksAboutTheDirectory(t *testing.T) {
	dir := t.TempDir()
	// A root-owned binary in a directory the operator owns is replaceable, because
	// the rename is a directory operation.
	target := filepath.Join(dir, "fleetfix")
	if err := os.WriteFile(target, []byte("x"), 0o444); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if !CanWriteDirectly(target) {
		t.Error("a writable directory holding a read-only file is still replaceable")
	}
	// A path whose parent is not a directory at all.
	if CanWriteDirectly(filepath.Join(target, "fleetfix")) {
		t.Error("a file is not a directory to install into")
	}
	if CanWriteDirectly(filepath.Join(dir, "nope", "fleetfix")) {
		t.Error("a directory that is not there is not writable")
	}
}

func TestHaveWritableTargetNeedsEitherWriteAccessOrSudo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory regardless of its mode")
	}
	writable := filepath.Join(t.TempDir(), "fleetfix")
	if !HaveWritableTarget(writable, cmdrun.NewFakeLooker()) {
		t.Error("a writable directory needs no sudo")
	}

	locked := t.TempDir()
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	target := filepath.Join(locked, "fleetfix")
	if HaveWritableTarget(target, cmdrun.NewFakeLooker()) {
		t.Error("a locked directory with no sudo has nowhere to install")
	}
	if !HaveWritableTarget(target, cmdrun.NewFakeLooker("sudo")) {
		t.Error("a locked directory with sudo can still be escalated into")
	}
	// A directory that does not exist is not somewhere sudo can help with either.
	if HaveWritableTarget(filepath.Join(locked, "nope", "fleetfix"), cmdrun.NewFakeLooker("sudo")) {
		t.Error("sudo cannot install into a directory that is not there")
	}
}

// The nil Looker is the production path, and it consults the real PATH.
func TestHaveWritableTargetDefaultsToTheRealPATH(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory regardless of its mode")
	}
	writable := filepath.Join(t.TempDir(), "fleetfix")
	if !HaveWritableTarget(writable, nil) {
		t.Error("a writable directory is writable whatever is on PATH")
	}

	// A locked directory is the case that has to ask PATH. Whether this machine
	// has sudo is not the assertion -- that the answer follows from it is.
	locked := t.TempDir()
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err := cmdrun.NewPATH().Look("sudo")
	want := err == nil
	if got := HaveWritableTarget(filepath.Join(locked, "fleetfix"), nil); got != want {
		t.Errorf("got %v for a locked directory, want %v (sudo on PATH: %v)", got, want, want)
	}
}

// The test binary, which is what os.Executable reports here.
func TestTheInstallTargetIsTheRunningBinary(t *testing.T) {
	got, err := InstallTarget()
	if err != nil {
		t.Fatalf("InstallTarget: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("got %q, want an absolute path", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("got %q, which is not a file: %v", got, err)
	}
}

// Apply with no Target resolves one rather than refusing. It cannot succeed here --
// the running test binary is not something to overwrite -- so the swap is a seam.
func TestApplyWithNoTargetInstallsOverTheRunningBinary(t *testing.T) {
	i, rel := installer(t)
	i.Target = ""
	var swapped string
	i.Swap = func(_ context.Context, _, target string) error {
		swapped = target
		return nil
	}

	res, err := i.Apply(t.Context(), trailWriterOnly(t), rel)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want, _ := InstallTarget()
	if swapped != want || res.Target != want {
		t.Errorf("installed over %q and reported %q, want %q", swapped, res.Target, want)
	}
}

func TestSHA256FileHashesWhatIsThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte(binary), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	got, err := sha256File(path)
	if err != nil {
		t.Fatalf("sha256File: %v", err)
	}
	if got != digest(binary) {
		t.Errorf("got %s, want %s", got, digest(binary))
	}
	if _, err := sha256File(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a file that is not there has no digest")
	}
}

func TestCopyExecutableReportsAnUnreadableSource(t *testing.T) {
	dir := t.TempDir()
	if err := copyExecutable(filepath.Join(dir, "absent"), filepath.Join(dir, "out")); err == nil {
		t.Error("a source that is not there is not copyable")
	}
	// A directory opens and then will not read, which is the failure that happens
	// after the destination has already been created.
	if err := copyExecutable(dir, filepath.Join(dir, "out")); err == nil {
		t.Error("a directory is not an executable to copy")
	}
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte(binary), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if err := copyExecutable(src, filepath.Join(dir, "nope", "out")); err == nil {
		t.Error("a destination directory that is not there is not writable")
	}
}

// A staging copy that cannot be made must not leave the target half-replaced.
func TestAnInPlaceSwapThatCannotCopyLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "fleetfix")
	if err := swapInPlace(filepath.Join(dir, "absent"), target); err == nil {
		t.Fatal("a staged file that is not there cannot be installed")
	}
	if _, err := os.Stat(target + ".new"); !os.IsNotExist(err) {
		t.Errorf("%s.new exists (%v)", target, err)
	}
}

// The copy lands and the rename does not. The staging copy is a binary sitting
// next to the target, so it has to be removed on this path too.
func TestAnInPlaceSwapThatCannotRenameLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, []byte(binary), 0o600); err != nil {
		t.Fatalf("staging: %v", err)
	}
	// A non-empty directory cannot be renamed over.
	target := filepath.Join(dir, "fleetfix")
	if err := os.MkdirAll(filepath.Join(target, "occupied"), 0o755); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := swapInPlace(staged, target); err == nil {
		t.Fatal("a directory is not something to rename a binary over")
	}
	if _, err := os.Stat(target + ".new"); !os.IsNotExist(err) {
		t.Errorf("%s.new exists (%v)", target, err)
	}
}

func TestTheDownloaderStreamsToTheStagedPath(t *testing.T) {
	var gotAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAgent = r.Header.Get("User-Agent")
		fmt.Fprint(w, binary)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "staged")
	if err := downloadHTTP(t.Context(), srv.URL, dest); err != nil {
		t.Fatalf("downloadHTTP: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(got) != binary {
		t.Errorf("got %q, want the asset", got)
	}
	if !strings.HasPrefix(gotAgent, "fleetfix/") {
		t.Errorf("User-Agent = %q, want it to name this binary", gotAgent)
	}
	// 0o600: a staging copy, in a directory only this process can enter.
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode is %v, want 0600", fi.Mode().Perm())
	}
}

func TestTheDownloaderRefusesAnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "staged")
	err := downloadHTTP(t.Context(), srv.URL, dest)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("got %v, want the status", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("%s exists (%v) after a 404", dest, err)
	}
}

// O_EXCL: the staged path is created, never opened. A file already there means
// something else got to the directory first, and the download stops.
func TestTheDownloaderWillNotWriteOverAnExistingFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, binary)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(dest, []byte("someone else's file"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := downloadHTTP(t.Context(), srv.URL, dest); err == nil {
		t.Fatal("an existing staged path is not a place to download into")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(got) != "someone else's file" {
		t.Errorf("got %q; the existing file was overwritten", got)
	}
}

func TestTheDownloaderReportsAnUnreachableHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	if err := downloadHTTP(t.Context(), url, filepath.Join(t.TempDir(), "staged")); err == nil {
		t.Error("a closed server serves no asset")
	}
	if err := downloadHTTP(t.Context(), "://not a url", filepath.Join(t.TempDir(), "s")); err == nil {
		t.Error("a malformed URL is not a request")
	}
}

// The defaults, exercised once against a server standing in for the release page.
func TestAnInstallerWithNoSeamsUsesTheNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			fmt.Fprintf(w, "%s  %s\n", digest(binary), asset)
			return
		}
		fmt.Fprint(w, binary)
	}))
	defer srv.Close()

	i := &Installer{
		Target:     filepath.Join(t.TempDir(), "fleetfix"),
		AssetName:  asset,
		StagingDir: t.TempDir(),
	}
	rel := Release{
		Version:     "2.1.0",
		AssetURL:    srv.URL + "/" + asset,
		ChecksumURL: srv.URL + "/" + asset + ".sha256",
	}

	if _, err := i.Apply(t.Context(), trailWriterOnly(t), rel); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(i.Target)
	if err != nil {
		t.Fatalf("reading the target: %v", err)
	}
	if string(got) != binary {
		t.Errorf("got %q, want the downloaded binary", got)
	}
}

func TestNewInstallerCarriesTheAssetName(t *testing.T) {
	if got := NewInstaller(asset).AssetName; got != asset {
		t.Errorf("got %q, want %q", got, asset)
	}
}
