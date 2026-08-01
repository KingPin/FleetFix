package network

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// The users field ss appends with -p: users:(("sshd",pid=812,fd=3)). Only the
// first entry is read, so a socket several processes share reports the first one
// ss listed.
var usersRe = regexp.MustCompile(`\("([^"]+)",pid=(` + digits + `),fd=` + digits + `\)`)

// ListeningSocket is one LISTEN row from `ss -tulpn`.
//
// ProcessName and PID are set together or not at all: they come from one users
// entry, and a row without -p (or run without the privilege to see the owner)
// has neither.
type ListeningSocket struct {
	LocalAddress string  `json:"local_address"`
	LocalPort    int64   `json:"local_port"`
	ProcessName  *string `json:"process_name"`
	PID          *int64  `json:"pid"`
}

// ParseSSOutput reads `ss` output and keeps the listening sockets.
//
// Rows that are not LISTEN, and rows whose local address has no port that
// converts, are dropped rather than reported as partial readings -- what the
// callers want is the set of ports something is actually listening on.
//
// One departure from v1: a port or pid past int64 drops the socket, where
// Python reports the arbitrary-precision integer. A port is bounded by 16 bits
// and a pid by pid_max, so this is reachable only by output ss does not produce.
func ParseSSOutput(output string) []ListeningSocket {
	sockets := []ListeningSocket{}
	for _, raw := range pytext.SplitLines(output) {
		line := strings.TrimFunc(raw, pytext.IsSpace)
		// The two header spellings ss uses, depending on whether -f was given.
		if line == "" || strings.HasPrefix(line, "State") || strings.HasPrefix(line, "Netid") {
			continue
		}
		parts := pytext.Fields(line)
		if len(parts) < 4 || parts[0] != "LISTEN" {
			continue
		}
		addr, port, ok := splitAddrPort(parts[3])
		if !ok {
			continue
		}
		name, pid := extractUsers(line)
		sockets = append(sockets, ListeningSocket{
			LocalAddress: addr,
			LocalPort:    port,
			ProcessName:  name,
			PID:          pid,
		})
	}
	return sockets
}

// splitAddrPort splits ss's "address:port" column. The third return is false
// when there is no port to be had, which is how "0.0.0.0:*" and a bare "*:*"
// keep themselves out of the results.
//
// The port is not range-checked: v1 reports whatever int() reads, so a negative
// port in hand-written input is carried through rather than quietly repaired.
func splitAddrPort(local string) (string, int64, bool) {
	var addr, portPart string
	if strings.HasPrefix(local, "[") {
		end := strings.Index(local, "]")
		if end == -1 {
			return local, 0, false
		}
		addr = local[1:end]
		// v1 slices past the ']' and one further character without looking at
		// what that character is, so "[::1]x22" reads as port 22 exactly like
		// "[::1]:22" does. One *character*, not one byte -- "[::1]<U+20AC>22" is
		// also port 22.
		portPart = skipOneRune(local[end+1:])
	} else {
		// rpartition(":"): the last colon splits, and no colon -- or a leading
		// one, which leaves the address empty -- means there is no port here.
		i := strings.LastIndex(local, ":")
		if i <= 0 {
			return local, 0, false
		}
		addr, portPart = local[:i], local[i+1:]
	}
	port, err := pytext.Int(portPart)
	if err != nil {
		return addr, 0, false
	}
	return addr, port, true
}

func skipOneRune(s string) string {
	if s == "" {
		return ""
	}
	_, size := utf8.DecodeRuneInString(s)
	return s[size:]
}

// extractUsers reads the first process out of ss's users field. Both returns are
// nil when the row has no users field, or when its pid will not fit an int64 --
// half a users entry would claim to know a process while having lost the number
// that identifies it.
func extractUsers(line string) (*string, *int64) {
	m := usersRe.FindStringSubmatch(line)
	if m == nil {
		return nil, nil
	}
	pid, err := pytext.Int(m[2])
	if err != nil {
		return nil, nil
	}
	return &m[1], &pid
}
