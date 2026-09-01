package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/KingPin/FleetFix/v2/internal/audit"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/config"
	"github.com/KingPin/FleetFix/v2/internal/version"
)

// DownloadTimeout bounds fetching one asset. v1's 30s httpx timeout, which is the
// whole transfer and not just the connect: a binary is a few megabytes, and an
// operator running `fleetfix update` mid-incident should be told the mirror is slow
// rather than left watching a cursor.
const DownloadTimeout = 30 * time.Second

// swapTimeout bounds each privileged command. v1's 15s, per command rather than
// across the pair: `sudo -n` either has a cached credential or fails at once, so
// fifteen seconds is already generous and only a wedged filesystem reaches it.
const swapTimeout = 15 * time.Second

// maxAsset caps the download. Large enough for any binary this project will ship
// and small enough that a redirect landing on something endless fills a temporary
// directory rather than a disk.
const maxAsset = 256 << 20

// ErrNoDigest is a checksum file that names no digest for the asset being installed.
//
// Distinct from a mismatch: a release whose checksum file was built for a different
// asset list is a mistake in the release, and retrying will not fix it. A mismatch
// might be a truncated download, which retrying might.
var ErrNoDigest = errors.New("the checksum file names no digest for this asset")

// ErrDigestMismatch is a download whose hash is not the published one.
//
// The one failure in this package that is never worth working around. Everything
// between the release page and this host -- a proxy, a mirror, a captive portal
// that answered the asset URL -- is untrusted, and this check is what makes the
// difference between installing a release and installing whatever answered.
var ErrDigestMismatch = errors.New("the downloaded binary does not match its published digest")

// InstallResult describes what an Apply put where.
//
// v1's InstallResult carried `ok` and `error` as well. Both are gone: Apply returns
// an error, and a struct that also spells the failure out invites a caller to read
// one and not the other -- which for an updater means reporting a success that
// verified nothing.
type InstallResult struct {
	// Version is the release that was installed, or attempted.
	Version string

	// Target is the binary that was replaced.
	Target string

	// Bytes is the size of what was installed, zero if it could not be measured.
	Bytes int64
}

// Installer downloads a release, verifies it, and swaps it over the running binary.
//
// Every field is a seam. The defaults are the production path: a real HTTP client,
// a real sudo, and the binary this process was launched from.
type Installer struct {
	// Target is the binary to replace. Empty asks the kernel what is running.
	Target string

	// AssetName is the asset whose digest is looked up in the checksum file. It has
	// to match the asset that was downloaded, or verification checks the wrong file.
	AssetName string

	// StagingDir is where the private staging directory is created. Empty is
	// os.TempDir().
	StagingDir string

	// Download streams a URL to a path. Nil is an HTTP client with DownloadTimeout.
	Download func(ctx context.Context, url, dest string) error

	// FetchText retrieves the checksum file. Nil is the same client.
	FetchText func(ctx context.Context, url string) (string, error)

	// Swap installs the staged file over the target. Nil picks between the
	// unprivileged rename and the sudo pair based on the target's directory.
	Swap func(ctx context.Context, staged, target string) error

	// Runner runs the privileged commands. Nil is the real one.
	Runner cmdrun.Runner
}

// NewInstaller returns an installer for the given asset, with every seam defaulted.
func NewInstaller(assetName string) *Installer {
	return &Installer{AssetName: assetName}
}

// InstallTarget reports the binary an update should replace, and why it is guessing
// when it is.
//
// The running binary rather than a fixed path, so an update lands wherever the
// operator actually launched FleetFix from -- ~/bin, /opt, /usr/local/bin. v1 had to
// ask whether it was a frozen PyInstaller build before it could trust this, because
// running from source made sys.executable the interpreter; a Go binary is always
// the thing that was launched, so the question does not arise.
//
// Symlinks are resolved, as in v1: a /usr/local/bin/fleetfix pointing into /opt gets
// the file in /opt replaced, so the next launch through either path is the new
// version. The alternative -- replacing the symlink with a binary -- would leave the
// old file behind and the two paths disagreeing.
//
// The error is the reason for the fallback, not a failure. A caller that ignores it
// still gets a usable path, and the fallback is where a system install puts things.
func InstallTarget() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return config.BinaryPath, err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// The binary was moved or deleted while running, which is a live enough
		// case during an update to be worth answering rather than failing on.
		return exe, err
	}
	return resolved, nil
}

// CanWriteDirectly reports whether this user can replace target without sudo.
//
// The containing directory is what is tested, not the file. An atomic replacement
// renames a sibling over the target, so a root-owned binary in a directory the
// operator owns is replaceable and a user-owned binary in /usr/bin is not -- which
// is the opposite of what checking the file would say in both cases.
func CanWriteDirectly(target string) bool {
	parent := filepath.Dir(target)
	fi, err := os.Stat(parent)
	if err != nil || !fi.IsDir() {
		return false
	}
	// unix.Access rather than a mode comparison: the answer depends on the effective
	// uid, the gid list, and any ACL on the directory, and the kernel is the only
	// thing that knows all three.
	return unix.Access(parent, unix.W_OK) == nil
}

// HaveWritableTarget is the cheap pre-check: could an update land here at all?
//
// True when the directory is writable outright, or when it exists and sudo is on
// PATH to escalate for it. Not a guarantee -- sudo may have no cached credential,
// which only the attempt discovers -- but enough to keep `fleetfix update` from
// downloading a binary it has nowhere to put.
func HaveWritableTarget(target string, look cmdrun.Looker) bool {
	if CanWriteDirectly(target) {
		return true
	}
	fi, err := os.Stat(filepath.Dir(target))
	if err != nil || !fi.IsDir() {
		return false
	}
	if look == nil {
		look = cmdrun.NewPATH()
	}
	_, err = look.Look("sudo")
	return err == nil
}

// Apply downloads the release, verifies its digest, and swaps it into place.
//
// The order is v1's and the order is the point: nothing touches the install target
// until the download has been hashed against the digest the release published. A
// failure at any step leaves the running binary exactly as it was.
//
// The whole sequence is one audited action. The intent line is written before the
// download starts, so a host that loses power mid-update still has a record saying
// which version was being installed over which -- which is the question someone
// asks about a binary that no longer matches its package manager.
func (i *Installer) Apply(ctx context.Context, w *audit.Writer, rel Release) (InstallResult, error) {
	target := i.Target
	if target == "" {
		// The reason for a fallback is dropped here rather than reported: the path
		// is usable either way, and a swap onto the wrong one fails loudly at the
		// rename with something more specific than this error could say.
		target, _ = InstallTarget()
	}
	res := InstallResult{Version: rel.Version, Target: target}

	err := w.Do("updater.apply", audit.Fields{
		// v1 wrote the literal string "unknown" here, because its installer had no
		// way to reach the running version. This one does, and the field is the
		// only record of what was replaced -- "unknown -> 2.1.0" answers half the
		// question an operator brings to this line.
		{Key: "version_from", Value: version.Version()},
		{Key: "version_to", Value: rel.Version},
		{Key: "asset_url", Value: rel.AssetURL},
		{Key: "target", Value: target},
	}, func(call *audit.Call) error {
		staged, cleanup, err := i.stage(rel.Version)
		if err != nil {
			return fmt.Errorf("could not stage the download: %w", err)
		}
		defer cleanup()

		if err := i.download(ctx, rel.AssetURL, staged); err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
		text, err := i.fetchText(ctx, rel.ChecksumURL)
		if err != nil {
			return fmt.Errorf("checksum fetch failed: %w", err)
		}
		expected, ok := ParseSHA256Line(text, i.AssetName)
		if !ok {
			return fmt.Errorf("%w: %s", ErrNoDigest, i.AssetName)
		}
		actual, err := sha256File(staged)
		if err != nil {
			return fmt.Errorf("could not hash the download: %w", err)
		}
		if actual != expected {
			return fmt.Errorf("%w: published %s, downloaded %s", ErrDigestMismatch, expected, actual)
		}

		if err := i.swap(ctx, staged, target); err != nil {
			return err
		}

		// Measured after the swap, as in v1, and nil rather than zero when it
		// cannot be: a bytes_installed of 0 in the trail reads as an empty binary
		// having been installed, which is a much worse thing than an unknown size.
		var size any
		if fi, err := os.Stat(staged); err == nil {
			size = fi.Size()
			res.Bytes = fi.Size()
		}
		call.SetResult("bytes_installed", size)
		return nil
	})
	return res, err
}

// stage makes a private directory to download into, and a cleanup that removes it.
//
// A directory of its own, where v1 wrote /tmp/fleetfix.<version>.new directly. The
// name v1 used is predictable and /tmp is world-writable, so another local user
// could have left a symlink there and had the download follow it. The digest check
// protects what gets installed; it does nothing about what gets overwritten on the
// way. A 0o700 directory this process just created cannot be pre-seeded.
func (i *Installer) stage(release string) (string, func(), error) {
	dir, err := os.MkdirTemp(i.StagingDir, "fleetfix-update-")
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(dir, "fleetfix."+release+".new"), func() { _ = os.RemoveAll(dir) }, nil
}

func (i *Installer) download(ctx context.Context, url, dest string) error {
	if i.Download != nil {
		return i.Download(ctx, url, dest)
	}
	return downloadHTTP(ctx, url, dest)
}

func (i *Installer) fetchText(ctx context.Context, url string) (string, error) {
	if i.FetchText != nil {
		return i.FetchText(ctx, url)
	}
	body, err := fetchHTTP(ctx, url)
	return string(body), err
}

func (i *Installer) swap(ctx context.Context, staged, target string) error {
	if i.Swap != nil {
		return i.Swap(ctx, staged, target)
	}
	if CanWriteDirectly(target) {
		return swapInPlace(staged, target)
	}
	return i.sudoSwap(ctx, staged, target)
}

func (i *Installer) runner() cmdrun.Runner {
	if i.Runner != nil {
		return i.Runner
	}
	return cmdrun.New()
}

// swapInPlace installs without escalating: copy into the target's directory, then
// rename over the target.
//
// The copy has to land in the same directory as the target, because rename is only
// atomic within a filesystem and /tmp is often a different one. The rename itself is
// what makes this safe to run on a live binary -- a concurrent exec either gets the
// whole old file or the whole new one, and never a half-written one.
func swapInPlace(staged, target string) error {
	staging := target + ".new"
	if err := copyExecutable(staged, staging); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("in-place install failed: %w", err)
	}
	if err := os.Rename(staging, target); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("in-place install failed: %w", err)
	}
	return nil
}

// copyExecutable writes src to dst and makes it world-executable.
//
// The chmod is separate from the create because a create's mode is masked by the
// umask, and an operator running with 077 would otherwise install a binary only
// they could run -- on a host where the point of /usr/local/bin is that everyone
// can. This is the same 0o755 the `install -m 0755` on the privileged path sets.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: the path is what the caller staged
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gosec // G302/G304: a binary in a bin directory is meant to be executable
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, 0o755) //nolint:gosec // G302: see above
}

// sudoSwap runs the privileged install and rename.
//
// Two commands rather than one `sudo mv` from the staging directory: `install -m
// 0755` sets the mode as it copies, and the second step is a rename within the
// target directory, which is the atomic one. A single privileged move from /tmp
// would cross filesystems and stop being atomic.
func (i *Installer) sudoSwap(ctx context.Context, staged, target string) error {
	staging := target + ".new"
	if err := i.sudo(ctx, "install", "-m", "0755", staged, staging); err != nil {
		return err
	}
	return i.sudo(ctx, "mv", "-f", staging, target)
}

func (i *Installer) sudo(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, swapTimeout)
	defer cancel()

	res, err := cmdrun.RunSudo(ctx, i.runner(), name, args...)
	switch {
	case cmdrun.IsNotFound(err):
		// The pre-check said sudo was on PATH, or nobody ran one. Either way this
		// is the honest message: there is no way to write the target.
		return errors.New("sudo not found")
	case err != nil:
		return fmt.Errorf("sudo %s: %w", name, err)
	case !res.OK():
		// sudo's own stderr when there is any: "a password is required" is the
		// common one, and it tells the operator to run `sudo -v` and try again.
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = fmt.Sprintf("exited %d", res.ExitCode)
		}
		return fmt.Errorf("sudo %s: %s", name, detail)
	}
	return nil
}

// sha256File hashes a file in a stream rather than reading it in.
func sha256File(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the path is what this package staged
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// downloadHTTP streams an asset to dest.
//
// Written straight to the file rather than buffered, because the asset is the size
// of a binary and a host running this during an incident has other uses for its
// memory. The read is capped for the same reason the checker's is: whatever answers
// the redirect chain is not necessarily GitHub.
func downloadHTTP(ctx context.Context, url, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, DownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // the body is read or abandoned

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the download answered %s", resp.Status)
	}

	// 0o600: this is a staging copy in a directory only this process can enter, and
	// the mode it is installed with is set by the swap.
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the path is what this package staged
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxAsset)); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
