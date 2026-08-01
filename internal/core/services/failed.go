// Package services holds the systemd-facing logic: which units have failed,
// who they run as, and what made the last boot slow.
package services

import (
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// FailedUnit is one row of `systemctl list-units --failed --no-legend --plain`.
//
// The four fixed columns are systemd's own: the unit name, whether its file
// loaded, the high-level active state, and the sub-state that says how it
// failed. Description is the free text that follows and is the only column that
// may contain spaces.
type FailedUnit struct {
	Name        string `json:"name"`
	Load        string `json:"load"`
	Active      string `json:"active"`
	Sub         string `json:"sub"`
	Description string `json:"description"`
}

// ParseFailedUnits parses systemctl's tabular unit listing.
//
// --no-legend is asked for, so the header and the trailing summary should not be
// there at all; the two skip prefixes are belt-and-braces for the version that
// prints them anyway. Both are reproduced exactly as v1 wrote them, including
// the parts that are arguably wrong:
//
//   - The header prefix is "UNIT " with a trailing space, so a header separated
//     by tabs rather than spaces is not recognised and is parsed as a unit named
//     UNIT. The same prefix means a real unit literally named "UNIT" followed by
//     a space is dropped -- systemd unit names may not contain spaces, so that
//     costs nothing.
//   - The summary prefix is two ASCII spaces, so a row indented by one space, a
//     tab, or a non-breaking space is parsed as data.
//
// A row needs at least four fields; three is a truncated line rather than a unit
// with no description, and is skipped. The split is on runs of whitespace with
// at most four cuts, so the description keeps whatever spacing it had inside it
// and loses only what trails the line.
func ParseFailedUnits(text string) []FailedUnit {
	out := []FailedUnit{}
	for _, line := range pytext.SplitLines(text) {
		line = strings.TrimRightFunc(line, pytext.IsSpace)
		if line == "" || strings.HasPrefix(line, "UNIT ") || strings.HasPrefix(line, "  ") {
			continue
		}
		parts := pytext.SplitN(line, 4)
		if len(parts) < 4 {
			continue
		}
		description := ""
		if len(parts) > 4 {
			description = parts[4]
		}
		out = append(out, FailedUnit{
			Name:        parts[0],
			Load:        parts[1],
			Active:      parts[2],
			Sub:         parts[3],
			Description: description,
		})
	}
	return out
}

// ParseShowUser reads the User= of each unit out of `systemctl show -p User`,
// one answer per unit in the order asked.
//
// systemd separates the property blocks with a blank line, and an empty or
// absent User= means the unit runs as root -- which is the answer that matters,
// because it is the difference between a failed unit being one operator's
// problem and being the host's.
//
// The block split is on the literal two newlines v1 split on, not on "a blank
// line", and that difference is visible: CRLF output separates its blocks with
// "\r\n\r\n", which contains no "\n\n", so the whole output is one block and
// only the first unit's User= is reported. Reproduced rather than fixed, because
// systemctl on a Linux host does not emit CRLF and a divergence here would be
// the port changing an answer.
//
// The first User= line in a block wins; a later one is not read. Only a line
// starting exactly with "User=" counts, so an indented or lower-cased key reads
// as absent, i.e. as root.
func ParseShowUser(text string) []string {
	users := []string{}
	for _, block := range strings.Split(text, "\n\n") {
		if strings.TrimFunc(block, pytext.IsSpace) == "" {
			continue
		}
		user := "root"
		for _, line := range pytext.SplitLines(block) {
			if value, ok := strings.CutPrefix(line, "User="); ok {
				if v := strings.TrimFunc(value, pytext.IsSpace); v != "" {
					user = v
				}
				break
			}
		}
		users = append(users, user)
	}
	return users
}
