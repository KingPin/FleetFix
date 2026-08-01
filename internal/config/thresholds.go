package config

import "github.com/KingPin/FleetFix/v2/internal/threshold"

// Thresholds resolves the host's grading policy from thresholds.yml.
//
// This is the one resolver: the TUI's severity colours, trips[] in `check --json`,
// the agent's threshold-trip events and `fleetfix doctor`'s listing all come
// through here, so there is no way for doctor to report a policy the checks do not
// grade by. That is an M3 exit criterion rather than a nicety -- two resolvers is
// how a support call ends with "but doctor says 85".
//
// The returned Loaded carries both halves of the audit trail an operator needs:
// Sources says which files were consulted and which were read, and Warnings holds
// everything that went wrong at any layer -- a file that would not parse, a rule
// that does not exist, a bound that is not a number -- in one list, in a stable
// order, ready for config_warnings[].
//
// Never fails. Every degradation is a warning and the shipped policy stands, so a
// typo costs an operator a wrong bound rather than a host that stops grading.
func (p Paths) Thresholds() (threshold.Set, Loaded) {
	loaded := p.Load(ThresholdsFile)
	overrides, parseWarnings := threshold.ParseOverrides(loaded.Values)
	set, mergeWarnings := threshold.Merge(overrides)
	// Parse warnings before merge warnings: they are the earlier reading of the
	// same file, and an operator scanning the list wants "this line is not a
	// number" before "this rule could not be applied".
	loaded.Warnings = append(loaded.Warnings, parseWarnings...)
	loaded.Warnings = append(loaded.Warnings, mergeWarnings...)
	return set, loaded
}
