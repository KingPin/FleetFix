// Package cmdrun is the one place in FleetFix that runs an external command.
//
// v1 had no such seam: every module called subprocess.run directly, which is why
// its tests fake subprocess in five mutually incompatible styles and why none of
// the destructive paths are testable without a real host. depguard keeps os/exec
// out of every other package so that cannot happen again.
//
// The semantics reproduce what v1's callers relied on, because the parsers that
// consume the output are being ported byte-for-byte:
//
//   - A non-zero exit is not an error. `smartctl` sets bit flags in its exit code
//     while printing a perfectly good report, and `ping` exits 1 on 100% loss with
//     the summary the caller wants. The exit code is data; only a command that
//     could not be run or did not finish is an error.
//   - stdout and stderr are captured separately, and Combined concatenates them in
//     that order, matching the `(result.stdout or "") + (result.stderr or "")` that
//     v1's callers hand to their parsers.
//   - The timeout is the caller's context. v1 passed a per-call timeout=N and
//     treated expiry as "no data"; a context deadline is the same contract with the
//     cancellation the TUI needs, since a 25s traceroute has to be abandonable.
//
// Deliberately absent: an environment override. Forcing LC_ALL=C would stabilise
// `df` and `ss` output, but the fixture corpus was captured without it and the
// differential harness compares against those bytes. Changing the environment
// would change the subject of the comparison. Post-2.0, with the harness retired.
package cmdrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"time"
)

// waitDelay bounds how long Wait blocks after the context is cancelled and the
// process has been killed. A child that spawned its own children can leave the
// output pipes held open, and without this the goroutine parks forever on a read
// that will never return -- the Go analogue of the hang the TUI's worker groups
// exist to prevent.
const waitDelay = time.Second

// ErrNotFound reports that the executable is not on PATH.
//
// Its own sentinel because it is the routine case, not a fault: a host with no
// docker, no systemd, or no smartctl is a supported host, and every collector has
// to be able to tell "the tool is absent" (report unavailable) from "the tool ran
// and said something" (report a verdict).
var ErrNotFound = errors.New("cmdrun: executable not found")

// Result is the outcome of a command that ran to completion.
type Result struct {
	// Args is the full argv as invoked, including the executable name, so a
	// failure message can show exactly what was run.
	Args []string

	Stdout   string
	Stderr   string
	ExitCode int
}

// Combined returns stdout followed by stderr.
//
// Several tools split what one parser needs across both streams -- ping writes its
// summary to stdout and "Name or service not known" to stderr -- so the parsers
// take the concatenation. This is the v1 convention preserved verbatim.
func (r Result) Combined() string { return r.Stdout + r.Stderr }

// OK reports whether the command exited zero.
func (r Result) OK() bool { return r.ExitCode == 0 }

// Command renders the argv for a log line or an error message.
func (r Result) Command() string { return strings.Join(r.Args, " ") }

// Runner runs external commands. The OS implementation is the real thing; Fake is
// for tests.
type Runner interface {
	// Run executes name with args and waits for it to finish.
	//
	// It returns an error only when the command could not be started or did not
	// complete: a missing executable (ErrNotFound), an expired context, or an OS
	// failure. A command that ran and exited non-zero returns a Result with that
	// exit code and a nil error.
	Run(ctx context.Context, name string, args ...string) (Result, error)
}

// OS runs commands via os/exec.
type OS struct{}

// New returns a Runner that actually executes commands.
func New() *OS { return &OS{} }

// Run implements Runner.
func (*OS) Run(ctx context.Context, name string, args ...string) (Result, error) {
	argv := append([]string{name}, args...)

	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = waitDelay

	err := cmd.Run()
	res := Result{Args: argv, Stdout: stdout.String(), Stderr: stderr.String()}

	// Check the context first. A killed process reports an ExitError, and reading
	// that as "the tool said no" would turn an abandoned 25s traceroute into a
	// confident negative verdict.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, fmt.Errorf("cmdrun: %s: %w", strings.Join(argv, " "), ctxErr)
	}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = cmd.ProcessState.ExitCode()
	case errors.As(err, &exitErr):
		// Ran, exited non-zero. Data, not an error.
		res.ExitCode = exitErr.ExitCode()
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		// Two spellings of the same fact: a bare name missing from PATH gives
		// exec.ErrNotFound, while a name with a slash gives ENOENT. A caller
		// deciding whether the tool is installed should not have to know which.
		//
		// EACCES stays a plain error on purpose -- a smartctl that exists but is
		// not executable is a broken host, not an absent tool, and reporting it as
		// "not installed" would hide that.
		return res, fmt.Errorf("%w: %s", ErrNotFound, name)
	default:
		return res, fmt.Errorf("cmdrun: %s: %w", strings.Join(argv, " "), err)
	}
	return res, nil
}

// SudoArgs wraps an argv in `sudo -n`.
//
// -n is load-bearing: it makes sudo fail immediately when there is no cached
// credential rather than blocking on a password prompt that, inside a TUI or an
// unattended agent, nothing can answer. Per-call escalation is also why FleetFix
// never re-execs itself as root -- doing that would erase the operator's real
// identity from every audit record for the rest of the session.
func SudoArgs(name string, args ...string) (string, []string) {
	return "sudo", append([]string{"-n", name}, args...)
}

// RunSudo runs a command through `sudo -n` on any Runner.
//
// A free function rather than a Runner method so Fake needs no special case: the
// fake sees the full "sudo -n smartctl ..." argv, which is also exactly the argv
// v1 invoked and therefore what the fixture corpus was captured from.
func RunSudo(ctx context.Context, r Runner, name string, args ...string) (Result, error) {
	sudo, sudoArgs := SudoArgs(name, args...)
	return r.Run(ctx, sudo, sudoArgs...)
}

// IsNotFound reports whether err means the executable is absent from PATH.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsTimeout reports whether err means the command was abandoned rather than
// answered.
func IsTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// Key renders an argv into the string Fake matches on.
//
// Exported so a test builds its expectations exactly the way the fake looks them
// up; two spellings of the same key is how a fake ends up silently unmatched.
func Key(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), " ")
}
