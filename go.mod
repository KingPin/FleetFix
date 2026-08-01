module github.com/KingPin/FleetFix/v2

// 1.26.1, not 1.26.0, for GO-2026-4602 -- os.Root lets a FileInfo escape the
// root, reachable from system.ReadZones through fs.Glob. CI resolves its
// toolchain from this line with GOTOOLCHAIN=local, so the floor here is the
// version govulncheck sees; leaving it at .0 is what made the job red while a
// newer local toolchain stayed green.
go 1.26.1

require github.com/google/go-cmp v0.7.0

require (
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2
	golang.org/x/sys v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require golang.org/x/text v0.14.0 // indirect
