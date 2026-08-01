package storage

import (
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// wantCategory is _classify's answer for each line of storage/stale_names.txt, in
// file order, measured by running v1's own function against v1's own two glob
// lists. "" is v1's None.
var wantCategory = []string{
	CategoryArtifact, // backup-2026-07-01.sql
	CategoryArtifact, // backup-2026-07-01.sql.gz
	CategoryArtifact, // backup-2026-07-01.sql.xz
	CategoryArtifact, // cluster.dump
	CategoryArtifact, // cluster.dump.gz
	CategoryArtifact, // postgresql.conf.bak
	CategoryArtifact, // site-assets.tar.gz
	CategoryArtifact, // site-assets.tgz
	CategoryArtifact, // release-1.6.0.zip
	// A leading dot is not special to fnmatch, so a dotfile that is nothing but
	// the extension matches -- unlike a shell, where * would not expand to it.
	CategoryArtifact, // .sql
	CategoryArtifact, // .tgz
	"",               // sql
	"",               // tar.gz
	"",               // dump.SQL
	CategoryArtifact, // DUMP.sql
	"",               // backup.sql.bz2
	"",               // backup.sql.gz.tmp
	CategoryLog,      // nginx-access.log.1
	CategoryLog,      // nginx-access.log.9
	CategoryLog,      // nginx-access.log.10
	CategoryLog,      // nginx-access.log.99
	// Three digits match neither the one-digit nor the two-digit glob, and the
	// third glob needs a .gz. A real gap in v1's list, ported rather than fixed:
	// a host that rotates past 99 has its oldest logs invisible to the scanner.
	"",               // nginx-access.log.100
	CategoryLog,      // nginx-access.log.gz
	CategoryLog,      // nginx-access.log.1.gz
	CategoryLog,      // nginx-access.log.14.gz
	"",               // nginx-access.log.1gz
	CategoryLog,      // nginx-access.log.old
	"",               // nginx-access.log.OLD
	"",               // nginx-access.log
	"",               // nginx-access.log.x
	"",               // nginx-access.log.١ (ARABIC-INDIC DIGIT ONE: not in [0-9])
	CategoryArtifact, // app.log.0.zip
	// A bracket in the *name* is just a character; only a pattern has classes.
	CategoryArtifact, // weird [bracket].sql
	CategoryArtifact, // a file with spaces.tar.gz
	// A star and a question mark in the name are likewise literal.
	CategoryArtifact, // *.sql
	CategoryArtifact, // ?.sql
	"",               // archive.tar.gz.sha256
	CategoryArtifact, // café-export.sql
}

func TestClassifyFixture(t *testing.T) {
	t.Parallel()

	lines := fixtureLines(t, "storage/stale_names.txt")
	if len(lines) != len(wantCategory) {
		t.Fatalf("fixture has %d lines, the table has %d", len(lines), len(wantCategory))
	}
	for i, line := range lines {
		got, ok := Classify(line, StaleArtifactGlobs(), LegacyLogGlobs())
		want := wantCategory[i]
		if want == "" {
			if ok {
				t.Errorf("line %d: Classify(%q) = %q, want no category", i+1, line, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("line %d: Classify(%q) = %q/%v, want %q", i+1, line, got, ok, want)
		}
	}
}

func TestClassifyHasNoCategoryForAnEmptyName(t *testing.T) {
	t.Parallel()

	// The case a line-per-case fixture cannot hold. os.walk never produces it, but
	// the argument is a plain string and every glob v1 ships starts with a star,
	// so "" is one edit away from matching everything.
	for _, name := range []string{"", " ", "\t"} {
		if got, ok := Classify(name, StaleArtifactGlobs(), LegacyLogGlobs()); ok {
			t.Errorf("Classify(%q) = %q, want no category", name, got)
		}
	}
}

func TestClassifyHasNoCategoryWithNoGlobs(t *testing.T) {
	t.Parallel()

	// An operator who empties both lists in config has asked for nothing to be
	// reported, not for everything: any() over an empty tuple is False.
	if got, ok := Classify("backup.sql", nil, nil); ok {
		t.Errorf("Classify with no globs = %q, want no category", got)
	}
}

func TestClassifyPrefersTheArtifactList(t *testing.T) {
	t.Parallel()

	// Unreachable with the shipped lists -- no name can end in both ".sql.gz" and
	// ".log.gz" -- so the tie-break is pinned with the overlapping lists an
	// operator's config could hold. Without this the ordering is an accident of
	// which if-statement came first.
	got, ok := Classify("app.log.gz", []string{"*.gz"}, LegacyLogGlobs())
	if !ok || got != CategoryArtifact {
		t.Errorf("Classify(%q) = %q/%v, want %q", "app.log.gz", got, ok, CategoryArtifact)
	}
	// And the same name is a log when only the log list claims it.
	got, ok = Classify("app.log.gz", nil, LegacyLogGlobs())
	if !ok || got != CategoryLog {
		t.Errorf("Classify(%q) = %q/%v, want %q", "app.log.gz", got, ok, CategoryLog)
	}
}

func TestClassifierAgreesWithTheOneShotDoor(t *testing.T) {
	t.Parallel()

	// The compiled door is what a directory walk will use, so it is checked
	// against the same fixture rather than taken on trust from one line of
	// delegation.
	c := NewClassifier(StaleArtifactGlobs(), LegacyLogGlobs())
	for i, line := range fixtureLines(t, "storage/stale_names.txt") {
		wantCat, wantOK := Classify(line, StaleArtifactGlobs(), LegacyLogGlobs())
		gotCat, gotOK := c.Classify(line)
		if gotCat != wantCat || gotOK != wantOK {
			t.Errorf("line %d: Classifier.Classify(%q) = %q/%v, want %q/%v",
				i+1, line, gotCat, gotOK, wantCat, wantOK)
		}
	}
}

func TestDefaultGlobListsCannotBeEditedByACaller(t *testing.T) {
	t.Parallel()

	// v1's are tuples. If these ever become exported slices, one caller's
	// append-in-place changes what every later scan classifies.
	got := StaleArtifactGlobs()
	got[0] = "*"
	if again := StaleArtifactGlobs()[0]; again != "*.sql" {
		t.Errorf("StaleArtifactGlobs()[0] = %q after a caller wrote to it, want %q", again, "*.sql")
	}

	logs := LegacyLogGlobs()
	logs[0] = "*"
	if again := LegacyLogGlobs()[0]; again != "*.log.[0-9]" {
		t.Errorf("LegacyLogGlobs()[0] = %q after a caller wrote to it, want %q", again, "*.log.[0-9]")
	}
}

func FuzzClassify(f *testing.F) {
	for _, line := range fixtureLines(f, "storage/stale_names.txt") {
		f.Add(line)
	}
	f.Fuzz(func(t *testing.T, name string) {
		got, ok := Classify(name, StaleArtifactGlobs(), LegacyLogGlobs())
		if !ok {
			if got != "" {
				t.Fatalf("Classify(%q) reported no category but returned %q", name, got)
			}
			return
		}
		if got != CategoryArtifact && got != CategoryLog {
			t.Fatalf("Classify(%q) = %q, which is neither category", name, got)
		}
		// Every glob v1 ships is "*." plus a suffix, so anything classified must
		// contain a dot and be longer than the shortest suffix. A category handed
		// back for a name with no dot in it would mean a glob list edit went wrong
		// -- and the walker deletes what this function categorises.
		if !strings.Contains(name, ".") {
			t.Fatalf("Classify(%q) = %q for a name with no extension", name, got)
		}
	})
}

// fixtureLines splits a behaviour-table fixture the way the oracle adapter does:
// Python's splitlines, empty lines skipped.
func fixtureLines(tb testing.TB, rel string) []string {
	tb.Helper()
	out := []string{}
	for _, line := range pytext.SplitLines(fixture.Text(tb, rel)) {
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}
