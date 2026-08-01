module github.com/KingPin/FleetFix/v2

// 1.26.1, not 1.26.0, for GO-2026-4602 -- os.Root lets a FileInfo escape the
// root, reachable from system.ReadZones through fs.Glob. CI resolves its
// toolchain from this line with GOTOOLCHAIN=local, so the floor here is the
// version govulncheck sees; leaving it at .0 is what made the job red while a
// newer local toolchain stayed green.
go 1.26.1

require github.com/google/go-cmp v0.7.0

require gopkg.in/yaml.v3 v3.0.1
