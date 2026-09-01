package privilege

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/report"
)

// clock is a hand-wound time source. The memo's expiry is a property of the
// clock, not of how long a test is willing to sleep -- and sleeping to prove a
// TTL is the busy-wait this project does not do.
type clock struct{ t time.Time }

func (c *clock) now() time.Time       { return c.t }
func (c *clock) tick(d time.Duration) { c.t = c.t.Add(d) }

// prober builds a non-root Prober over a fake sudo, with a clock the test winds.
func prober(t *testing.T, f *cmdrun.Fake) (*Prober, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 7, 31, 22, 0, 0, 0, time.UTC)}
	return &Prober{Runner: f, UID: 1000, Now: c.now}, c
}

// works returns a fake whose sudo -n true succeeds.
func works() *cmdrun.Fake {
	return cmdrun.NewFake().Stdout("", "sudo", "-n", "true")
}

// refuses returns a fake whose sudo -n true exits non-zero, which is what a user
// with no cached credential and no NOPASSWD rule gets.
func refuses() *cmdrun.Fake {
	return cmdrun.NewFake().Exit(1, "", "sudo: a password is required\n", "sudo", "-n", "true")
}

// The v1 defect this package exists to fix. PrivilegeState derived can_tier2
// from `is_root or sudo_available`, and sudo_available meant only that the
// binary exists -- so a user with sudo installed and no rights was told Tier 2
// was reachable, and found out otherwise after typing the confirmation phrase.
func TestSudoBeingInstalledIsNotPermissionToUseIt(t *testing.T) {
	p, _ := prober(t, refuses())
	if p.CanTier2(context.Background()) {
		t.Error("a refused sudo -n was read as permission")
	}
}

func TestTheProbeIsTheSameQuestionTheActionWillAsk(t *testing.T) {
	f := works()
	p, _ := prober(t, f)
	p.CanTier2(context.Background())

	if !f.Called("sudo", "-n", "true") {
		t.Errorf("ran %v, want sudo -n true", f.Calls())
	}
}

func TestCanTier2(t *testing.T) {
	notInstalled := cmdrun.NewFake().Missing("sudo", "-n", "true")
	brokenHost := cmdrun.NewFake().Fail(errors.New("fork/exec: cannot allocate memory"), "sudo", "-n", "true")

	for name, tc := range map[string]struct {
		uid     int
		fake    *cmdrun.Fake
		want    bool
		mention string
	}{
		"root":              {0, cmdrun.NewFake(), true, "root"},
		"passwordless sudo": {1000, works(), true, "sudo -n"},
		"sudo refuses":      {1000, refuses(), false, "refused"},
		"no sudo at all":    {1000, notInstalled, false, "not installed"},
		"sudo will not run": {1000, brokenHost, false, "could not be run"},
	} {
		t.Run(name, func(t *testing.T) {
			p := &Prober{Runner: tc.fake, UID: tc.uid}
			if got := p.CanTier2(context.Background()); got != tc.want {
				t.Errorf("CanTier2() = %v, want %v", got, tc.want)
			}
			if reason := p.Reason(context.Background()); !strings.Contains(reason, tc.mention) {
				t.Errorf("reason %q does not mention %q", reason, tc.mention)
			}
		})
	}
}

// Root and a locked-down host are both a single boolean to the gate and entirely
// different problems to a person, which is what doctor prints and what the
// report carries.
func TestEveryReasonIsDistinctAndSaysSomething(t *testing.T) {
	seen := map[string]string{}
	for name, p := range map[string]*Prober{
		"root":              {Runner: cmdrun.NewFake(), UID: 0},
		"passwordless sudo": {Runner: works(), UID: 1000},
		"sudo refuses":      {Runner: refuses(), UID: 1000},
		"no sudo at all":    {Runner: cmdrun.NewFake().Missing("sudo", "-n", "true"), UID: 1000},
		"sudo times out": {
			Runner:  cmdrun.NewFake().Fail(context.DeadlineExceeded, "sudo", "-n", "true"),
			UID:     1000,
			Timeout: 250 * time.Millisecond,
		},
	} {
		reason := p.Reason(context.Background())
		if reason == "" {
			t.Errorf("%s has no reason", name)
		}
		if prev, dup := seen[reason]; dup {
			t.Errorf("%s and %s give the same reason %q", name, prev, reason)
		}
		seen[reason] = name
	}
}

// The timeout is in the message because "sudo did not answer" without a number
// leaves an operator wondering how long the tool waited.
func TestATimeoutSaysHowLongItWaited(t *testing.T) {
	p := &Prober{
		Runner:  cmdrun.NewFake().Fail(context.DeadlineExceeded, "sudo", "-n", "true"),
		UID:     1000,
		Timeout: 250 * time.Millisecond,
	}
	if got := p.Reason(context.Background()); !strings.Contains(got, "250ms") {
		t.Errorf("reason = %q, want the timeout in it", got)
	}
}

// Root already has every privilege there is, and a `sudo -n true` that proves it
// costs an auth.log entry on every root-run cron invocation in the fleet.
func TestRootNeverShellsOut(t *testing.T) {
	// A fake with nothing registered fails loudly on any call, which is the
	// assertion: reaching sudo at all is the bug.
	f := cmdrun.NewFake()
	p := &Prober{Runner: f, UID: 0}
	if !p.CanTier2(context.Background()) {
		t.Error("root cannot act as root")
	}
	if len(f.Calls()) != 0 {
		t.Errorf("root ran %v", f.Calls())
	}
}

// Twenty Tier 2 checks in one run must ask sudo once. Every ask lands in
// auth.log, and a tool that fills an operator's logs with its own bookkeeping
// gets its rules loosened.
func TestTheAnswerIsMemoisedWithinTheTTL(t *testing.T) {
	f := works()
	p, c := prober(t, f)

	for range 20 {
		if !p.CanTier2(context.Background()) {
			t.Fatal("the memo changed the answer")
		}
	}
	if n := len(f.Calls()); n != 1 {
		t.Errorf("asked sudo %d times, want 1", n)
	}

	// Reason reads the same memo rather than asking again.
	p.Reason(context.Background())
	p.State(context.Background())
	if n := len(f.Calls()); n != 1 {
		t.Errorf("Reason and State asked sudo again: %d calls", n)
	}

	c.tick(MemoTTL)
	p.CanTier2(context.Background())
	if n := len(f.Calls()); n != 2 {
		t.Errorf("the memo outlived its TTL: %d calls", n)
	}
}

// A negative is memoised too, or a run of twenty Tier 2 checks on a host without
// sudo pays twenty process launches to be told the same thing.
func TestARefusalIsMemoisedAsWellAsAGrant(t *testing.T) {
	f := refuses()
	p, _ := prober(t, f)
	for range 5 {
		p.CanTier2(context.Background())
	}
	if n := len(f.Calls()); n != 1 {
		t.Errorf("asked sudo %d times for the same refusal, want 1", n)
	}
}

// The memo exists so an operator who runs `sudo -v` in another terminal is
// noticed. Ten seconds, not the length of the session.
func TestTheAnswerCanChange(t *testing.T) {
	f := cmdrun.NewFake()
	f.Exit(1, "", "sudo: a password is required\n", "sudo", "-n", "true")
	p, c := prober(t, f)

	if p.CanTier2(context.Background()) {
		t.Fatal("a refusal was read as permission")
	}
	// The operator authenticates elsewhere.
	f.Stdout("", "sudo", "-n", "true")
	c.tick(MemoTTL + time.Second)
	if !p.CanTier2(context.Background()) {
		t.Error("the operator authenticated and the prober never noticed")
	}
}

// Two goroutines arriving together must produce one sudo call. Without the lock
// held across the probe itself, both miss the memo and both shell out -- two
// auth.log entries and, on a host with a slow directory, two four-second waits.
func TestConcurrentCallersProduceOneProbe(t *testing.T) {
	f := works()
	p, _ := prober(t, f)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.CanTier2(context.Background())
		}()
	}
	wg.Wait()

	if n := len(f.Calls()); n != 1 {
		t.Errorf("eight concurrent callers asked sudo %d times, want 1", n)
	}
}

// The four fields go to four different places in the envelope, and two of them
// are booleans about escalation -- a transposition compiles and reports a
// locked-down host as able to act as root.
func TestState(t *testing.T) {
	for name, tc := range map[string]struct {
		p    *Prober
		want report.Privilege
	}{
		"root": {
			&Prober{Runner: cmdrun.NewFake(), UID: 0},
			report.Privilege{UID: 0, IsRoot: true, CanTier2: true, Reason: "running as root"},
		},
		"an operator with sudo": {
			&Prober{Runner: works(), UID: 1000},
			report.Privilege{
				UID: 1000, IsRoot: false, CanTier2: true,
				Reason: "sudo -n succeeded, so Tier 2 actions will not prompt",
			},
		},
		"an operator without": {
			&Prober{Runner: refuses(), UID: 1000},
			report.Privilege{
				UID: 1000, IsRoot: false, CanTier2: false,
				Reason: "sudo -n was refused: no cached credential, or this user has no sudo rights",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.p.State(context.Background()); got != tc.want {
				t.Errorf("State()\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// A uid of zero is root and a uid of anything else is not, with no third case.
// Asserted separately because IsRoot is what decides whether sudo runs at all.
func TestIsRoot(t *testing.T) {
	for uid, want := range map[int]bool{0: true, 1: false, 1000: false, 65534: false} {
		p := &Prober{Runner: cmdrun.NewFake(), UID: uid}
		if got := p.IsRoot(); got != want {
			t.Errorf("uid %d: IsRoot() = %v, want %v", uid, got, want)
		}
	}
}

// The probe gets its own deadline rather than inheriting the caller's, so a
// check with a generous budget does not sit behind a hung NSS module for it.
func TestTheProbeBoundsItselfEvenWhenTheCallerDoesNot(t *testing.T) {
	var got time.Duration
	p := &Prober{
		UID:     1000,
		Timeout: 30 * time.Millisecond,
		Runner: runnerFunc(func(ctx context.Context, _ string, _ ...string) (cmdrun.Result, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				return cmdrun.Result{}, errors.New("no deadline was set on the probe")
			}
			got = time.Until(deadline)
			return cmdrun.Result{}, nil
		}),
	}
	p.CanTier2(context.Background())
	if got <= 0 || got > 30*time.Millisecond {
		t.Errorf("the probe's deadline is %v away, want at most its 30ms timeout", got)
	}
}

// A cancelled caller must not leave a memoised answer that outlives the reason
// it failed -- but it also must not hang. This asserts it returns at all, with
// the honest answer, which is what every other failure path gives.
func TestACancelledCallerGetsAnAnswerAndNotAHang(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &Prober{Runner: cmdrun.NewFake().Fail(context.Canceled, "sudo", "-n", "true"), UID: 1000}
	if p.CanTier2(ctx) {
		t.Error("a cancelled probe was read as permission")
	}
}

// The defaults are the constants, so a Prober built by New behaves like the
// documented one rather than like a zero-valued struct.
func TestNewTakesItsUIDFromTheProcessAndItsBoundsFromTheConstants(t *testing.T) {
	p := New(cmdrun.NewFake())
	if p.UID != os.Geteuid() {
		t.Errorf("uid = %d, want this process's %d", p.UID, os.Geteuid())
	}
	if p.timeout() != ProbeTimeout {
		t.Errorf("timeout = %v, want %v", p.timeout(), ProbeTimeout)
	}
	if p.ttl() != MemoTTL {
		t.Errorf("ttl = %v, want %v", p.ttl(), MemoTTL)
	}
	if p.now().IsZero() {
		t.Error("the default clock returns the zero time")
	}
	// A zero or negative field is "unset", not "no timeout at all" -- the latter
	// is how a hung directory lookup becomes a hung check run.
	zero := &Prober{Runner: cmdrun.NewFake(), Timeout: -1, TTL: -1}
	if zero.timeout() != ProbeTimeout || zero.ttl() != MemoTTL {
		t.Errorf("a negative bound was taken literally: %v / %v", zero.timeout(), zero.ttl())
	}

	// A positive one is honoured, or the fields would be decoration.
	set := &Prober{Runner: cmdrun.NewFake(), Timeout: time.Second, TTL: time.Minute}
	if set.timeout() != time.Second || set.ttl() != time.Minute {
		t.Errorf("an explicit bound was ignored: %v / %v", set.timeout(), set.ttl())
	}
}

// The TTL is a field, not just a constant, because the agent's cadence and a
// human's are not the same question. A short one re-asks; a long one does not.
func TestTheTTLIsHonouredRatherThanTheConstant(t *testing.T) {
	f := works()
	c := &clock{t: time.Date(2026, 7, 31, 22, 0, 0, 0, time.UTC)}
	p := &Prober{Runner: f, UID: 1000, Now: c.now, TTL: time.Hour}

	p.CanTier2(context.Background())
	c.tick(MemoTTL + time.Second)
	p.CanTier2(context.Background())
	if n := len(f.Calls()); n != 1 {
		t.Errorf("asked sudo %d times inside a one-hour TTL, want 1", n)
	}

	c.tick(time.Hour)
	p.CanTier2(context.Background())
	if n := len(f.Calls()); n != 2 {
		t.Errorf("the one-hour TTL never expired: %d calls", n)
	}
}

// runnerFunc adapts a function to cmdrun.Runner for the one test that needs to
// inspect the context rather than the argv.
type runnerFunc func(context.Context, string, ...string) (cmdrun.Result, error)

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) (cmdrun.Result, error) {
	return f(ctx, name, args...)
}
