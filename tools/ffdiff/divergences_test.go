package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// No file means no accepted divergences. This is the default the harness ships
// with, so it gets a test rather than being left to the reader of the loader.
func TestNoKnownDivergencesFile(t *testing.T) {
	accepted, err := loadKnownDivergences("")
	if err != nil {
		t.Fatalf("loadKnownDivergences(\"\"): %v", err)
	}
	if len(accepted) != 0 {
		t.Errorf("an absent file accepted %d divergence(s)", len(accepted))
	}
}

func TestLoadKnownDivergences(t *testing.T) {
	body := `
divergences:
  - id: df.usage_mixed
    reason: Python's int() accepts Arabic-Indic digits; Go's parser is ASCII-only.
    issue: https://github.com/KingPin/FleetFix/issues/7
  - id: ss.listening
    reason: column widths differ between iproute2 versions.
    issue: "#8"
`
	accepted, err := loadKnownDivergences(writeTemp(t, "known.yaml", body))
	if err != nil {
		t.Fatalf("loadKnownDivergences: %v", err)
	}
	if len(accepted) != 2 || !accepted["df.usage_mixed"] || !accepted["ss.listening"] {
		t.Errorf("accepted = %v, want both ids", accepted)
	}
}

// Each required field gets its own case. The failure mode of a list like this is
// that it becomes where divergences go to be forgotten, so an entry that cannot
// be evaluated by a reader must not load at all.
func TestLoadKnownDivergencesRejectsIncompleteEntries(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{
			"no id",
			"divergences:\n  - reason: r\n    issue: i\n",
			"entry 1 has no id",
		},
		{
			"no reason",
			"divergences:\n  - id: a\n    issue: i\n",
			"a has no reason",
		},
		{
			"no issue",
			"divergences:\n  - id: a\n    reason: r\n",
			"a has no issue link",
		},
		{
			"listed twice",
			"divergences:\n  - id: a\n    reason: r\n    issue: i\n  - id: a\n    reason: r2\n    issue: i2\n",
			"a is listed twice",
		},
		{
			"not yaml",
			"divergences: [\n",
			"known.yaml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadKnownDivergences(writeTemp(t, "known.yaml", tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("loadKnownDivergences = %v, want an error mentioning %q", err, tt.want)
			}
		})
	}

	if _, err := loadKnownDivergences(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("a --known path that does not exist was accepted; a typo would silently accept nothing")
	}
}
