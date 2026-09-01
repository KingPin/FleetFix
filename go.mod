module github.com/KingPin/FleetFix/v2

// Ahead of 1.26.0 for two standard-library advisories:
//
//   - GO-2026-4602 -- os.Root lets a FileInfo escape the root, reachable from
//     system.ReadZones through fs.Glob. Fixed in 1.26.1.
//   - GO-2026-4971 -- Dial and LookupPort panic on a NUL byte, reachable from
//     netprobe's Prober.PrimaryIPv4 and Prober.Port. Fixed in 1.26.3. The panic
//     itself is Windows-only and we ship Linux, so nothing here is exploitable;
//     govulncheck grades by reachable symbol rather than by GOOS, and the gate
//     is the exit code.
//
// CI resolves its toolchain from this line with GOTOOLCHAIN=local, so the floor
// here is the version govulncheck sees; leaving it behind is what makes the job
// red while a newer local toolchain stays green.
go 1.26.3

require github.com/google/go-cmp v0.7.0

require (
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2
	golang.org/x/sys v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require golang.org/x/text v0.14.0 // indirect
