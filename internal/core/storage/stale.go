package storage

import "github.com/KingPin/FleetFix/v2/internal/pytext"

// The two categories a stale candidate can carry. v1 spells them as bare strings
// in a dataclass field annotated `str  # "artifact" | "log"`; they are named here
// because the check envelope reports the category and a typo in a string literal
// is not a compile error.
const (
	CategoryArtifact = "artifact"
	CategoryLog      = "log"
)

// staleArtifactGlobs are database dump and archive artifacts that pile up in user
// homes during debugging and rarely get cleaned up afterwards.
var staleArtifactGlobs = []string{
	"*.sql",
	"*.sql.gz",
	"*.sql.xz",
	"*.dump",
	"*.dump.gz",
	"*.bak",
	"*.tar.gz",
	"*.tgz",
	"*.zip",
}

// legacyLogGlobs are logrotate and journald leftovers a user can write under
// their own home. Production system logs live under /var/log and are not scanned
// by Tier 1.
var legacyLogGlobs = []string{
	"*.log.[0-9]",
	"*.log.[0-9][0-9]",
	"*.log.gz",
	"*.log.[0-9]*.gz",
	"*.log.old",
}

// StaleArtifactGlobs and LegacyLogGlobs return the default glob lists.
//
// Functions returning a fresh slice rather than exported slice variables: v1's
// are tuples, so a caller cannot edit the defaults for every other caller in the
// process, and an exported []string would quietly give that up. The cost is one
// allocation per scan, against a per-file cost of walking a directory tree.
func StaleArtifactGlobs() []string { return append([]string(nil), staleArtifactGlobs...) }

// LegacyLogGlobs returns the default rotated-log globs.
func LegacyLogGlobs() []string { return append([]string(nil), legacyLogGlobs...) }

// Classifier decides which category a filename falls into, with its globs already
// compiled.
//
// The compilation is the reason this type exists. v1 leans on fnmatch's internal
// lru_cache to translate each glob once for a whole walk; pytext makes that the
// caller's business instead, so a walk that built its patterns per filename would
// pay fourteen regex compiles per file.
type Classifier struct {
	artifact []pytext.FNPattern
	log      []pytext.FNPattern
}

// NewClassifier compiles the two glob lists.
//
// Neither list is validated. fnmatch has no invalid patterns -- an unterminated
// "[" is a literal bracket, and a reversed range is a class that matches nothing
// -- so there is nothing here to reject, and a glob from an operator's config
// that matches nothing is their answer rather than an error.
func NewClassifier(artifactGlobs, logGlobs []string) Classifier {
	return Classifier{
		artifact: compileGlobs(artifactGlobs),
		log:      compileGlobs(logGlobs),
	}
}

func compileGlobs(globs []string) []pytext.FNPattern {
	out := make([]pytext.FNPattern, 0, len(globs))
	for _, glob := range globs {
		out = append(out, pytext.CompileFNPattern(glob))
	}
	return out
}

// Classify reports which category name falls into, with ok=false where v1 returns
// None.
//
// Artifact wins a tie, because v1 tests that list first. Today's two lists cannot
// both match one name, but they are a config surface: an operator who adds
// "*.gz" to the artifacts is entitled to have every .gz counted as an artifact
// rather than have the answer depend on list order they cannot see.
//
// Case matters -- fnmatchcase, not fnmatch -- so "DUMP.SQL" is not a match. That
// is v1's behaviour and correct on a case-sensitive filesystem, where the two are
// genuinely different files.
func (c Classifier) Classify(name string) (string, bool) {
	if matchesAny(c.artifact, name) {
		return CategoryArtifact, true
	}
	if matchesAny(c.log, name) {
		return CategoryLog, true
	}
	return "", false
}

func matchesAny(patterns []pytext.FNPattern, name string) bool {
	for _, p := range patterns {
		if p.MatchCase(name) {
			return true
		}
	}
	return false
}

// Classify is v1's _classify: one filename against two glob lists.
//
// It compiles both lists on every call, which is what v1's signature invites. A
// caller with more than one filename wants NewClassifier.
func Classify(name string, artifactGlobs, logGlobs []string) (string, bool) {
	return NewClassifier(artifactGlobs, logGlobs).Classify(name)
}
