package disk

import (
	"unicode/utf8"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// GhostFile is one deleted-but-still-open file, as reported by
// `lsof -F pcufsLn`.
//
// These are the files that make df and du disagree: the directory entry is
// gone, so du cannot see the bytes, but a process still holds the descriptor,
// so the kernel has not freed them. Killing or restarting the holder is what
// reclaims the space, which is why the pid and command travel with the size.
type GhostFile struct {
	PID       int64  `json:"pid"`
	Command   string `json:"command"`
	User      string `json:"user"`
	FD        string `json:"fd"`
	SizeBytes int64  `json:"size_bytes"`
	Path      string `json:"path"`
}

// ParseLsofFieldOutput parses lsof's field mode (-F).
//
// The format is one field per line, first character the tag and the rest the
// value: p (pid), c (command) and u (user) describe a process and stay in
// effect for every file under it; f (descriptor), s (size), L (link count) and
// n (name) describe one file. A p or an f line ends whatever file was being
// accumulated, and so does the end of the input.
//
// A file is a ghost when its link count is exactly zero -- no name left in any
// directory. An L line that is missing or unparseable counts as one link, i.e.
// not a ghost, which is the conservative direction: a file wrongly called a
// ghost is one an operator might kill a process to reclaim.
//
// Three details are v1's rather than lsof's, and are reproduced because the
// port must not be what changes an operator's numbers:
//
//   - The tag is one *rune*, not one byte, so a non-ASCII tag consumes the
//     whole character and the remainder is its value. Such a tag is stored and
//     never read, exactly as an unrecognised ASCII tag is.
//   - Fields seen before the first p line are kept, and the file they describe
//     is reported with pid 0 and an empty command and user.
//   - c and u are cleared by a p line but *not* by an f line, so every file
//     under one process inherits that process's command and user, and a group
//     that never names them reports empty strings rather than the previous
//     group's.
//
// One departure: v1's ints are arbitrary precision and these are int64. A pid
// or a size past 2^63-1 falls back to the same value the unparseable case uses
// -- 0 -- rather than being truncated to a smaller number that would read as a
// real reading. A link count past int64 falls back to 1 and so is still not a
// ghost, which is what Python concludes as well, so only pid and size can
// differ and only for values no kernel produces.
func ParseLsofFieldOutput(text string) []GhostFile {
	files := []GhostFile{}

	var pid int64
	var command, user string
	cur := map[rune]string{}

	// flush ends the file being accumulated, appending it if it is a ghost. It
	// reads pid, command and user as they stand now, which is why the p branch
	// below flushes before it overwrites them.
	flush := func() {
		if len(cur) == 0 {
			return
		}
		links := int64(1)
		if raw, ok := cur['L']; ok {
			if n, err := pytext.Int(raw); err == nil {
				links = n
			}
		}
		if links == 0 {
			var size int64
			if raw, ok := cur['s']; ok {
				if n, err := pytext.Int(raw); err == nil {
					size = n
				}
			}
			files = append(files, GhostFile{
				PID:       pid,
				Command:   command,
				User:      user,
				FD:        cur['f'],
				SizeBytes: size,
				Path:      cur['n'],
			})
		}
		cur = map[rune]string{}
	}

	for _, line := range pytext.SplitLines(text) {
		if line == "" {
			continue
		}
		tag, size := utf8.DecodeRuneInString(line)
		value := line[size:]
		switch tag {
		case 'p':
			flush()
			pid = 0
			if n, err := pytext.Int(value); err == nil {
				pid = n
			}
			command, user = "", ""
		case 'c':
			command = value
		case 'u':
			user = value
		case 'f':
			flush()
			cur['f'] = value
		default:
			cur[tag] = value
		}
	}
	flush()

	return files
}

// TotalBytes is what the ghost files are holding open, and the number the
// operator is deciding against: it is the space a restart would return.
//
// The sum is int64 where v1's is arbitrary precision, so a corpus of ghosts
// adding past 8 exabytes wraps rather than growing. Every term came from a
// single file's size, so reaching that needs either a fabricated size or more
// open deleted files than a host has descriptors.
func TotalBytes(files []GhostFile) int64 {
	var total int64
	for _, f := range files {
		total += f.SizeBytes
	}
	return total
}
