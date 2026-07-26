// Package release names the artifacts a release publishes.
//
// One source of truth, on purpose. A v1.6.0 host updates itself by looking for an
// asset called exactly "fleetfix-linux-x86_64" in the latest GitHub release, and
// nothing in that host will ever change again — the string is frozen forever, and
// the entire auto-update continuity story for the Python-to-Go hop rests on it.
// The checker, the installer, and a CI assert against the release workflow's
// matrix all call in here rather than each spelling it out.
//
// Names follow the uname -m convention (x86_64, aarch64) rather than Go's
// (amd64, arm64). That keeps x86_64 continuity free and lets install.sh map
// `uname -m` straight through.
package release

import "fmt"

// AssetNamePrefix is common to every published binary.
const AssetNamePrefix = "fleetfix-linux-"

// ChecksumSuffix is appended to an asset name to get its sidecar. install.sh runs
// `sha256sum -c` on this file from inside the download directory, so the sidecar
// has to contain a bare filename — a "dist/" prefix in it breaks the check.
const ChecksumSuffix = ".sha256"

// machine maps GOARCH onto the uname -m name used in asset names.
var machine = map[string]string{
	"amd64": "x86_64",
	"arm64": "aarch64",
}

// AssetName returns the published asset name for a GOARCH, or an error for an
// architecture this project does not build.
//
// Erroring rather than falling back is deliberate: a silent default would publish
// a plausible-looking asset under a name no installer looks for, and the failure
// would surface as "no update available" on every host instead of a failed build.
func AssetName(goarch string) (string, error) {
	m, ok := machine[goarch]
	if !ok {
		return "", fmt.Errorf("release: no asset name defined for GOARCH %q", goarch)
	}
	return AssetNamePrefix + m, nil
}

// ChecksumName returns the sidecar name for a GOARCH's asset.
func ChecksumName(goarch string) (string, error) {
	asset, err := AssetName(goarch)
	if err != nil {
		return "", err
	}
	return asset + ChecksumSuffix, nil
}

// Architectures lists the GOARCH values this project publishes, in release order.
func Architectures() []string {
	return []string{"amd64", "arm64"}
}
