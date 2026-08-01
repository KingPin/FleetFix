package config

import (
	"strings"
	"testing"
)

func TestPrincipalsReadsTheTable(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.UserDir, IdentityFile, strings.Join([]string{
		"principals:",
		"  alice: alice@corp.example",
		"  bob: bob@corp.example",
		"",
	}, "\n"))

	got, loaded := p.Principals()
	if len(loaded.Warnings) != 0 {
		t.Errorf("a valid file produced warnings: %v", loaded.Warnings)
	}
	for user, want := range map[string]string{"alice": "alice@corp.example", "bob": "bob@corp.example"} {
		if got[user] != want {
			t.Errorf("%s = %q, want %q", user, got[user], want)
		}
	}
}

// Most hosts have no identity.yml. That is the common case, not a degradation:
// the principal then comes from FLEETFIX_AUTH_PRINCIPAL or is empty.
func TestAnAbsentFileIsAnEmptyTableAndNotAWarning(t *testing.T) {
	got, loaded := testPaths(t).Principals()
	if len(got) != 0 {
		t.Errorf("got %v from a host with no identity.yml", got)
	}
	if got == nil {
		t.Error("the table is nil, so a caller indexing it has to check first")
	}
	if len(loaded.Warnings) != 0 {
		t.Errorf("an absent optional file warned: %v", loaded.Warnings)
	}
	// doctor's question is "which files are in play?", which a list of only the
	// files that existed cannot answer.
	if len(loaded.Sources) != 2 {
		t.Errorf("Sources = %v, want both candidate layers named", loaded.Sources)
	}
}

// The whole point of the fleet layer: Ansible names everyone in /etc and a
// personal file corrects one entry without restating the rest.
func TestTheUserLayerCorrectsOneEntry(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.SystemDir, IdentityFile,
		"principals:\n  alice: alice@old.example\n  bob: bob@corp.example\n")
	writeYAML(t, p.UserDir, IdentityFile,
		"principals:\n  alice: alice@corp.example\n")

	got, loaded := p.Principals()
	if len(loaded.Warnings) != 0 {
		t.Errorf("warnings: %v", loaded.Warnings)
	}
	if got["alice"] != "alice@corp.example" {
		t.Errorf("alice = %q, want the personal file's value", got["alice"])
	}
	if got["bob"] != "bob@corp.example" {
		t.Errorf("bob = %q, want the fleet file's value; a partial override wiped the table", got["bob"])
	}
}

// Every rejection names what the operator wrote. A dropped entry means an action
// recorded against a unix login when they believed it would carry their SSO
// identity, and that is only findable if the file said so.
func TestEveryRejectionIsAWarningThatNamesIt(t *testing.T) {
	for name, tc := range map[string]struct {
		body    string
		mention []string
		want    map[string]string
	}{
		"a misspelled top-level key": {
			"principal:\n  alice: alice@corp.example\n",
			[]string{"principal"},
			map[string]string{},
		},
		"principals is not a mapping": {
			"principals:\n  - alice\n  - bob\n",
			[]string{"principals", "mapping"},
			map[string]string{},
		},
		"principals is a scalar": {
			"principals: alice\n",
			[]string{"principals", "mapping"},
			map[string]string{},
		},
		"an entry with no value": {
			"principals:\n  alice:\n  bob: bob@corp.example\n",
			[]string{"alice"},
			map[string]string{"bob": "bob@corp.example"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := testPaths(t)
			writeYAML(t, p.UserDir, IdentityFile, tc.body)
			got, loaded := p.Principals()

			joined := strings.Join(loaded.Warnings, "\n")
			if len(loaded.Warnings) == 0 {
				t.Fatalf("no warning; the entry vanished silently. got %v", got)
			}
			for _, word := range append([]string{IdentityFile}, tc.mention...) {
				if !strings.Contains(joined, word) {
					t.Errorf("the warning does not mention %q: %s", word, joined)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for user, want := range tc.want {
				if got[user] != want {
					t.Errorf("%s = %q, want %q", user, got[user], want)
				}
			}
		})
	}
}

// `alice:` with nothing after it is None once PyYAML is done with it, and PyStr
// renders None as "None" -- a principal no directory has ever issued.
func TestAnEmptyEntryIsNotTheStringNone(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.UserDir, IdentityFile, "principals:\n  alice:\n")
	got, _ := p.Principals()
	if v, ok := got["alice"]; ok {
		t.Errorf("alice = %q, want the entry dropped", v)
	}
}

// An unquoted employee number is an int by the time PyYAML is done with it, and
// dropping it for not being a string would be the same silent loss the warnings
// above exist to prevent.
func TestANonStringPrincipalIsRenderedRatherThanDropped(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.UserDir, IdentityFile, "principals:\n  alice: 100427\n  bob: true\n")
	got, loaded := p.Principals()
	if len(loaded.Warnings) != 0 {
		t.Errorf("warnings: %v", loaded.Warnings)
	}
	if got["alice"] != "100427" {
		t.Errorf("alice = %q, want 100427", got["alice"])
	}
	if got["bob"] != "True" {
		t.Errorf("bob = %q, want Python's True -- PyStr is the vocabulary here", got["bob"])
	}
}

// Warnings travel in the report, which is asserted byte-stable across
// consecutive runs, so two runs over the same broken file must say the same
// thing in the same order.
func TestWarningsAreOrderedTheSameWayEveryRun(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.UserDir, IdentityFile, strings.Join([]string{
		"principals:",
		"  zoe:",
		"  alice:",
		"  mallory:",
		"  bob:",
		"",
	}, "\n"))

	_, a := p.Principals()
	_, b := p.Principals()
	if strings.Join(a.Warnings, "\n") != strings.Join(b.Warnings, "\n") {
		t.Errorf("two runs disagree:\n%v\n%v", a.Warnings, b.Warnings)
	}
	if len(a.Warnings) != 4 {
		t.Fatalf("want one warning per bad entry, got %v", a.Warnings)
	}
	for i, user := range []string{"alice", "bob", "mallory", "zoe"} {
		if !strings.Contains(a.Warnings[i], user) {
			t.Errorf("warning %d is %q, want it to be about %s", i, a.Warnings[i], user)
		}
	}
}

// A file that will not parse contributes nothing and warns. A fleet tool that
// stopped naming operators because of a stray tab would be worse than one
// naming them by unix login.
func TestAFileThatWillNotParseIsAWarningAndNotAFailure(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.UserDir, IdentityFile, "principals:\n  alice: [unclosed\n")
	got, loaded := p.Principals()
	if len(got) != 0 {
		t.Errorf("got %v from an unparseable file", got)
	}
	if len(loaded.Warnings) == 0 {
		t.Error("an unparseable file was silent")
	}
}
