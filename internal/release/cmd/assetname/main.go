// Command assetname prints the published asset name for a GOARCH.
//
// It exists so the release workflow can assert its matrix against Go rather than
// repeating the names in YAML:
//
//	test "$(go run ./internal/release/cmd/assetname amd64)" = "fleetfix-linux-x86_64"
//
// Two places naming the same artifact is how the amd64 name — the one every
// fleetfix 1.6.0 install in the field looks for — drifts without anyone noticing.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/KingPin/FleetFix/v2/internal/release"
)

func main() {
	checksum := flag.Bool("checksum", false, "print the .sha256 sidecar name instead")
	all := flag.Bool("all", false, "print every published architecture as GOARCH<tab>name")
	flag.Usage = func() {
		// Ignored deliberately, and only here: a failed write of usage text leaves
		// nothing to recover, since reporting it would need the stream that just
		// refused a write. The writes carrying this tool's actual output, below,
		// stay checked: a CI step compares them against a literal asset name, and
		// an empty answer must fail rather than pass.
		_, _ = fmt.Fprint(flag.CommandLine.Output(),
			"usage: assetname [-checksum] <goarch>\n       assetname -all\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	name := release.AssetName
	if *checksum {
		name = release.ChecksumName
	}

	if *all {
		for _, goarch := range release.Architectures() {
			n, err := name(goarch)
			if err != nil {
				fail(err)
			}
			fmt.Printf("%s\t%s\n", goarch, n)
		}
		return
	}

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	n, err := name(flag.Arg(0))
	if err != nil {
		fail(err)
	}
	fmt.Println(n)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
