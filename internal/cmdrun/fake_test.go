package cmdrun

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestFakeReturnsTheRegisteredResponse(t *testing.T) {
	f := NewFake().Stdout("PING 8.8.8.8\n", "ping", "-c", "3", "8.8.8.8")

	res, err := f.Run(t.Context(), "ping", "-c", "3", "8.8.8.8")
	if err != nil {
		t.Fatalf("Run errored: %v", err)
	}
	if res.Stdout != "PING 8.8.8.8\n" {
		t.Errorf("Stdout = %q", res.Stdout)
	}
	if !res.OK() {
		t.Errorf("ExitCode = %d, want 0 for Stdout()", res.ExitCode)
	}
	// Filled in by Run so a test does not repeat the argv inside the Result.
	if got := res.Command(); got != "ping -c 3 8.8.8.8" {
		t.Errorf("Command() = %q, want the argv as invoked", got)
	}
}

func TestFakeFailsLoudlyOnAnUnregisteredArgv(t *testing.T) {
	// The most important behaviour in the file. v1's fakes returned an empty string
	// for anything unrecognised, so a test with the wrong argv watched its parser
	// receive no input, report "unavailable", and then asserted that as correct --
	// the bug moved into the test's expectations.
	f := NewFake().Stdout("out", "df", "-P")

	_, err := f.Run(t.Context(), "df", "-Pk")
	if err == nil {
		t.Fatal("Run returned no error for an unregistered argv")
	}
	if !errors.Is(err, ErrUnexpectedCall) {
		t.Errorf("error %v does not wrap ErrUnexpectedCall", err)
	}
	// The message has to name what actually ran, or the test author is left
	// guessing which flag differs.
	if !strings.Contains(err.Error(), "df -Pk") {
		t.Errorf("error %q does not name the unmatched argv", err)
	}
}

func TestFakeExitRegistersANonZeroResult(t *testing.T) {
	f := NewFake().Exit(1, "summary", "100% packet loss", "ping", "-c", "1", "10.0.0.1")

	res, err := f.Run(t.Context(), "ping", "-c", "1", "10.0.0.1")
	if err != nil {
		t.Fatalf("Run errored on a non-zero registration: %v", err)
	}
	if res.ExitCode != 1 || res.OK() {
		t.Errorf("ExitCode = %d, OK() = %v, want 1 and false", res.ExitCode, res.OK())
	}
	if got := res.Combined(); got != "summary100% packet loss" {
		t.Errorf("Combined() = %q", got)
	}
}

func TestFakeFailAndMissing(t *testing.T) {
	sentinel := errors.New("boom")
	f := NewFake().
		Fail(sentinel, "curl", "-s", "https://example.test").
		Missing("smartctl", "-H", "/dev/sda")

	if _, err := f.Run(t.Context(), "curl", "-s", "https://example.test"); !errors.Is(err, sentinel) {
		t.Errorf("Run error = %v, want the registered sentinel", err)
	}

	_, err := f.Run(t.Context(), "smartctl", "-H", "/dev/sda")
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false; Missing must look like an absent tool", err)
	}
	if !strings.Contains(err.Error(), "smartctl") {
		t.Errorf("error %q does not name the tool", err)
	}
}

func TestFakeFallbackAnswersAnythingUnregistered(t *testing.T) {
	// Opt-in only, for a test that genuinely does not care what ran.
	f := NewFake()
	f.Fallback = &Result{Stdout: "whatever", ExitCode: 0}

	res, err := f.Run(t.Context(), "anything", "--at", "all")
	if err != nil {
		t.Fatalf("Run errored with a fallback set: %v", err)
	}
	if res.Stdout != "whatever" {
		t.Errorf("Stdout = %q, want the fallback", res.Stdout)
	}
	if res.Command() != "anything --at all" {
		t.Errorf("Command() = %q, want the argv as invoked, not the fallback's", res.Command())
	}
}

func TestFakeRegistrationsWinOverTheFallback(t *testing.T) {
	f := NewFake().Stdout("specific", "df", "-P")
	f.Fallback = &Result{Stdout: "generic"}

	res, err := f.Run(t.Context(), "df", "-P")
	if err != nil {
		t.Fatalf("Run errored: %v", err)
	}
	if res.Stdout != "specific" {
		t.Errorf("Stdout = %q, want the specific registration", res.Stdout)
	}
}

func TestFakeErrorsWinOverResponsesForTheSameArgv(t *testing.T) {
	// Registering both is a mistake in the test, not the fake. Resolving it toward
	// the error is the safer default: a test that meant to inject a failure and
	// silently got a success is the harder one to notice.
	sentinel := errors.New("registered failure")
	f := NewFake().Stdout("out", "df", "-P").Fail(sentinel, "df", "-P")

	if _, err := f.Run(t.Context(), "df", "-P"); !errors.Is(err, sentinel) {
		t.Errorf("Run error = %v, want the registered failure to win", err)
	}
}

func TestFakeRecordsCallsInOrderIncludingUnmatchedOnes(t *testing.T) {
	f := NewFake().Stdout("out", "first")

	if _, err := f.Run(t.Context(), "first"); err != nil {
		t.Fatalf("Run errored: %v", err)
	}
	// Recorded even though nothing matched -- otherwise a failing test cannot show
	// what the code actually tried to run, which is the whole diagnostic.
	if _, err := f.Run(t.Context(), "second", "--flag"); err == nil {
		t.Fatal("expected an error for the unregistered argv")
	}
	if _, err := f.Run(t.Context(), "first"); err != nil {
		t.Fatalf("Run errored: %v", err)
	}

	want := []string{"first", "second --flag", "first"}
	got := f.Calls()
	if len(got) != len(want) {
		t.Fatalf("Calls() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Calls() = %v, want %v", got, want)
		}
	}
	if f.Called("never", "run") {
		t.Error("Called() reported an argv that was never run")
	}
}

func TestFakeCallsReturnsACopy(t *testing.T) {
	f := NewFake().Stdout("out", "df", "-P")
	if _, err := f.Run(t.Context(), "df", "-P"); err != nil {
		t.Fatalf("Run errored: %v", err)
	}

	calls := f.Calls()
	calls[0] = "clobbered"
	if again := f.Calls(); again[0] != "df -P" {
		t.Errorf("Calls() = %v; an assertion mutated the recorded history", again)
	}
}

func TestFakeHonoursACancelledContext(t *testing.T) {
	// So a test can exercise budget handling without a real process, and get the
	// same shape of error the OS runner produces.
	f := NewFake().Stdout("out", "traceroute", "8.8.8.8")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := f.Run(ctx, "traceroute", "8.8.8.8")
	if err == nil {
		t.Fatal("Run returned no error under a cancelled context")
	}
	if !IsTimeout(err) {
		t.Errorf("IsTimeout(%v) = false, want true", err)
	}
	// Still recorded: a cancelled call was still attempted.
	if !f.Called("traceroute", "8.8.8.8") {
		t.Errorf("calls = %v, want the cancelled call recorded", f.Calls())
	}
}

func TestFakeIsSafeUnderConcurrentUse(t *testing.T) {
	// The collectors fan out across goroutines; a mutex here beats every caller
	// remembering that. Meaningful under -race, which CI runs.
	f := NewFake().Stdout("out", "df", "-P")

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.Run(t.Context(), "df", "-P"); err != nil {
				t.Errorf("Run errored: %v", err)
			}
			_ = f.Calls()
		}()
	}
	wg.Wait()

	if got := len(f.Calls()); got != 32 {
		t.Errorf("Calls() has %d entries, want 32", got)
	}
}

func TestFakePartialKeepsTheOutputAWrittenCommandAlreadyProduced(t *testing.T) {
	// Partial registers output AND an error for a command killed after writing.
	// The diagnostic is the partial output written before the kill.
	f := NewFake().Partial(Result{Stdout: "hop 1\nhop 2\n"}, context.DeadlineExceeded, "traceroute", "8.8.8.8")

	res, err := f.Run(t.Context(), "traceroute", "8.8.8.8")
	if err == nil {
		t.Fatal("Run returned no error for a Partial registration")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v is not context.DeadlineExceeded", err)
	}
	if res.Stdout != "hop 1\nhop 2\n" {
		t.Errorf("Stdout = %q, want the output written before the kill", res.Stdout)
	}
}
