package cmdrun

import (
	"fmt"
	"os/exec"
	"sync"
)

// notFound is the one spelling of "that program is not here", so a caller can
// branch on IsNotFound whether it asked a Looker or ran the command outright.
func notFound(name string) error { return fmt.Errorf("%w: %s", ErrNotFound, name) }

// A Looker answers "is this program installed?".
//
// Separate from Runner because the answer is needed before a command is run, not
// as a consequence of running one: a check that shells out to smartctl reports
// unavailable when smartctl is absent, and finding that out by running it would
// mean every such check writing the same ErrNotFound branch. Its own interface, so
// a test can stage a host without docker without also staging what docker would
// have said.
type Looker interface {
	// Look returns the resolved path, or ErrNotFound wrapped with the name.
	Look(name string) (string, error)
}

// LookerFunc adapts a function to Looker.
type LookerFunc func(string) (string, error)

// Look calls f.
func (f LookerFunc) Look(name string) (string, error) { return f(name) }

// PATH is the real thing: exec.LookPath, with the answer remembered.
//
// Caching is not an optimisation. The presence of docker cannot change during one
// `check --json`, and asking twice invites a report that says docker is
// unavailable in one check and grades containers in another -- an inconsistency an
// operator would spend an afternoon on. The TUI is long-running, so it gets a
// fresh PATH per invocation of NewPATH rather than a process-wide cache.
type PATH struct {
	mu   sync.Mutex
	seen map[string]lookResult
}

type lookResult struct {
	path string
	err  error
}

// NewPATH returns a Looker backed by exec.LookPath.
func NewPATH() *PATH { return &PATH{seen: map[string]lookResult{}} }

// Look implements Looker.
func (p *PATH) Look(name string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if got, ok := p.seen[name]; ok {
		return got.path, got.err
	}
	path, err := exec.LookPath(name)
	if err != nil {
		// Collapsed onto the sentinel every collector already branches on, so
		// "not installed" and "found but not executable" read the same to a
		// caller whose only question is whether it can shell out.
		err = notFound(name)
	}
	p.seen[name] = lookResult{path, err}
	return path, err
}

// FakeLooker is a Looker for tests: the programs it names are installed and
// nothing else is.
//
// Present-by-list rather than absent-by-list, because the interesting host is the
// one missing a tool. A test that has to enumerate what is absent will drift as
// checks grow; a test that names what is present stays honest.
type FakeLooker struct {
	mu        sync.Mutex
	installed map[string]bool
	looked    []string
}

// NewFakeLooker returns a Looker on which only the named programs exist.
func NewFakeLooker(installed ...string) *FakeLooker {
	f := &FakeLooker{installed: make(map[string]bool, len(installed))}
	for _, name := range installed {
		f.installed[name] = true
	}
	return f
}

// Look implements Looker.
func (f *FakeLooker) Look(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.looked = append(f.looked, name)
	if !f.installed[name] {
		return "", notFound(name)
	}
	return "/usr/bin/" + name, nil
}

// Looked returns the names asked about, in order. A copy, so an assertion cannot
// mutate the record.
func (f *FakeLooker) Looked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.looked...)
}
