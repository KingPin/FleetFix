package updatecmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/config"
	"github.com/KingPin/FleetFix/v2/internal/exitcode"
	"github.com/KingPin/FleetFix/v2/internal/resolve"
	"github.com/KingPin/FleetFix/v2/internal/updater"
)

const (
	asset  = "fleetfix-linux-x86_64"
	binary = "#!/bin/sh\necho the new fleetfix\n"
)

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// releasePayload is a releases API response naming both assets for one tag.
func releasePayload(tag string) string {
	return fmt.Sprintf(`{
		"tag_name": %q,
		"html_url": "https://github.com/KingPin/FleetFix/releases/tag/%s",
		"assets": [
			{"name": %q, "browser_download_url": "https://example.invalid/bin"},
			{"name": %q, "browser_download_url": "https://example.invalid/sum"}
		]
	}`, tag, tag, asset, asset+".sha256")
}

// checker answers with one release and never reaches the network.
func checker(t *testing.T, tag string) *updater.Checker {
	t.Helper()
	return &updater.Checker{
		URL:       "https://example.invalid/releases/latest",
		AssetName: asset,
		CachePath: filepath.Join(t.TempDir(), "release_check.json"),
		Now:       time.Now,
		Fetch: func(context.Context, string) ([]byte, error) {
			return []byte(releasePayload(tag)), nil
		},
	}
}

// host is a resolver whose audit trail lands in a temporary file rather than in
// /var/log, and whose PATH is whatever the caller says it is.
func host(t *testing.T, installed ...string) *resolve.Resolved {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	return resolve.New(resolve.Options{
		Paths:     &config.Paths{CacheDir: t.TempDir(), StateDir: t.TempDir()},
		Looker:    cmdrun.NewFakeLooker(installed...),
		AuditPath: func() (string, error) { return path, nil },
	})
}

// installer writes `binary` to the target instead of downloading it.
func installer(t *testing.T) *updater.Installer {
	t.Helper()
	return &updater.Installer{
		Target:     filepath.Join(t.TempDir(), "fleetfix"),
		AssetName:  asset,
		StagingDir: t.TempDir(),
		Download: func(_ context.Context, _, dest string) error {
			return os.WriteFile(dest, []byte(binary), 0o600)
		},
		FetchText: func(context.Context, string) (string, error) {
			return digest(binary) + "  " + asset + "\n", nil
		},
	}
}

func run(t *testing.T, opts Options) (string, int, error) {
	t.Helper()
	var out strings.Builder
	opts.Stdout = &out
	code, err := Run(t.Context(), opts)
	return out.String(), code, err
}

func TestAHostOnTheLatestReleaseIsToldSo(t *testing.T) {
	out, code, err := run(t, Options{
		Version:  "2.0.0",
		Resolved: host(t),
		Checker:  checker(t, "v2.0.0"),
	})
	if err != nil || code != exitcode.OK {
		t.Fatalf("Run = %d, %v, want OK", code, err)
	}
	if !strings.Contains(out, "latest release") || !strings.Contains(out, "2.0.0") {
		t.Errorf("got %q, want the up-to-date line", out)
	}
}

// The default. It has to be obvious from the output that nothing was installed,
// or an operator will believe it was.
func TestAnAvailableReleaseIsReportedAndNotInstalled(t *testing.T) {
	i := installer(t)
	out, code, err := run(t, Options{
		Version:   "2.0.0",
		Resolved:  host(t),
		Checker:   checker(t, "v2.1.0"),
		Installer: i,
	})
	if err != nil || code != exitcode.OK {
		t.Fatalf("Run = %d, %v, want OK", code, err)
	}
	for _, want := range []string{"2.1.0 is available", "this host runs 2.0.0", "releases/tag/v2.1.0", "--apply"} {
		if !strings.Contains(out, want) {
			t.Errorf("got %q, want it to mention %q", out, want)
		}
	}
	if _, err := os.Stat(i.Target); !os.IsNotExist(err) {
		t.Errorf("%s exists (%v); a report-only run installed something", i.Target, err)
	}
}

// A release with no notes page. The line is dropped rather than printed empty.
func TestAReleaseWithNoNotesPagePrintsNoLinkLine(t *testing.T) {
	c := checker(t, "v2.1.0")
	c.Fetch = func(context.Context, string) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"tag_name": "v2.1.0", "assets": [
			{"name": %q, "browser_download_url": "https://example.invalid/bin"},
			{"name": %q, "browser_download_url": "https://example.invalid/sum"}]}`,
			asset, asset+".sha256")), nil
	}

	out, _, err := run(t, Options{Version: "2.0.0", Resolved: host(t), Checker: c})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out, "Release notes") {
		t.Errorf("got %q, want no notes line for a release with no page", out)
	}
}

// v1 reported nothing when GitHub would not answer, because a banner had nothing
// to say. A command an operator typed does.
func TestAnUnansweredCheckIsReportedRatherThanSilent(t *testing.T) {
	c := checker(t, "v2.1.0")
	c.Fetch = func(context.Context, string) ([]byte, error) {
		return nil, errors.New("the releases API answered 403 rate limit exceeded")
	}

	out, code, err := run(t, Options{Version: "2.0.0", Resolved: host(t), Checker: c})
	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("got %v, want the API's answer", err)
	}
	if out != "" {
		t.Errorf("stdout = %q; a diagnostic belongs on stderr", out)
	}
}

func TestApplyInstallsTheVerifiedRelease(t *testing.T) {
	i := installer(t)
	out, code, err := run(t, Options{
		Version:   "2.0.0",
		Resolved:  host(t),
		Checker:   checker(t, "v2.1.0"),
		Installer: i,
		Apply:     true,
	})
	if err != nil || code != exitcode.OK {
		t.Fatalf("Run = %d, %v, want OK", code, err)
	}
	got, err := os.ReadFile(i.Target)
	if err != nil {
		t.Fatalf("reading the target: %v", err)
	}
	if string(got) != binary {
		t.Errorf("got %q, want the installed binary", got)
	}
	if !strings.Contains(out, "Installed fleetfix 2.1.0") || !strings.Contains(out, i.Target) {
		t.Errorf("got %q, want it to name the version and the target", out)
	}
	// v1 never relaunched, and neither does this: the operator may be mid-triage.
	if !strings.Contains(out, "Restart") {
		t.Errorf("got %q, want the restart line", out)
	}
}

// The install is audited, and the trail is the local file rather than a promise.
func TestAnInstallLeavesATrail(t *testing.T) {
	trail := filepath.Join(t.TempDir(), "audit.log")
	h := resolve.New(resolve.Options{
		Paths:     &config.Paths{CacheDir: t.TempDir(), StateDir: t.TempDir()},
		Looker:    cmdrun.NewFakeLooker(),
		AuditPath: func() (string, error) { return trail, nil },
	})

	if _, code, err := run(t, Options{
		Version: "2.0.0", Resolved: h, Checker: checker(t, "v2.1.0"),
		Installer: installer(t), Apply: true,
	}); err != nil || code != exitcode.OK {
		t.Fatalf("Run = %d, %v", code, err)
	}

	data, err := os.ReadFile(trail)
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	for _, want := range []string{"updater.apply", `"phase": "intent"`, `"phase": "result"`, "2.1.0"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the trail does not mention %q:\n%s", want, data)
		}
	}
}

// A trail that cannot be opened stops the update before anything is downloaded.
// An unrecorded binary swap is how a fleet ends up with a version nobody can
// account for.
func TestAnUnwritableTrailRefusesTheInstall(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory regardless of its mode")
	}
	locked := t.TempDir()
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	h := resolve.New(resolve.Options{
		Paths:     &config.Paths{CacheDir: t.TempDir(), StateDir: t.TempDir()},
		Looker:    cmdrun.NewFakeLooker(),
		AuditPath: func() (string, error) { return filepath.Join(locked, "audit.log"), nil },
	})
	i := installer(t)
	downloaded := false
	i.Download = func(context.Context, string, string) error {
		downloaded = true
		return nil
	}

	_, code, err := run(t, Options{
		Version: "2.0.0", Resolved: h, Checker: checker(t, "v2.1.0"),
		Installer: i, Apply: true,
	})
	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
	if err == nil || !strings.Contains(err.Error(), "audit trail") {
		t.Errorf("got %v, want the missing trail", err)
	}
	if downloaded {
		t.Error("the asset was downloaded before the trail was checked")
	}
}

// A host where the binary cannot be replaced is told so in a second, not after a
// download has crossed the network.
func TestAnUnwritableTargetIsRefusedBeforeTheDownload(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory regardless of its mode")
	}
	locked := t.TempDir()
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// The pre-check asks about the installer's target and the resolver's PATH, so a
	// locked directory plus a PATH with no sudo is a host that cannot install.
	i := installer(t)
	i.Target = filepath.Join(locked, "fleetfix")
	downloaded := false
	i.Download = func(context.Context, string, string) error {
		downloaded = true
		return nil
	}

	_, code, err := run(t, Options{
		Version: "2.0.0", Resolved: host(t), Checker: checker(t, "v2.1.0"),
		Installer: i, Apply: true,
	})
	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
	if err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Errorf("got %v, want the unwritable target", err)
	}
	if downloaded {
		t.Error("the asset was downloaded onto a host that cannot install it")
	}
}

// The one failure worth its own message: what arrived is not what was published.
func TestAMismatchedDigestIsNamedForWhatItIs(t *testing.T) {
	i := installer(t)
	i.Download = func(_ context.Context, _, dest string) error {
		return os.WriteFile(dest, []byte("something else answered the redirect"), 0o600)
	}

	_, code, err := run(t, Options{
		Version: "2.0.0", Resolved: host(t), Checker: checker(t, "v2.1.0"),
		Installer: i, Apply: true,
	})
	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
	if err == nil || !strings.Contains(err.Error(), "unverified binary") {
		t.Errorf("got %v, want the refusal to name itself", err)
	}
	if !errors.Is(err, updater.ErrDigestMismatch) {
		t.Errorf("got %v, want it to wrap ErrDigestMismatch", err)
	}
}

func TestAnOrdinaryInstallFailureIsPassedThrough(t *testing.T) {
	i := installer(t)
	i.Swap = func(context.Context, string, string) error {
		return errors.New("read-only file system")
	}

	_, code, err := run(t, Options{
		Version: "2.0.0", Resolved: host(t), Checker: checker(t, "v2.1.0"),
		Installer: i, Apply: true,
	})
	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("got %v, want the install's own failure", err)
	}
}

// A closed pipe. The message is the whole output, so failing to write it is a
// failure of the command.
func TestAStdoutThatWillNotTakeTheMessageIsAnError(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts Options
	}{
		{"up to date", Options{Version: "2.0.0", Checker: checker(t, "v2.0.0")}},
		{"available", Options{Version: "2.0.0", Checker: checker(t, "v2.1.0")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := tt.opts
			opts.Resolved = host(t)
			opts.Stdout = brokenWriter{}
			if _, err := Run(t.Context(), opts); err == nil {
				t.Error("a write that failed is not a report that was made")
			}
		})
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// A nil Resolved resolves the live host rather than panicking. The check below it
// is injected, so nothing here reaches the network or the real trail.
func TestANilResolverResolvesTheLiveHost(t *testing.T) {
	if _, code, err := run(t, Options{Version: "2.0.0", Checker: checker(t, "v2.0.0")}); err != nil || code != exitcode.OK {
		t.Errorf("Run = %d, %v, want OK", code, err)
	}
}

// The checker the command builds for itself: pointed at the real API, caching to
// this host's cache directory, and treating every stored answer as stale.
func TestTheDefaultCheckerAlwaysAsksAndStillWrites(t *testing.T) {
	dir := t.TempDir()
	c := freshChecker(resolve.New(resolve.Options{
		Paths:     &config.Paths{CacheDir: dir, StateDir: t.TempDir()},
		Looker:    cmdrun.NewFakeLooker(),
		AuditPath: func() (string, error) { return filepath.Join(t.TempDir(), "audit.log"), nil },
	}))

	if c.URL != config.GitHubReleasesURL {
		t.Errorf("URL = %q, want the releases API", c.URL)
	}
	if c.CacheTTL != 0 {
		t.Errorf("CacheTTL = %v, want every read to be stale", c.CacheTTL)
	}
	// Still written, so the next launch banner has something to read.
	if c.CachePath != filepath.Join(dir, config.ReleaseCacheFile) {
		t.Errorf("CachePath = %q, want it under %q", c.CachePath, dir)
	}
}

// The checker the command builds when it was handed none. Exercised through a
// context that is already over, so the answer is the failure and no request for
// it ever leaves the host.
func TestARunWithNoCheckerBuildsOneAndReportsWhatItSays(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var out strings.Builder
	code, err := Run(ctx, Options{Stdout: &out, Version: "2.0.0", Resolved: host(t)})
	if err == nil {
		t.Fatal("a check that could not be made is not an up-to-date host")
	}
	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
	if out.String() != "" {
		t.Errorf("got %q on stdout, want the reason on stderr alone", out.String())
	}
}

// An install with no injected installer builds one against the running binary.
// The trail is arranged to be unopenable so the refusal lands before anything is
// downloaded -- which is also what keeps this test from replacing itself.
func TestApplyBuildsAnInstallerAgainstTheRunningBinary(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory regardless of its mode")
	}
	locked := t.TempDir()
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	h := resolve.New(resolve.Options{
		Paths:     &config.Paths{CacheDir: t.TempDir(), StateDir: t.TempDir()},
		Looker:    cmdrun.NewFakeLooker("sudo"),
		AuditPath: func() (string, error) { return filepath.Join(locked, "audit.log"), nil },
	})

	_, code, err := run(t, Options{
		Version: "2.0.0", Resolved: h, Checker: checker(t, "v2.1.0"), Apply: true,
	})
	if code != exitcode.Unknown {
		t.Errorf("exit code = %d, want %d", code, exitcode.Unknown)
	}
	if err == nil || !strings.Contains(err.Error(), "audit trail") {
		t.Errorf("got %v, want the missing trail", err)
	}
}

// A target the caller named is the one the pre-check asks about. The two coming
// apart would mean approving one binary and replacing another.
func TestAnInjectedTargetIsTheOneThatIsChecked(t *testing.T) {
	i := installer(t)
	target := i.Target
	swapped := ""
	i.Swap = func(_ context.Context, _, dest string) error {
		swapped = dest
		return nil
	}

	if _, _, err := run(t, Options{
		Version: "2.0.0", Resolved: host(t), Checker: checker(t, "v2.1.0"),
		Installer: i, Apply: true,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if swapped != target {
		t.Errorf("swapped %q, want the target the caller named, %q", swapped, target)
	}
}

// A stdout that stops taking writes partway is still an error, on both of the
// lines that follow a successful first one.
func TestAReportThatStopsPartwayIsAnError(t *testing.T) {
	t.Run("the notes link", func(t *testing.T) {
		if _, err := Run(t.Context(), Options{
			Stdout: &failAfter{n: 1}, Version: "2.0.0",
			Resolved: host(t), Checker: checker(t, "v2.1.0"),
		}); err == nil {
			t.Error("a write that failed is not a report that was made")
		}
	})
	t.Run("the installed line", func(t *testing.T) {
		i := installer(t)
		i.Swap = func(context.Context, string, string) error { return nil }
		if _, err := Run(t.Context(), Options{
			Stdout: brokenWriter{}, Version: "2.0.0", Resolved: host(t),
			Checker: checker(t, "v2.1.0"), Installer: i, Apply: true,
		}); err == nil {
			t.Error("a write that failed is not an install that was announced")
		}
	})
}

// failAfter takes n writes and then refuses, which is a pipe whose reader went
// away mid-report.
type failAfter struct{ n int }

func (f *failAfter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("broken pipe")
	}
	f.n--
	return len(p), nil
}
