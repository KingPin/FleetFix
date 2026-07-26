// Command ffdiff is the Go side of the differential harness.
//
// It has two modes. `ffdiff oracle` runs every case in testdata/cases.jsonl
// through the ported Go implementation and writes one JSON-lines record per
// case, in the same format tools/oracle/py_oracle.py produces. `ffdiff compare`
// reads the two files and reports where they disagree.
//
//	python tools/oracle/py_oracle.py --out py.jsonl
//	go run ./tools/ffdiff oracle --out go.jsonl
//	go run ./tools/ffdiff compare --py py.jsonl --go go.jsonl --out report.json
//
// Three process launches, which is the whole point of the batch design: an
// interpreter start per case would put the harness past the budget that lets it
// run on every PR, and a harness that only runs nightly gets ignored and then
// muted.
//
// Comparison is on decoded values, not bytes. The two encoders disagree about
// things that carry no meaning -- Python escapes non-ASCII and Go escapes HTML
// -- and a harness that reported those as divergences would be noise from the
// first run.
//
// While the port is in progress, a case whose function has no Go adapter yet
// records UnknownFunction, exactly as py_oracle would for a name it does not
// know. compare counts those separately as "unimplemented" rather than as
// divergences, so the harness is useful from the first parser onward instead of
// only once all forty exist. `compare --require-complete` turns them back into
// failures; that is the flag the M2 exit gate runs with.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "ffdiff: %v\n", err)
		}
		os.Exit(1)
	}
}

func run(argv []string, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		usage(stderr)
		return errors.New("no mode given")
	}
	switch argv[0] {
	case "oracle":
		return runOracle(argv[1:], stdout, stderr)
	case "compare":
		return runCompare(argv[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown mode %q", argv[0])
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: ffdiff <mode> [flags]

modes:
  oracle    run every manifest case through the Go implementation
  compare   diff a Python oracle run against a Go oracle run

run `+"`ffdiff <mode> -h`"+` for the flags of a mode.
`)
}
