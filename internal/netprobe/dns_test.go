package netprobe

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDNSReportsSortedUniqueAddressesAndLatency(t *testing.T) {
	t.Parallel()
	p, _ := newTestProber(t)
	p.Now = tickClock(3 * time.Millisecond)
	p.Lookup = &stubResolver{hosts: map[string][]string{
		// getaddrinfo returns one entry per socket type, so duplicates are the
		// normal case rather than a malformed answer.
		"github.com": {"140.82.121.4", "140.82.121.3", "140.82.121.4"},
	}}

	res := p.DNS(context.Background(), "github.com", 3)
	if !res.OK {
		t.Fatalf("lookup failed: %v", res.Error)
	}
	if len(res.Addresses) != 2 || res.Addresses[0] != "140.82.121.3" || res.Addresses[1] != "140.82.121.4" {
		t.Errorf("addresses = %v, want deduped and sorted", res.Addresses)
	}
	if res.LatencyMS != 3 {
		t.Errorf("latency = %vms, want one clock tick", res.LatencyMS)
	}
	if res.Error != nil {
		t.Errorf("error = %q on a successful lookup", *res.Error)
	}
}

// A failed lookup still reports how long it took to fail: a resolver that takes
// three seconds to say NXDOMAIN is a different problem from one that says it
// instantly, and the second number is the one that names it.
func TestDNSReportsTheFailureAndHowLongItTook(t *testing.T) {
	t.Parallel()
	p, _ := newTestProber(t)
	p.Now = tickClock(500 * time.Millisecond)
	p.Lookup = &stubResolver{err: errors.New("lookup nope.invalid: no such host")}

	res := p.DNS(context.Background(), "nope.invalid", 3)
	if res.OK {
		t.Fatal("a failed lookup reported ok")
	}
	if res.Error == nil || *res.Error != "lookup nope.invalid: no such host" {
		t.Errorf("error = %v", res.Error)
	}
	if res.LatencyMS != 500 {
		t.Errorf("latency = %vms", res.LatencyMS)
	}
	if res.Addresses == nil {
		t.Error("a nil slice marshals to null; the wire format wants []")
	}
	if res.Name != "nope.invalid" {
		t.Errorf("name = %q, want it echoed on the failure path", res.Name)
	}
}

// A name that resolves to nothing is not an error to the resolver, and it must not
// become one here: "resolved, no addresses" is a real and confusing DNS state that
// the operator needs reported as itself.
func TestDNSReportsAnEmptyAnswerAsASuccessWithNoAddresses(t *testing.T) {
	t.Parallel()
	p, _ := newTestProber(t)
	p.Lookup = &stubResolver{hosts: map[string][]string{"empty.example": {}}}

	res := p.DNS(context.Background(), "empty.example", 3)
	if !res.OK {
		t.Fatal("an empty answer was reported as a failed lookup")
	}
	if len(res.Addresses) != 0 || res.Addresses == nil {
		t.Errorf("addresses = %v, want an empty non-nil slice", res.Addresses)
	}
}

func TestDNSAppliesItsOwnTimeout(t *testing.T) {
	t.Parallel()
	p, _ := newTestProber(t)
	var seen time.Duration
	p.Lookup = resolverFunc(func(ctx context.Context, _ string) ([]string, error) {
		if deadline, ok := ctx.Deadline(); ok {
			seen = time.Until(deadline)
		}
		return []string{"192.0.2.1"}, nil
	})

	p.DNS(context.Background(), "github.com", 3)
	if seen <= 0 || seen > 3*time.Second {
		t.Fatalf("deadline %v away, want just under 3s", seen)
	}
}

func TestSortedUniqueLeavesAnEmptyInputEmptyAndNonNil(t *testing.T) {
	t.Parallel()
	got := sortedUnique(nil)
	if got == nil {
		t.Fatal("nil in, nil out: the wire format wants []")
	}
	if len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}
