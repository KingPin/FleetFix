package cmdrun

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrUnexpectedCall is what Fake returns for an argv nothing registered.
//
// Erroring is the whole point. v1's subprocess fakes mostly returned an empty
// string for anything they did not recognise, so a test that got the argv wrong
// -- a flag reordered, a unit suffix dropped -- watched the parser receive no
// input and report "unavailable", which several tests then asserted as correct
// behaviour. The bug moved into the test's expectations. A loud failure naming
// the unmatched argv keeps a wrong-command bug a wrong-command bug.
var ErrUnexpectedCall = errors.New("cmdrun: unexpected command")

// Fake is a Runner for tests, keyed on the joined argv.
//
// One fake replaces the five incompatible subprocess-faking styles in the Python
// suite (module-level monkeypatch, a stub class, a lambda, patch.object, and a
// conftest fixture), which is what made Tier B of the test corpus impossible to
// port mechanically. Keying on the joined argv means a test states the exact
// command it expects, so the argv is under test too, not just the parsing.
//
// Safe for concurrent use: the collectors that will drive it fan out across
// goroutines, and a mutex here is cheaper than every caller remembering that.
type Fake struct {
	mu        sync.Mutex
	responses map[string]Result
	errs      map[string]error
	calls     []string

	// Fallback answers any argv with no registered response. Nil by default, so
	// an unregistered call fails loudly; set it only for a test that genuinely
	// does not care what ran.
	Fallback *Result
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{
		responses: make(map[string]Result),
		errs:      make(map[string]error),
	}
}

// Respond registers a full Result for an argv. Result.Args is filled in by Run,
// so a caller does not repeat the argv inside the Result.
func (f *Fake) Respond(res Result, name string, args ...string) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[Key(name, args...)] = res
	return f
}

// Stdout registers a zero-exit response whose stdout is out. The common case:
// the tool ran, printed, and succeeded.
func (f *Fake) Stdout(out, name string, args ...string) *Fake {
	return f.Respond(Result{Stdout: out}, name, args...)
}

// Exit registers a response with a non-zero exit code and stderr, for the tools
// whose exit code is data -- ping on total loss, smartctl's status bits.
func (f *Fake) Exit(code int, stdout, stderr, name string, args ...string) *Fake {
	return f.Respond(Result{Stdout: stdout, Stderr: stderr, ExitCode: code}, name, args...)
}

// Fail registers an error for an argv: ErrNotFound for an absent tool,
// context.DeadlineExceeded for one that blew its budget.
func (f *Fake) Fail(err error, name string, args ...string) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[Key(name, args...)] = err
	return f
}

// Missing registers an argv as an executable that is not installed.
func (f *Fake) Missing(name string, args ...string) *Fake {
	return f.Fail(fmt.Errorf("%w: %s", ErrNotFound, name), name, args...)
}

// Run implements Runner.
func (f *Fake) Run(ctx context.Context, name string, args ...string) (Result, error) {
	key := Key(name, args...)
	argv := append([]string{name}, args...)

	f.mu.Lock()
	f.calls = append(f.calls, key)
	err, hasErr := f.errs[key]
	res, hasRes := f.responses[key]
	fallback := f.Fallback
	f.mu.Unlock()

	// Honour an already-cancelled context so a test can exercise budget handling
	// without a real process, matching what the OS runner does.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{Args: argv}, fmt.Errorf("cmdrun: %s: %w", key, ctxErr)
	}

	switch {
	case hasErr:
		return Result{Args: argv}, err
	case hasRes:
		res.Args = argv
		return res, nil
	case fallback != nil:
		out := *fallback
		out.Args = argv
		return out, nil
	}
	return Result{Args: argv}, fmt.Errorf("%w: %s", ErrUnexpectedCall, key)
}

// Calls returns the argv keys Run received, in order, including ones that had no
// registered response. A copy, so an assertion cannot mutate the record.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Called reports whether an argv was run.
func (f *Fake) Called(name string, args ...string) bool {
	want := Key(name, args...)
	for _, got := range f.Calls() {
		if got == want {
			return true
		}
	}
	return false
}
