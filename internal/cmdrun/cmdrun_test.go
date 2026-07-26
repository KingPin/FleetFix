package cmdrun

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Both implementations must satisfy the interface, checked at compile time so a
// signature drift is a build failure rather than a puzzling test failure.
var (
	_ Runner = (*OS)(nil)
	_ Runner = (*Fake)(nil)
)

func TestRunCapturesBothStreamsSeparately(t *testing.T) {
	res, err := New().Run(t.Context(), "sh", "-c", "printf out; printf err >&2")
	if err != nil {
		t.Fatalf("Run errored: %v", err)
	}
	if res.Stdout != "out" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "out")
	}
	if res.Stderr != "err" {
		t.Errorf("Stderr = %q, want %q", res.Stderr, "err")
	}
	// The parsers consume this concatenation, in this order. ping puts its summary
	// on stdout and "Name or service not known" on stderr, and one parser reads
	// both.
	if got := res.Combined(); got != "outerr" {
		t.Errorf("Combined() = %q, want %q", got, "outerr")
	}
}

func TestNonZeroExitIsDataNotAnError(t *testing.T) {
	// smartctl sets bit flags in its exit code while printing a usable report, and
	// ping exits 1 on total loss with the summary the caller wants. Treating either
	// as an error would throw away the output the check is built on.
	res, err := New().Run(t.Context(), "sh", "-c", "printf still-useful; exit 3")
	if err != nil {
		t.Fatalf("Run errored on a non-zero exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if res.OK() {
		t.Error("OK() = true for exit 3")
	}
	if res.Stdout != "still-useful" {
		t.Errorf("Stdout = %q, want the output kept", res.Stdout)
	}
}

func TestSuccessReportsExitZero(t *testing.T) {
	res, err := New().Run(t.Context(), "true")
	if err != nil {
		t.Fatalf("Run errored: %v", err)
	}
	if !res.OK() || res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, OK() = %v, want 0 and true", res.ExitCode, res.OK())
	}
}

func TestMissingExecutableIsErrNotFound(t *testing.T) {
	tests := []struct {
		name string
		exe  string
	}{
		// A bare name missing from PATH and a path that does not exist fail with
		// different errors from os/exec. A collector asking "is smartctl installed"
		// must not have to care which.
		{name: "bare name", exe: "fleetfix-no-such-executable-8f2a"},
		{name: "absolute path", exe: "/nonexistent/fleetfix-no-such-executable-8f2a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().Run(t.Context(), tt.exe)
			if err == nil {
				t.Fatal("Run returned no error for a missing executable")
			}
			if !IsNotFound(err) {
				t.Errorf("IsNotFound(%v) = false, want true", err)
			}
			if IsTimeout(err) {
				t.Errorf("IsTimeout(%v) = true; a missing tool is not a timeout", err)
			}
			if !strings.Contains(err.Error(), tt.exe) {
				t.Errorf("error %q does not name the executable %q", err, tt.exe)
			}
		})
	}
}

func TestUnexecutableFileIsNotReportedAsMissing(t *testing.T) {
	// A tool that exists but cannot be executed is a broken host, not an absent
	// tool. Folding EACCES into ErrNotFound would report it as "not installed" and
	// hide the real problem.
	path := t.TempDir() + "/not-executable"
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture failed: %v", err)
	}
	_, err := New().Run(t.Context(), path)
	if err == nil {
		t.Fatal("Run returned no error for a non-executable file")
	}
	if IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = true; the file exists, it just is not executable", err)
	}
}

func TestExpiredBudgetIsATimeoutNotAnExitCode(t *testing.T) {
	// The killed process reports an ExitError. Reading that as "the tool said no"
	// would turn an abandoned 25s traceroute into a confident negative verdict.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := New().Run(ctx, "sleep", "30")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Run returned no error; got exit %d", res.ExitCode)
	}
	if !IsTimeout(err) {
		t.Errorf("IsTimeout(%v) = false, want true", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Error("the timeout surfaced as an ExitError; callers would read a verdict out of it")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Run took %v to abandon a killed process", elapsed)
	}
}

func TestAnOrphanHoldingThePipeDoesNotHangForever(t *testing.T) {
	// The guard waitDelay exists for. Killing the shell does not kill the
	// background sleep still holding stdout, so Wait would block on a read that
	// never returns -- a permanently stuck goroutine and, in the TUI, a spinner
	// that never stops.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := New().Run(ctx, "sh", "-c", "sleep 30 & wait")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run returned no error for an abandoned command")
	}
	if !IsTimeout(err) {
		t.Errorf("IsTimeout(%v) = false, want true", err)
	}
	// waitDelay bounds this at ~1s; anything near 30s means the delay is not wired
	// up and the goroutine is waiting on the orphan.
	if elapsed > 10*time.Second {
		t.Errorf("Run took %v; waitDelay is not bounding the wait", elapsed)
	}
}

func TestAlreadyCancelledContextRunsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := New().Run(ctx, "sh", "-c", "exit 0")
	if err == nil {
		t.Fatal("Run returned no error under a cancelled context")
	}
	if !IsTimeout(err) {
		t.Errorf("IsTimeout(%v) = false; a cancelled run is abandoned, not answered", err)
	}
}

func TestResultRecordsTheFullArgv(t *testing.T) {
	res, err := New().Run(t.Context(), "sh", "-c", "true")
	if err != nil {
		t.Fatalf("Run errored: %v", err)
	}
	want := []string{"sh", "-c", "true"}
	if len(res.Args) != len(want) {
		t.Fatalf("Args = %v, want %v", res.Args, want)
	}
	for i := range want {
		if res.Args[i] != want[i] {
			t.Fatalf("Args = %v, want %v", res.Args, want)
		}
	}
	if got := res.Command(); got != "sh -c true" {
		t.Errorf("Command() = %q, want %q", got, "sh -c true")
	}
}

func TestRunDoesNotAliasTheCallersArgs(t *testing.T) {
	// Args is built with append over a fresh slice; if it ever shared backing
	// storage with the variadic args, a caller reusing that slice would find its
	// own values rewritten.
	args := make([]string, 0, 8)
	args = append(args, "-c", "true")
	res, err := New().Run(t.Context(), "sh", args...)
	if err != nil {
		t.Fatalf("Run errored: %v", err)
	}
	if args[0] != "-c" || args[1] != "true" {
		t.Errorf("caller's args mutated to %v", args)
	}
	res.Args[1] = "clobbered"
	if args[0] != "-c" {
		t.Errorf("mutating Result.Args reached the caller's slice: %v", args)
	}
}

func TestSudoArgsUsesNonInteractiveSudo(t *testing.T) {
	// -n is load-bearing: without it sudo blocks on a password prompt that nothing
	// inside a TUI or an unattended agent can answer. Fails closed instead.
	name, args := SudoArgs("smartctl", "-H", "-A", "/dev/sda")
	if name != "sudo" {
		t.Errorf("name = %q, want %q", name, "sudo")
	}
	want := "-n smartctl -H -A /dev/sda"
	if got := strings.Join(args, " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestSudoArgsDoesNotAliasTheCallersArgs(t *testing.T) {
	args := make([]string, 0, 4)
	args = append(args, "-H", "/dev/sda")
	SudoArgs("smartctl", args...)
	if args[0] != "-H" || args[1] != "/dev/sda" {
		t.Errorf("caller's args mutated to %v", args)
	}
}

func TestRunSudoPassesTheWholeArgvToTheRunner(t *testing.T) {
	// The fake sees "sudo -n smartctl ...", which is also the argv v1 invoked and
	// therefore what the fixture corpus was captured from.
	f := NewFake().Stdout("report", "sudo", "-n", "smartctl", "-H", "/dev/sda")
	res, err := RunSudo(t.Context(), f, "smartctl", "-H", "/dev/sda")
	if err != nil {
		t.Fatalf("RunSudo errored: %v", err)
	}
	if res.Stdout != "report" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "report")
	}
	if !f.Called("sudo", "-n", "smartctl", "-H", "/dev/sda") {
		t.Errorf("calls = %v, want the sudo-wrapped argv", f.Calls())
	}
}

func TestIsNotFoundAndIsTimeoutRejectUnrelatedErrors(t *testing.T) {
	err := errors.New("something else")
	if IsNotFound(err) {
		t.Error("IsNotFound matched an unrelated error")
	}
	if IsTimeout(err) {
		t.Error("IsTimeout matched an unrelated error")
	}
	if IsNotFound(nil) || IsTimeout(nil) {
		t.Error("a nil error matched a failure predicate")
	}
}

func TestIsTimeoutCoversCancellation(t *testing.T) {
	// A view switched away from cancels its group; that is abandonment too, and a
	// caller must not report it as a verdict.
	if !IsTimeout(context.Canceled) {
		t.Error("IsTimeout(context.Canceled) = false, want true")
	}
}

func TestKey(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "docker", args: []string{"ps", "--format", "{{.ID}}"}, want: "docker ps --format {{.ID}}"},
		{name: "true", want: "true"},
		// Joined on single spaces with no quoting: an argument containing a space
		// collides with two arguments. Acceptable because the argv sets here are
		// fixed tool invocations, but it is why Key is exported -- a test builds its
		// expectation the same way the fake looks it up.
		{name: "df", args: []string{"a b"}, want: "df a b"},
	}
	for _, tt := range tests {
		if got := Key(tt.name, tt.args...); got != tt.want {
			t.Errorf("Key(%q, %v) = %q, want %q", tt.name, tt.args, got, tt.want)
		}
	}
}
