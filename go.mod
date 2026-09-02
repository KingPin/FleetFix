module github.com/KingPin/FleetFix/v2

// Ahead of 1.26.0 for standard-library advisories govulncheck finds reachable.
// Earlier floors were 1.26.1 for GO-2026-4602 (os.Root lets a FileInfo escape the
// root, reached from system.ReadZones through fs.Glob) and 1.26.3 for GO-2026-4971
// (Dial and LookupPort panic on a NUL byte, reached from netprobe -- Windows-only,
// and we ship Linux, but govulncheck grades by reachable symbol rather than by
// GOOS and the gate is the exit code).
//
// 1.26.6 is the first release clearing all seven the updater's HTTP client and the
// audit tailer pulled in:
//
//   - GO-2026-6218 net/url, GO-2026-6090 and GO-2026-5856 crypto/tls,
//     GO-2026-5972 encoding/asn1, GO-2026-5026 net/http -- all reached from
//     updater.fetchHTTP calling http.Client.Do against the releases API.
//   - GO-2026-5039 net/textproto -- reached from audit.Tailer.Next via io.ReadAll.
//   - GO-2026-5037 crypto/x509 -- reached from the same TLS handshake.
//
// The floor is 1.26.8, the current patch, rather than the 1.26.6 that clears them:
// a floor set to the exact version an advisory named goes red again on the next
// one, and there is nothing to weigh against taking the newest patch of a release
// series we already build against.
//
// CI resolves its toolchain from this line with GOTOOLCHAIN=local, so the floor
// here is the version govulncheck sees; leaving it behind is what makes the job
// red while a newer local toolchain stays green.
go 1.26.8

require github.com/google/go-cmp v0.7.0

require (
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2
	golang.org/x/sys v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require golang.org/x/text v0.14.0 // indirect
