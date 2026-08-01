package check

import (
	"context"
	"strings"
	"testing"
)

// fake is a check whose behaviour a test dictates. Run defaults to reporting ok,
// so a test about selection does not have to say anything about running.
type fake struct {
	spec Spec
	run  func(ctx context.Context, in Input) Result
}

func (f fake) Spec() Spec { return f.spec }

func (f fake) Run(ctx context.Context, in Input) Result {
	if f.run == nil {
		return Result{Status: StatusOK, Summary: string(f.spec.ID) + " is fine"}
	}
	return f.run(ctx, in)
}

// stub builds a default-set check with the given id, taking its domain from the
// id so every stub is selectable both ways.
func stub(id ID) fake {
	domain, _, _ := strings.Cut(string(id), ".")
	return fake{spec: Spec{
		ID: id, Title: string(id), Domain: domain, InDefault: true,
	}}
}

func registry(t *testing.T, checks ...Check) *Registry {
	t.Helper()
	r := NewRegistry()
	for _, c := range checks {
		if err := r.Register(c); err != nil {
			t.Fatalf("registering %s: %v", c.Spec().ID, err)
		}
	}
	return r
}

// All three rejections are defects that would otherwise be invisible at runtime.
func TestRegisterRefusesWhatWouldBeInvisibleLater(t *testing.T) {
	r := registry(t, stub("disk.usage"))

	// A duplicate silently shadows one of the two, and which one depends on
	// registration order.
	if err := r.Register(stub("disk.usage")); err == nil {
		t.Error("a duplicate id registered")
	} else if !strings.Contains(err.Error(), "twice") {
		t.Errorf("the duplicate error does not say so: %v", err)
	}

	// A malformed spec produces a report field that is empty for reasons nobody
	// can trace.
	if err := r.Register(fake{spec: Spec{ID: "no.title", Domain: "no"}}); err == nil {
		t.Error("a spec with no title registered")
	}

	// Reusing a retired id keeps an operator's alert firing on a name that now
	// means something else -- and nothing errors, which is what makes it the worst
	// kind of break.
	Retired["disk.free"] = "replaced by disk.usage, which reports a percentage"
	t.Cleanup(func() { delete(Retired, "disk.free") })
	err := r.Register(stub("disk.free"))
	if err == nil {
		t.Fatal("a retired id registered")
	}
	for _, want := range []string{"retired", "disk.usage"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the retired error does not mention %q: %v", want, err)
		}
	}

	if r.Len() != 1 {
		t.Errorf("Len() = %d after three rejections, want 1", r.Len())
	}
}

func TestMustRegisterPanicsSoAMisbuiltBinaryDoesNotBoot(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustRegister accepted a malformed check")
		}
	}()
	NewRegistry().MustRegister(fake{spec: Spec{ID: "bad"}})
}

// Registration order is whatever the wiring happens to be; the report is asserted
// byte-stable, so everything the registry hands out is sorted.
func TestTheRegistryHandsEverythingBackSorted(t *testing.T) {
	r := registry(t, stub("net.ladder"), stub("disk.usage"), stub("disk.inodes"))

	want := []ID{"disk.inodes", "disk.usage", "net.ladder"}
	for i, c := range r.All() {
		if c.Spec().ID != want[i] {
			t.Errorf("All()[%d] = %s, want %s", i, c.Spec().ID, want[i])
		}
	}
	for i, s := range r.Specs() {
		if s.ID != want[i] {
			t.Errorf("Specs()[%d] = %s, want %s", i, s.ID, want[i])
		}
	}
	domains := r.Domains()
	if len(domains) != 2 || domains[0] != "disk" || domains[1] != "net" {
		t.Errorf("Domains() = %v, want [disk net]", domains)
	}
}

func TestSelectResolvesIdsDomainsAndExclusions(t *testing.T) {
	slow := stub("net.traceroute")
	slow.spec.InDefault = false
	r := registry(t, stub("disk.usage"), stub("disk.inodes"), stub("net.ladder"), slow)

	for name, tc := range map[string]struct {
		include, exclude []string
		want             []ID
	}{
		// A bare `fleetfix check`: everything marked InDefault, and nothing slow.
		"nothing named": {
			nil, nil,
			[]ID{"disk.inodes", "disk.usage", "net.ladder"},
		},
		"an exact id": {
			[]string{"disk.usage"},
			nil,
			[]ID{"disk.usage"},
		},
		"a whole domain": {
			[]string{"disk"},
			nil,
			[]ID{"disk.inodes", "disk.usage"},
		},
		// Naming a check explicitly overrides InDefault: that is the whole point of
		// marking something slow rather than deleting it.
		"a check that is not in the default set": {
			[]string{"net.traceroute"},
			nil,
			[]ID{"net.traceroute"},
		},
		"two selectors of different kinds": {
			[]string{"disk", "net.ladder"},
			nil,
			[]ID{"disk.inodes", "disk.usage", "net.ladder"},
		},
		"the same check twice": {
			[]string{"disk.usage", "disk.usage", "disk"},
			nil,
			[]ID{"disk.inodes", "disk.usage"},
		},
		"excluding from the default set": {
			nil,
			[]string{"disk"},
			[]ID{"net.ladder"},
		},
		// Exclude is applied last, so it wins. An operator writing both means
		// "this group except that one", which is the only reading that is useful.
		"exclude beats include": {
			[]string{"disk"},
			[]string{"disk.inodes"},
			[]ID{"disk.usage"},
		},
		"excluding everything": {
			[]string{"disk"},
			[]string{"disk"},
			nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := r.Select(tc.include, tc.exclude)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("selected %v, want %v", ids(got), tc.want)
			}
			for i, id := range tc.want {
				if got[i].Spec().ID != id {
					t.Errorf("selected[%d] = %s, want %s", i, got[i].Spec().ID, id)
				}
			}
		})
	}
}

// The decision this test exists for: a warning here would run nothing, find
// nothing wrong and exit 0, so a typo in an Ansible play would turn into a
// permanently green host.
func TestAnUnmatchedSelectorIsAnErrorAndNotAnEmptyGreenRun(t *testing.T) {
	r := registry(t, stub("disk.usage"), stub("net.ladder"))

	for name, sel := range map[string]string{
		"a typo'd id":     "dsk.usage",
		"a typo'd domain": "dis",
		"a plain miss":    "kubernetes",
		"empty":           "  ",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Select([]string{sel}, nil); err == nil {
				t.Fatalf("--check %q was accepted", sel)
			}
			// The same reasoning applies to --exclude: excluding a check that does
			// not exist means the operator thinks something is being skipped and it
			// is not.
			if _, err := r.Select(nil, []string{sel}); err == nil {
				t.Fatalf("--exclude %q was accepted", sel)
			}
		})
	}
}

// The selector that fails is almost always a typo of one that would have worked,
// and sending the operator to --list for a dropped letter is a poor use of their
// afternoon.
func TestAnUnmatchedSelectorOffersTheNearMiss(t *testing.T) {
	r := registry(t, stub("disk.usage"), stub("disk.inodes"), stub("net.ladder"))

	for sel, want := range map[string]string{
		"disk.usag":   "disk.usage", // a dropped letter
		"disk.usagee": "disk.usage", // a doubled one
		"disk.usagf":  "disk.usage", // a neighbouring key
		"dis":         "disk",       // the domain, not the id
	} {
		_, err := r.Select([]string{sel}, nil)
		if err == nil {
			t.Fatalf("%q was accepted", sel)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error does not suggest %q: %v", sel, want, err)
		}
	}

	// Every check in a domain nominates that domain, so "did you mean disk, disk?"
	// is the shape this goes wrong in.
	_, dup := r.Select([]string{"dis"}, nil)
	if dup == nil {
		t.Fatal("dis was accepted")
	}
	if n := strings.Count(dup.Error(), "disk"); n != 1 {
		t.Errorf("the domain is suggested %d times: %v", n, dup)
	}

	// A looser threshold would start suggesting disk.usage for net.ladder, which
	// is worse than saying nothing.
	_, err := r.Select([]string{"kubernetes"}, nil)
	if err == nil {
		t.Fatal("kubernetes was accepted")
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Errorf("a selector with no near miss got a suggestion: %v", err)
	}
	// Whatever else it says, it has to say where the real list is.
	if !strings.Contains(err.Error(), "--list") {
		t.Errorf("the error does not point at --list: %v", err)
	}
}

// An empty registry is a real state -- a build with no collectors linked -- and
// Select must not invent anything for it.
func TestSelectOnAnEmptyRegistry(t *testing.T) {
	r := NewRegistry()
	got, err := r.Select(nil, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("Select on an empty registry = %v, %v", ids(got), err)
	}
	if _, err := r.Select([]string{"disk"}, nil); err == nil {
		t.Error("an empty registry matched a selector")
	}
}

func ids(checks []Check) []ID {
	out := make([]ID, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.Spec().ID)
	}
	return out
}
