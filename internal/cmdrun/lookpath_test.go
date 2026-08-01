package cmdrun

import (
	"strings"
	"sync"
	"testing"
)

// Every collector already branches on ErrNotFound from Run. A Looker that reported
// a different error would make "docker is not installed" two conditions to handle
// instead of one, and the second one would get missed.
func TestALookerReportsTheSameAbsenceARunDoes(t *testing.T) {
	_, err := NewPATH().Look("fleetfix-no-such-program-exists")
	if err == nil {
		t.Fatal("a program that cannot exist was found")
	}
	if !IsNotFound(err) {
		t.Errorf("error %v is not ErrNotFound, so a caller has two absences to handle", err)
	}
	if !strings.Contains(err.Error(), "fleetfix-no-such-program-exists") {
		t.Errorf("error %v does not name the program", err)
	}
}

func TestALookerFindsSomethingThatIsThere(t *testing.T) {
	// sh is required by POSIX and present in every image the smoke matrix runs.
	path, err := NewPATH().Look("sh")
	if err != nil {
		t.Fatalf("sh was not found: %v", err)
	}
	if path == "" {
		t.Error("sh was found at no path")
	}
}

// The presence of docker cannot change during one `check --json`, and asking twice
// invites a report that says docker is unavailable in one check and grades
// containers in another -- an inconsistency an operator would spend an afternoon
// on.
func TestALookerAnswersTheSameWayTwice(t *testing.T) {
	p := NewPATH()
	for _, name := range []string{"sh", "fleetfix-no-such-program-exists"} {
		firstPath, firstErr := p.Look(name)
		for range 5 {
			gotPath, gotErr := p.Look(name)
			if gotPath != firstPath || (gotErr == nil) != (firstErr == nil) {
				t.Errorf("%s: answer changed between calls", name)
			}
		}
	}
}

// Checks fan out across goroutines, and several of them ask about the same tool.
func TestALookerIsSafeUnderConcurrentUse(t *testing.T) {
	p := NewPATH()
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Look("sh")
			_, _ = p.Look("fleetfix-no-such-program-exists")
		}()
	}
	wg.Wait()
}

// The interesting host is the one missing a tool, so the fake names what is
// present: a test that enumerated what is absent would drift as checks grow.
func TestFakeLookerInstallsOnlyWhatItWasGiven(t *testing.T) {
	f := NewFakeLooker("docker", "systemctl")
	for _, name := range []string{"docker", "systemctl"} {
		if _, err := f.Look(name); err != nil {
			t.Errorf("%s was named as installed and was not found: %v", name, err)
		}
	}
	if _, err := f.Look("smartctl"); !IsNotFound(err) {
		t.Errorf("smartctl was not named and did not report absent: %v", err)
	}
	want := []string{"docker", "systemctl", "smartctl"}
	got := f.Looked()
	if len(got) != len(want) {
		t.Fatalf("Looked() = %v, want %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("Looked()[%d] = %s, want %s", i, got[i], name)
		}
	}
}

func TestLookerFuncAdaptsAFunction(t *testing.T) {
	var l Looker = LookerFunc(func(name string) (string, error) { return "/x/" + name, nil })
	got, err := l.Look("df")
	if err != nil || got != "/x/df" {
		t.Errorf("Look = %q, %v", got, err)
	}
}
