package netprobe

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
)

const ssListeners = `LISTEN 0      4096         0.0.0.0:22         0.0.0.0:*    users:(("sshd",pid=812,fd=3))
LISTEN 0      511          0.0.0.0:80         0.0.0.0:*    users:(("nginx",pid=1204,fd=6))
LISTEN 0      4096       127.0.0.1:5432       0.0.0.0:*
`

func TestSocketsListsWhatIsListening(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Stdout(ssListeners, "ss", "-tlnpH")

	socks, err := p.Sockets(context.Background())
	if err != nil {
		t.Fatalf("ss failed: %v", err)
	}
	if len(socks) != 3 {
		t.Fatalf("got %d sockets, want 3: %+v", len(socks), socks)
	}
	if socks[0].LocalPort != 22 || socks[0].ProcessName == nil || *socks[0].ProcessName != "sshd" {
		t.Errorf("first socket = %+v", socks[0])
	}
	// The third row has no users:(...) block, which is what an unprivileged run
	// sees for a socket it does not own. The port is still the finding.
	if socks[2].LocalPort != 5432 || socks[2].ProcessName != nil {
		t.Errorf("third socket = %+v, want a port with no owner", socks[2])
	}
}

// ss exits 1 when it cannot read the process table for some sockets, which is
// routine unprivileged, having already printed every row it could. Discarding
// those rows would report an empty listener list on every non-root run.
func TestSocketsKeepsTheRowsFromANonZeroExit(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Exit(1, ssListeners, "Cannot open netlink socket: Operation not permitted", "ss", "-tlnpH")

	socks, err := p.Sockets(context.Background())
	if err != nil {
		t.Fatalf("a non-zero exit became an error: %v", err)
	}
	if len(socks) != 3 {
		t.Fatalf("got %d sockets, want the rows ss managed to print", len(socks))
	}
}

// The whole reason this returns an error rather than v1's empty list: a host
// with no iproute2 must not read as a box with nothing listening.
func TestSocketsReportsAnAbsentBinaryRatherThanAnEmptyList(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Missing("ss", "-tlnpH")

	socks, err := p.Sockets(context.Background())
	if !errors.Is(err, cmdrun.ErrNotFound) {
		t.Fatalf("err = %v, want the missing-executable reason", err)
	}
	if socks != nil {
		t.Errorf("sockets = %+v, want nothing alongside the error", socks)
	}
}

func TestSocketsReportsNothingListeningAsAnEmptyNonNilList(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Stdout("", "ss", "-tlnpH")

	socks, err := p.Sockets(context.Background())
	if err != nil {
		t.Fatalf("ss failed: %v", err)
	}
	if socks == nil {
		t.Fatal("a nil slice marshals to null; the wire format wants []")
	}
	if len(socks) != 0 {
		t.Fatalf("got %+v", socks)
	}
}

func TestSocketsAppliesItsOwnTimeout(t *testing.T) {
	t.Parallel()
	p, _ := newTestProber(t)
	var seen time.Duration
	p.Run = runnerFunc(func(ctx context.Context, _ string, _ ...string) (cmdrun.Result, error) {
		if deadline, ok := ctx.Deadline(); ok {
			seen = time.Until(deadline)
		}
		return cmdrun.Result{}, nil
	})

	if _, err := p.Sockets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen <= 0 || seen > socketsTimeoutS*time.Second {
		t.Fatalf("deadline %v away, want just under %ds", seen, socketsTimeoutS)
	}
}
