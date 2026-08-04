package threshold

import (
	"fmt"
	"math/big"
	"sort"
)

// An Override is what an operator wrote for one rule in thresholds.yml.
//
// Both bounds are optional, and a nil one means "leave the shipped value alone".
// That is the common case rather than a nicety: an operator tightening the disk
// warning to 75 has said nothing about when a full disk becomes critical, and
// reading their silence as zero would page them on every host they own.
//
// Pointers rather than a NaN sentinel because the file can legitimately contain
// NaN -- PyYAML parses `.nan` -- and that has to reach Validate as the mistake it
// is rather than be swallowed as "unset".
type Override struct {
	Warn *float64
	Crit *float64
}

// ParseOverrides decodes a thresholds.yml body into overrides plus a warning for
// every line that could not be used.
//
// The file is a flat mapping of rule id to bounds, which is the shape an operator
// can write from a `fleetfix doctor` listing without consulting documentation:
//
//	disk.used_pct:
//	  warn: 75
//	  crit: 90
//	mem.used_pct:
//	  warn: 90
//
// Values, not decoding, are this function's subject: it takes the plain Go values
// internal/config produces from PyYAML's vocabulary and never sees YAML. That is
// what keeps this package a leaf -- the grader must not depend on the decoder, or
// the TUI's severity colours would pull a YAML parser in behind them.
//
// Nothing here fails. Every rejection is a warning naming the key the operator
// wrote, because the failure mode this guards against is a threshold that was
// silently not applied and a host that then did not page.
func ParseOverrides(values map[string]any) (map[string]Override, []string) {
	out := make(map[string]Override, len(values))
	var warnings []string

	// Sorted, so two runs over the same file produce byte-identical warnings; the
	// report is asserted byte-stable across consecutive runs.
	for _, id := range sortedKeys(values) {
		body, ok := values[id].(map[string]any)
		if !ok {
			warnings = append(warnings, fmt.Sprintf(
				"thresholds.yml: %s must be a mapping of warn and crit, ignoring it", id,
			))
			continue
		}
		var ov Override
		for _, key := range sortedKeys(body) {
			switch key {
			case "warn", "crit":
				n, err := toFloat(body[key])
				if err != nil {
					warnings = append(warnings, fmt.Sprintf(
						"thresholds.yml: %s.%s %v, ignoring it", id, key, err,
					))
					continue
				}
				if key == "warn" {
					ov.Warn = &n
				} else {
					ov.Crit = &n
				}
			default:
				// A misspelled bound parses, stores, and is read by nothing, so
				// without this the operator's setting vanishes without a trace.
				warnings = append(warnings, fmt.Sprintf(
					"thresholds.yml: %s has no setting named %q, ignoring it", id, key,
				))
			}
		}
		if ov.Warn == nil && ov.Crit == nil {
			continue
		}
		out[id] = ov
	}
	return out, warnings
}

// toFloat accepts the numeric half of the value vocabulary internal/config
// documents, and rejects the rest by name.
//
// A quoted "90" is refused rather than coerced: YAML's quoting is the operator
// saying they meant a string, and accepting it would make the one typo that
// changes a document's meaning invisible.
func toFloat(v any) (float64, error) {
	switch n := v.(type) {
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	case *big.Int:
		// Out of range for any real bound, but converting is what lets Validate
		// give the reason rather than this function inventing one.
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, nil
	case nil:
		return 0, fmt.Errorf("is empty, and a bound has to be a number")
	default:
		return 0, fmt.Errorf("is %T, and a bound has to be a number", v)
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Describe renders a set for `fleetfix doctor`, marking the rules an operator has
// changed so the listing answers "is my file in effect?" and not merely "what are
// the numbers?".
// Two passes, because the bounds column's width is content and not a guess: %g
// renders 1 and 85 at different widths, so padding the unit alone left the
// descriptions of the ratio rules two characters left of everybody else's. A
// table that loses its alignment on some rows is harder to read than one that
// never had any.
func (s Set) Describe() []string {
	base := Defaults()
	ids := s.IDs()

	bounds := make([]string, len(ids))
	width := 0
	for i, id := range ids {
		r := s[id]
		bounds[i] = fmt.Sprintf("warn %g crit %g %s", r.Warn, r.Crit, r.Unit)
		width = max(width, len(bounds[i]))
	}

	lines := make([]string, 0, len(ids))
	for i, id := range ids {
		r := s[id]
		suffix := ""
		if b, ok := base[id]; !ok || b.Warn != r.Warn || b.Crit != r.Crit {
			suffix = " (overridden)"
		}
		lines = append(lines, fmt.Sprintf(
			"%-18s %-*s  %s%s",
			r.ID, width, bounds[i], r.Description, suffix,
		))
	}
	return lines
}
