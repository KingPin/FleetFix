// Package privilege answers "can this process act as root right now?"
//
// FleetFix never re-execs itself as root. Doing that would replace the
// operator's identity with root's for the rest of the session, and every audit
// record written afterwards would name the tool's privilege instead of the
// person who used it. Individual Tier 2 actions wrap their own command in
// `sudo -n` instead, which is why the question this package answers is asked
// repeatedly rather than once at startup.
//
// v1 asked it once, at compose time, and answered it wrongly. PrivilegeState
// derived can_tier2 from `is_root or sudo_available`, where sudo_available meant
// only that the sudo binary exists -- so on any host with sudo installed, Tier 2
// was reported reachable for a user with no sudo rights at all. The nav items
// appeared, the operator picked one, and the refusal arrived after they had
// typed the confirmation phrase. Here it is `sudo -n true` exiting zero, which
// is the same question the action itself will ask.
package privilege

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/report"
)

// ProbeTimeout bounds one `sudo -n true`. v1's value, kept: -n means sudo cannot
// prompt, so anything slower than this is a host problem -- an unreachable LDAP
// group lookup, a hung NSS module -- and a fleet tool that hung with it would
// turn a slow directory into a dead check run.
const ProbeTimeout = 2 * time.Second

// MemoTTL is how long an answer is reused.
//
// Short enough that an operator who runs `sudo -v` in another terminal sees the
// difference within a few seconds, long enough that a run of twenty Tier 2
// checks asks sudo once. Every entry also lands in auth.log, so asking per check
// would fill an operator's logs with the tool's own bookkeeping.
const MemoTTL = 10 * time.Second

// sudoProbe is the cheapest command that proves escalation works and changes
// nothing if it does.
var sudoProbe = []string{"-n", "true"}

// Prober answers the privilege question, memoising briefly.
//
// The zero value is not usable -- Runner is required. Use New.
type Prober struct {
	Runner cmdrun.Runner

	// UID is the effective user id this process is running as. A field rather
	// than a call to os.Geteuid so a test can be root without being root.
	UID int

	// Timeout and TTL default to ProbeTimeout and MemoTTL when zero.
	Timeout time.Duration
	TTL     time.Duration

	// Now defaults to time.Now. Injected so the memo's expiry is testable
	// without sleeping, which is the standing no-busy-wait rule applied to the
	// tests as well as the code.
	Now func() time.Time

	// mu is held across the probe itself, not just around the memo. Two
	// goroutines arriving together must produce one sudo call, not two: the
	// second waits and reads the answer the first just wrote. The probe is
	// bounded by Timeout, so the wait is too.
	mu   sync.Mutex
	memo *answer
}

type answer struct {
	ok     bool
	reason string
	at     time.Time
}

// New returns a Prober for this process.
func New(r cmdrun.Runner) *Prober {
	return &Prober{Runner: r, UID: os.Geteuid()}
}

// IsRoot reports whether this process is already root, in which case nothing
// needs escalating.
func (p *Prober) IsRoot() bool { return p.UID == 0 }

// CanTier2 reports whether a Tier 2 action would run right now without
// prompting. This is the function internal/check.Runner gates on.
func (p *Prober) CanTier2(ctx context.Context) bool {
	ok, _ := p.probe(ctx)
	return ok
}

// Reason explains the answer in one operator-facing line, for `fleetfix doctor`
// and for the report envelope. "sudo is not installed" and "sudo -n was refused"
// are the same false to a gate and entirely different problems to a person.
func (p *Prober) Reason(ctx context.Context) string {
	_, reason := p.probe(ctx)
	return reason
}

// State is the privilege block of the report envelope.
//
// Returns report.Privilege rather than a struct of its own, for the reason
// hostinfo does: two of these four fields are booleans about escalation, and a
// mapping function between two identical shapes is one transposition away from
// reporting a locked-down host as able to act as root.
func (p *Prober) State(ctx context.Context) report.Privilege {
	ok, reason := p.probe(ctx)
	return report.Privilege{
		UID:      p.UID,
		IsRoot:   p.IsRoot(),
		CanTier2: ok,
		Reason:   reason,
	}
}

func (p *Prober) probe(ctx context.Context) (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	if p.memo != nil && now.Sub(p.memo.at) < p.ttl() {
		return p.memo.ok, p.memo.reason
	}

	ok, reason := p.ask(ctx)
	p.memo = &answer{ok: ok, reason: reason, at: now}
	return ok, reason
}

// ask runs the probe. Never memoised itself; probe owns that.
func (p *Prober) ask(ctx context.Context) (bool, string) {
	// Root does not shell out. `sudo -n true` as root succeeds and tells us
	// nothing we did not already know, at the cost of an auth.log entry per
	// probe on every root-run cron invocation in the fleet.
	if p.IsRoot() {
		return true, "running as root"
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()

	res, err := p.Runner.Run(ctx, "sudo", sudoProbe...)
	switch {
	case cmdrun.IsNotFound(err):
		return false, "sudo is not installed, so Tier 2 actions cannot escalate"
	case cmdrun.IsTimeout(err):
		return false, fmt.Sprintf("sudo did not answer within %s", p.timeout())
	case err != nil:
		return false, fmt.Sprintf("sudo could not be run: %v", err)
	case res.OK():
		return true, "sudo -n succeeded, so Tier 2 actions will not prompt"
	}
	// Exited non-zero. sudo does not distinguish "no cached credential" from "no
	// sudo rights" in its exit status, and guessing between them would put a
	// wrong instruction in front of an operator who is already blocked.
	return false, "sudo -n was refused: no cached credential, or this user has no sudo rights"
}

func (p *Prober) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

func (p *Prober) timeout() time.Duration {
	if p.Timeout <= 0 {
		return ProbeTimeout
	}
	return p.Timeout
}

func (p *Prober) ttl() time.Duration {
	if p.TTL <= 0 {
		return MemoTTL
	}
	return p.TTL
}
