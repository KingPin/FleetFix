package check

import (
	"fmt"
	"sort"
	"strings"
)

// A Registry is every check this build knows about.
//
// A value rather than a package-level default, because a process-wide registry
// populated by init() makes what runs depend on which packages happened to be
// linked, and makes a test that wants three checks fight whatever the rest of the
// binary registered. Each front door builds its own and hands it to a Runner.
type Registry struct {
	byID  map[ID]Check
	order []ID
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byID: map[ID]Check{}}
}

// Register adds a check.
//
// Every rejection here is a defect that would otherwise be invisible at runtime: a
// duplicate id silently shadows one of the two, a retired id resurrects a name an
// operator's alert still refers to, and a malformed spec produces a report field
// that is empty for reasons nobody can trace. Failing at startup makes all three a
// build-and-boot problem rather than a fleet-wide one.
func (r *Registry) Register(c Check) error {
	spec := c.Spec()
	if err := spec.Validate(); err != nil {
		return err
	}
	if reason, retired := Retired[spec.ID]; retired {
		return fmt.Errorf(
			"%s is retired (%s); reusing the id would keep an operator's alert firing on a name that now means something else",
			spec.ID, reason,
		)
	}
	if _, dup := r.byID[spec.ID]; dup {
		return fmt.Errorf("%s is registered twice", spec.ID)
	}
	r.byID[spec.ID] = c
	r.order = append(r.order, spec.ID)
	return nil
}

// MustRegister panics on a rejected check, for the wiring that runs at startup
// where there is nothing useful to do with the error and no report to put it in.
func (r *Registry) MustRegister(checks ...Check) {
	for _, c := range checks {
		if err := r.Register(c); err != nil {
			panic("check: " + err.Error())
		}
	}
}

// Len reports how many checks are registered.
func (r *Registry) Len() int { return len(r.byID) }

// All returns every registered check, ordered by id.
func (r *Registry) All() []Check {
	ids := append([]ID(nil), r.order...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]Check, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.byID[id])
	}
	return out
}

// Specs returns every spec, ordered by id. This is what `--list` prints and what
// `doctor` walks to report which external programs the build wants.
func (r *Registry) Specs() []Spec {
	checks := r.All()
	specs := make([]Spec, 0, len(checks))
	for _, c := range checks {
		specs = append(specs, c.Spec())
	}
	return specs
}

// Domains returns the distinct domains, sorted -- the selectors `--check disk` and
// friends, listed for an operator who does not know the individual ids.
func (r *Registry) Domains() []string {
	seen := map[string]struct{}{}
	for _, c := range r.byID {
		seen[c.Spec().Domain] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// Select resolves --check and --exclude into the checks to run.
//
// A selector is an exact id or a whole domain; "disk" selects every disk check and
// "disk.usage" selects the one. Empty include means the default set, which is what
// a bare `fleetfix check` runs.
//
// An unmatched selector is an error rather than a warning, and that is the
// important decision here. `check --json --check dsk.usage` with a warning would
// run nothing, find nothing wrong, and exit 0 -- a typo in an Ansible play turning
// into a permanently green host. Erroring makes the same typo loud, and the caller
// still emits a valid JSON document saying so.
func (r *Registry) Select(include, exclude []string) ([]Check, error) {
	chosen := map[ID]bool{}
	if len(include) == 0 {
		for id, c := range r.byID {
			if c.Spec().InDefault {
				chosen[id] = true
			}
		}
	}
	for _, sel := range include {
		matched, err := r.match(sel)
		if err != nil {
			return nil, err
		}
		for _, id := range matched {
			chosen[id] = true
		}
	}
	for _, sel := range exclude {
		matched, err := r.match(sel)
		if err != nil {
			return nil, err
		}
		for _, id := range matched {
			delete(chosen, id)
		}
	}

	out := make([]Check, 0, len(chosen))
	for _, c := range r.All() {
		if chosen[c.Spec().ID] {
			out = append(out, c)
		}
	}
	return out, nil
}

// match resolves one selector to the ids it names.
func (r *Registry) match(sel string) ([]ID, error) {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return nil, fmt.Errorf("an empty selector matches nothing; drop it or name a check")
	}
	if _, ok := r.byID[ID(sel)]; ok {
		return []ID{ID(sel)}, nil
	}
	var ids []ID
	for id, c := range r.byID {
		if c.Spec().Domain == sel {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no check or domain named %q; `fleetfix check --list` names them all%s", sel, r.suggest(sel))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// suggest offers the near miss, because the selector that fails is almost always a
// typo of one that would have worked and making the operator go and read --list
// for a dropped letter is a poor use of their afternoon.
func (r *Registry) suggest(sel string) string {
	var near []string
	for _, c := range r.All() {
		spec := c.Spec()
		for _, cand := range []string{string(spec.ID), spec.Domain} {
			if cand != sel && closeEnough(sel, cand) && !contains(near, cand) {
				near = append(near, cand)
			}
		}
	}
	if len(near) == 0 {
		return ""
	}
	return " (did you mean " + strings.Join(near, ", ") + "?)"
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// closeEnough is a one-edit check: an insertion, a deletion or a substitution.
//
// Deliberately not a full edit-distance implementation. One edit covers the typo
// that actually happens -- a dropped letter, a doubled one, a neighbouring key --
// and a looser threshold starts suggesting disk.usage for net.ladder, which is
// worse than saying nothing.
func closeEnough(a, b string) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(b)-len(a) > 1 {
		return false
	}
	for i := range len(a) {
		if a[i] == b[i] {
			continue
		}
		if len(a) == len(b) {
			return a[i+1:] == b[i+1:] // one substitution
		}
		return a[i:] == b[i+1:] // one insertion in b
	}
	return true
}
