// Command fleetfix is the FleetFix binary.
//
// This file exists to do exactly three things -- name the process's real streams,
// call into internal/cli, and turn the returned code into an exit status -- and
// nothing else, ever. os.Exit does not run deferred functions, so any logic that
// lives here is logic that cannot be tested without spawning a subprocess and
// cannot clean up after itself. Everything testable belongs one package down.
package main

import (
	"os"

	"github.com/KingPin/FleetFix/v2/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
