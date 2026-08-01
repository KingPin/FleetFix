// Package cli parses arguments and dispatches to a subcommand.
//
// Stdlib flag, not cobra. v2 ships three subcommands and roughly fifteen flags;
// cobra's generated help and completion would be the only thing earned, against a
// dependency in the path of every invocation including the unattended ones.
//
// Everything here writes through injected streams rather than touching os.Stdout or
// os.Stderr. cmd/fleetfix is the only place in the tree that names the process's
// real streams, which is what lets an end-to-end test of `check --json` assert on
// the exact bytes a cron job would receive -- and a test in this package walks the
// AST to keep it that way.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/check/builtin"
	"github.com/KingPin/FleetFix/v2/internal/cli/checkcmd"
	"github.com/KingPin/FleetFix/v2/internal/exitcode"
	"github.com/KingPin/FleetFix/v2/internal/logging"
	"github.com/KingPin/FleetFix/v2/internal/resolve"
	"github.com/KingPin/FleetFix/v2/internal/version"
)

// Main runs FleetFix and returns the process exit code.
//
// Returning a code instead of calling os.Exit is what makes the whole surface
// testable: os.Exit skips deferred closes, so a version of this that exited
// internally could not be exercised without spawning a subprocess for every case.
func Main(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fleetfix", flag.ContinueOnError)
	// The default handler prints usage to the flagset's output on any parse error,
	// including -h. Silencing it lets help go to stdout (it was asked for) while
	// errors go to stderr (they were not).
	fs.SetOutput(stderr)
	fs.Usage = func() {}

	var (
		showVersion = fs.Bool("version", false, "Print the version and exit.")
		logLevel    = fs.String("log-level", "",
			"Diagnostic log level: "+strings.Join(logging.LevelNames(), ", ")+" (default warn).")
		logFile = fs.String("log-file", "",
			"Append diagnostics to this file instead of stderr.")
	)

	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stdout, fs)
			return exitcode.OK
		}
		// flag has already described the offending argument on stderr.
		fmt.Fprintln(stderr, "run `fleetfix --help` for usage.")
		return exitcode.Unknown
	}

	if *showVersion {
		// Byte-compatible with v1.6.0's argparse output. The release workflow asserts
		// this line matches the git tag minus its "v", so the shape is a contract.
		//
		// The error is checked here and nowhere else in this file, and the distinction
		// is deliberate: this is output. Exiting 0 having printed nothing would tell
		// the release workflow, and any operator's script, that a version it never saw
		// was confirmed.
		if _, err := fmt.Fprintf(stdout, "fleetfix %s\n", version.Version()); err != nil {
			fmt.Fprintf(stderr, "fleetfix: writing the version failed: %v\n", err)
			return exitcode.Unknown
		}
		return exitcode.OK
	}

	args := fs.Args()
	if len(args) == 0 {
		// v1 opened the TUI here. This build has no TUI, and silently doing nothing
		// would read as a broken binary rather than an unfinished one.
		fmt.Fprintln(stderr, "fleetfix: no interactive UI in this build; run `fleetfix check --json`.")
		usage(stderr, fs)
		return exitcode.Unknown
	}

	if args[0] == "help" {
		usage(stdout, fs)
		return exitcode.OK
	}

	// The logger is initialised after the subcommand is known to exist but before it
	// runs, so a bad --log-level is a usage error rather than something discovered
	// after a probe ladder has already spent 25 seconds.
	//
	// TUIAttached is false throughout this build: there is no TUI to corrupt yet.
	// It becomes conditional at M5, and the flag exists now so that change is one
	// line rather than a new argument threaded through every caller.
	log, err := logging.Init(logging.Options{Level: *logLevel, File: *logFile, Stderr: stderr})
	if err != nil {
		fmt.Fprintf(stderr, "fleetfix: %v\n", err)
		return exitcode.Unknown
	}
	defer func() {
		if cerr := log.Close(); cerr != nil {
			fmt.Fprintf(stderr, "fleetfix: closing the log file failed: %v\n", cerr)
		}
	}()
	log.Logger.Debug("starting", "version", version.Version(), "command", args[0], "log_destination", log.Destination)

	switch args[0] {
	case "check":
		return runCheck(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "fleetfix: unknown command %q\n", args[0])
		usage(stderr, fs)
		return exitcode.Unknown
	}
}

// A stringList is a repeatable flag that also accepts a comma-separated value.
//
// Both spellings, because both are how this gets used: a person types
// `--check disk --check network`, and an Ansible task templates one string. A
// flag that took only one of them would silently do nothing useful with the
// other -- `--check disk,network` as a single selector matches no check, and
// the run would report an error about a name nobody wrote.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

// runCheck parses `check`'s own flags and runs it.
func runCheck(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fleetfix check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}

	var include, exclude, params stringList
	var (
		asJSON = fs.Bool("json", true, "Emit one indented JSON document on stdout.")
		ndjson = fs.Bool("ndjson", false, "Emit the envelope, then one compact object per check.")
		list   = fs.Bool("list", false, "Print what this build can check and exit.")
		zero   = fs.Bool("exit-zero", false, "Always exit 0; the JSON still reports the real verdict.")
		strict = fs.Bool("strict", false, "Exit unknown when a check was skipped or unavailable.")
	)
	fs.Var(&include, "check", "Run only this check id or domain. Repeatable, comma-separated.")
	fs.Var(&exclude, "exclude", "Do not run this check id or domain. Repeatable, comma-separated.")
	fs.Var(&params, "param", "Supply K=V to an operator-driven check. Repeatable.")

	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			checkcmd.Usage(stdout)
			return exitcode.OK
		}
		fmt.Fprintln(stderr, "run `fleetfix check --help` for usage.")
		return exitcode.Unknown
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(stderr, "fleetfix check: unexpected argument %q\n", rest[0])
		return exitcode.Unknown
	}

	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	format := checkcmd.FormatJSON
	switch {
	case given["json"] && *asJSON && *ndjson:
		fmt.Fprintln(stderr, "fleetfix check: --json and --ndjson are different modes; pick one.")
		return exitcode.Unknown
	case *ndjson:
		format = checkcmd.FormatNDJSON
	case !*asJSON:
		// Refused rather than ignored. Accepting --json=false and emitting JSON anyway
		// would teach a script that the flag does nothing, and that script would still
		// be wrong once the human table lands.
		fmt.Fprintln(stderr, "fleetfix check: --json=false is not implemented in this build; JSON is the only output mode.")
		return exitcode.Unknown
	}

	values, err := paramPairs(params)
	if err != nil {
		fmt.Fprintf(stderr, "fleetfix check: %v\n", err)
		return exitcode.Unknown
	}

	// The host is resolved here, once, and handed to both places that need it: the
	// envelope describes this host, and the collectors shell out through this
	// host's runner. Letting checkcmd resolve its own would put a second read of
	// /etc/os-release and a second sudo -n probe behind the same invocation, and
	// nothing would guarantee the two agreed.
	host := resolve.New(resolve.Options{})

	code, err := checkcmd.Run(context.Background(), checkcmd.Options{
		Stdout:   stdout,
		Version:  version.Version(),
		Resolved: host,
		Registry: builtin.Registry(builtin.Deps{Run: host.Runner}),
		Include:  include,
		Exclude:  exclude,
		Params:   values,
		Format:   format,
		List:     *list,
		ExitZero: *zero,
		Strict:   *strict,
	})
	if err != nil {
		fmt.Fprintf(stderr, "fleetfix check: %v\n", err)
	}
	return code
}

// paramPairs turns --param K=V into the map the runner takes.
//
// A malformed pair is a usage error rather than a silently ignored argument: an
// operator who typed `--param path /var/log` meant to inspect something, and a
// run that quietly skipped the check for want of a parameter it was given would
// be the least helpful possible answer.
func paramPairs(args stringList) (map[string]string, error) {
	out := map[string]string{}
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("--param %q is not K=V", arg)
		}
		out[key] = value
	}
	return out, nil
}

// usage writes the top-level help. Takes the stream because help asked for with
// --help belongs on stdout and help shown after a mistake belongs on stderr.
func usage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(w, `Usage: fleetfix [flags] <command>

Triage toolbox for Ubuntu/Debian fleet operators.

Commands:
  check     Collect host health and write JSON to stdout.
  help      Show this help.

Flags:
`)
	fs.SetOutput(w)
	fs.PrintDefaults()
}
