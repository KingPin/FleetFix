package core_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// forbidden is what internal/core must not reach, as import-path prefixes.
//
// The front doors and the terminal, for two different reasons. A parser that
// imports internal/check cannot then be called *by* a check without an import
// cycle, so the dependency has to run one way. And a parser that reaches tview
// drags a terminal library into `check --json` and the agent, neither of which has
// a terminal -- which is the whole reason the boundary exists rather than being a
// tidiness preference.
var forbidden = []string{
	"github.com/KingPin/FleetFix/v2/internal/tui",
	"github.com/KingPin/FleetFix/v2/internal/cli",
	"github.com/KingPin/FleetFix/v2/internal/check",
	"github.com/KingPin/FleetFix/v2/internal/agent",
	"github.com/KingPin/FleetFix/v2/internal/action",
	"github.com/rivo/tview",
	"github.com/gdamore/tcell",
}

// TestCoreDependsOnNothingAboveIt asks the question transitively, which is the
// part review and depguard both miss.
//
// depguard refuses these imports file by file, so it catches internal/core naming
// one of them directly. What it cannot catch is internal/core importing some
// package that is itself perfectly entitled to import tview -- the rule holds for
// every file and is still violated in aggregate. `go list -deps` is the only cheap
// answer to "and everything they pull in, too".
func TestCoreDependsOnNothingAboveIt(t *testing.T) {
	// Run from this package's directory, so ./... is internal/core and everything
	// under it. -deps lists the transitive imports of those packages and, without
	// -test, leaves this test file's own out of the answer.
	out, err := exec.Command("go", "list", "-deps", "./...").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("go list -deps: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list -deps: %v", err)
	}

	deps := strings.Fields(string(out))
	if len(deps) == 0 {
		t.Fatal("go list -deps returned nothing, so this test proved nothing")
	}
	for _, dep := range deps {
		for _, bad := range forbidden {
			if dep == bad || strings.HasPrefix(dep, bad+"/") {
				t.Errorf("internal/core depends on %s; nothing here may reach above it", dep)
			}
		}
	}
}
