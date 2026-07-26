package docker

import (
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
	"github.com/KingPin/FleetFix/v2/internal/pytime"
)

// InspectFields is what `docker inspect` gives back through the pipe-delimited
// template v1 asks for: `<RestartCount>|<LogPath>|<StartedAt>|<State.Status>`.
//
// StartedAt is a *string because v1 distinguishes two answers a plain string
// cannot. A well-formed line whose third field is empty reports "", meaning docker
// named a container with no start time; a line too short to read reports None,
// meaning the template did not come back at all. The caller then feeds it to an
// ISO-8601 parse that treats both as "never started" -- but it is the difference
// between an empty answer and no answer, and the check envelope will want to say
// which.
//
// A struct rather than a map, so the four keys are always present in v1's order.
type InspectFields struct {
	RestartCount int64   `json:"restart_count"`
	LogPath      string  `json:"log_path"`
	StartedAt    *string `json:"started_at"`
	Status       string  `json:"status"`
}

// ParseInspectFields splits the inspect template into its four fields.
//
// Fewer than four fields is the whole line rejected rather than a partial read: a
// truncated template means the docker call went wrong, and reporting the first two
// fields of a wrong answer is worse than reporting none of it. More than four is
// the extras discarded, which is what a LogPath containing a "|" comes to.
//
// Only the whole string is stripped, never the individual fields -- measured. So
// "5|  b  |  c  |  d  " keeps the interior padding and loses only the trailing run,
// and a LogPath is reported with whatever spaces docker put in the path.
func ParseInspectFields(text string) InspectFields {
	parts := strings.Split(strings.TrimFunc(text, pytext.IsSpace), "|")
	if len(parts) < 4 {
		return InspectFields{}
	}
	// int() on the count, so " 7 ", "+7" and "1_0" all read as numbers and "7.0"
	// and "0x10" do not. A count above 2^63-1 is pytext.Int's documented magnitude
	// departure and lands on this same zero, where v1 keeps the value; docker's own
	// RestartCount is a Go int, so nothing it writes can reach it.
	count, err := pytext.Int(parts[0])
	if err != nil {
		count = 0
	}
	startedAt := parts[2]
	return InspectFields{
		RestartCount: count,
		LogPath:      parts[1],
		StartedAt:    &startedAt,
		Status:       parts[3],
	}
}

// zeroTimePrefix is the date half of Go's zero time.Time, which is what docker
// writes into .State.StartedAt for a container that has never run. v1 treats the
// whole prefix as "never started" rather than parsing it and comparing, so
// "0001-01-01T99:99:99Z" is also never-started while "0001-01-02T00:00:00Z" is a
// real timestamp in the year 1.
const zeroTimePrefix = "0001-01-01"

// ParseISO reads a container's start time, reporting ok=false where v1 reports
// None: an empty string, docker's zero value, or anything datetime.fromisoformat
// refuses.
//
// A string rather than the *string InspectFields carries, because v1 collapses the
// distinction here -- _parse_iso(None) and _parse_iso("") are both None -- and the
// pointer is only worth keeping where it still means something.
//
// The Z substitution is v1's, quirk included: str.replace rewrites *every* "Z",
// not just a trailing one, so a "Z" used as the date/time separator turns
// "2026-07-26Z15:04:05" into "2026-07-26+00:0015:04:05" and takes a string
// fromisoformat would otherwise have accepted. Reproduced rather than corrected,
// because a start time that reads as "never started" changes what the restart-loop
// check says, and the port is not the place to decide that v1 was wrong.
func ParseISO(value string) (pytime.Time, bool) {
	if value == "" || strings.HasPrefix(value, zeroTimePrefix) {
		return pytime.Time{}, false
	}
	t, err := pytime.FromISOFormat(strings.ReplaceAll(value, "Z", "+00:00"))
	if err != nil {
		return pytime.Time{}, false
	}
	return t, true
}

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
