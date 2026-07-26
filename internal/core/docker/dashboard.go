package docker

import (
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// ParsePSJSONLines reads `docker ps --format json`, one value per line, skipping
// any line that is not JSON.
//
// The return type is []any rather than a slice of objects because that is what
// this function really produces: v1 annotates it list[dict] but appends whatever
// json.loads handed back, so a line holding 123 or null or [1,2] ends up in the
// list and crashes the next function along instead of this one. Reproducing that
// costs nothing here and matters for the fuzzing pass, where a corpus replayed
// through both implementations will produce a bare scalar line long before it
// produces a plausible container.
func ParsePSJSONLines(text string) []any {
	rows := []any{}
	for _, line := range pytext.SplitLines(text) {
		line = strings.TrimFunc(line, pytext.IsSpace)
		if line == "" {
			continue
		}
		if v, ok := decodeJSONLine(line); ok {
			rows = append(rows, v)
		}
	}
	return rows
}
