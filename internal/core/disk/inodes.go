package disk

import (
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// InodeUsage is one row of `df -P -i`.
//
// A host can have 90% of its space free and still fail to create a file because
// the filesystem ran out of inodes, so this is a separate signal from Usage
// rather than more columns on it -- which is also how the v1 dataclasses split
// it, and the field names are theirs.
type InodeUsage struct {
	Filesystem string `json:"filesystem"`
	Mount      string `json:"mount"`
	Total      int64  `json:"total"`
	Used       int64  `json:"used"`
	Free       int64  `json:"free"`
	UsedPct    int64  `json:"used_pct"`
}

// ParseDFInodes parses `df -P -i` output.
//
// Same POSIX six-column layout as ParseDF and the same reasons for the split cap
// and the pseudo-filesystem skip; only the columns mean something different.
// Rows reporting zero inodes are dropped because btrfs and zfs allocate them
// dynamically and report 0, which would otherwise read as a full filesystem.
func ParseDFInodes(text string) []InodeUsage {
	out := []InodeUsage{}
	for _, line := range pytext.SplitLines(text) {
		if line == "" || strings.HasPrefix(line, headerPrefix) {
			continue
		}
		if hasAnyPrefix(line, skipLinePrefixes) {
			continue
		}
		parts := pytext.SplitN(line, 5)
		if len(parts) < 6 {
			continue
		}
		total, errTotal := pytext.Int(parts[1])
		used, errUsed := pytext.Int(parts[2])
		free, errFree := pytext.Int(parts[3])
		if errTotal != nil || errUsed != nil || errFree != nil {
			// df prints '-' for a filesystem with no inode accounting; some
			// overlayfs mounts do this.
			continue
		}
		if total == 0 {
			continue
		}
		out = append(out, InodeUsage{
			Filesystem: parts[0],
			Mount:      parts[5],
			Total:      total,
			Used:       used,
			Free:       free,
			// Shared with ParseDF: the IUse% column is read the same way, and its
			// fallback is what the inodes_missing_iuse fixture exercises. The
			// divide-by-zero guard inside it is unreachable from here because a
			// zero-inode row was already dropped above -- as it is in Python, where
			// the same `if total else 0` sits after the same skip.
			UsedPct: usedPercent(parts[4], used, total),
		})
	}
	return out
}
