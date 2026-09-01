package updater

import (
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// ParseSHA256Line pulls the digest for assetName out of a sha256sum-style file,
// reporting false when no line names that asset.
//
// The false is the whole point of the second return: an empty digest would go on
// to be compared against the hash of the downloaded file and fail closed, but it
// would fail as "the file is corrupt" rather than as "the release published no
// checksum for this asset", and those want different messages in front of an
// operator who is deciding whether to retry.
//
// Two details of the v1 loop are load-bearing rather than incidental, so they are
// reproduced rather than tidied:
//
//   - The name is compared after stripping every leading asterisk, not one. GNU
//     coreutils writes a single "*" to mark binary mode, but lstrip("*") removes a
//     run, so a file literally named "*fleetfix" is unreachable and a line reading
//     "<digest>  ***" matches an empty asset name. Both are v1 behaviour.
//   - The split is Python's str.split(None, 1), so any run of whitespace separates
//     the two fields and a name containing spaces still matches. A line with three
//     fields does not match, because the third stays attached to the second.
func ParseSHA256Line(text, assetName string) (string, bool) {
	for _, raw := range pytext.SplitLines(text) {
		line := strings.TrimFunc(raw, pytext.IsSpace)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := pytext.SplitN(line, 1)
		if len(parts) != 2 {
			continue
		}
		if strings.TrimLeft(parts[1], "*") == assetName {
			// pytext.Lower, not strings.ToLower: the digest is about to be
			// compared against one this process computed, so a rune the two
			// languages lowercase differently is a verification that fails on a
			// good download.
			return pytext.Lower(parts[0]), true
		}
	}
	return "", false
}
