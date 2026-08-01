// Package container resolves which container runtime this host has, if any.
//
// v1 had no such resolver. Every docker call caught FileNotFoundError where it
// stood, so a host without docker produced the same "unavailable" verdict several
// times over with no one place that knew why, and a host whose daemon was merely
// stopped produced a different confusing message per call site. Resolving once
// means the domain reports one reason, and the reason is the daemon's own.
//
// Detection is a table, not a chain of ifs, because the podman dialect is deferred
// rather than declined -- see issue #7, which could not be built without captures
// from a real podman host. Adding it is one entry plus the parsers it needs, not a
// second seam alongside this one; the Kind constant is already here so nothing
// downstream has to change shape when it lands.
package container

import (
	"context"
	"fmt"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
)

// Kind names a runtime. These strings reach the report, so they are a contract:
// a consumer grouping hosts by runtime pins on them.
type Kind string

const (
	// None means no runtime was found. The empty string rather than "none", so
	// the zero Runtime is already the honest answer.
	None Kind = ""

	// Docker is the only runtime this build detects.
	Docker Kind = "docker"

	// Podman is reserved and never returned by this build. The dialect is issue
	// #7: podman's ps, stats and system df output has to be captured from a real
	// host before a parser for it can exist, and hand-writing those fixtures
	// would ship a parser that passes its tests and fails on contact. A
	// podman-only host keeps reporting the docker domain unavailable, which is
	// v1's behaviour -- a gap that stays the same size rather than a regression.
	Podman Kind = "podman"
)

// DockerHostEnv points the docker CLI at a socket other than the default. It is
// how rootless docker is reachable at all: the daemon listens under the user's
// runtime directory and nothing finds it without this.
const DockerHostEnv = "DOCKER_HOST"

// candidates are tried in order. Podman belongs here, one line, when issue #7
// lands.
var candidates = []struct {
	kind Kind
	bin  string
}{
	{Docker, "docker"},
}

// A Runtime is the answer: which runtime, which executable, and why.
//
// The zero value means none was found, which is a supported host rather than an
// error -- a great many machines in a fleet run no containers at all.
type Runtime struct {
	Kind Kind

	// Bin is the executable the collectors invoke. Empty when Kind is None.
	Bin string

	// Endpoint is DOCKER_HOST when it is set, empty otherwise. Carried because a
	// docker reporting on a different socket than the operator assumes is the
	// hardest kind of wrong answer to spot: everything works, and it is about
	// somewhere else.
	Endpoint string

	// Reason is one operator-facing line for `fleetfix doctor` and the report.
	Reason string

	// Available is set by Probe. Detect leaves it false, because finding the CLI
	// says nothing about whether a daemon will answer it.
	Available bool
}

// Found reports whether a runtime's command-line tool is installed.
func (r Runtime) Found() bool { return r.Kind != None }

// Detect finds the runtime from PATH alone. No subprocess: this runs on every
// invocation including ones that will not touch containers at all.
func Detect(look cmdrun.Looker, getenv func(string) string) Runtime {
	endpoint := getenv(DockerHostEnv)
	for _, c := range candidates {
		if _, err := look.Look(c.bin); err != nil {
			continue
		}
		return Runtime{
			Kind:     c.kind,
			Bin:      c.bin,
			Endpoint: endpoint,
			Reason:   found(c.bin, endpoint),
		}
	}
	return Runtime{Reason: notFound(endpoint)}
}

func found(bin, endpoint string) string {
	if endpoint == "" {
		return fmt.Sprintf("%s is installed", bin)
	}
	return fmt.Sprintf("%s is installed, talking to %s=%s", bin, DockerHostEnv, endpoint)
}

func notFound(endpoint string) string {
	// A DOCKER_HOST with no docker to use it is worth saying out loud: it is
	// usually a shell profile that outlived the package, and the operator
	// otherwise reads "no container runtime" on a host they are certain has one.
	if endpoint != "" {
		return fmt.Sprintf(
			"no container runtime is installed, though %s=%s is set", DockerHostEnv, endpoint,
		)
	}
	return "no container runtime is installed"
}

// probeArgs asks the runtime for its server version.
//
// The cheapest question that only a running daemon can answer. The output is not
// parsed in the sense the ported collectors are -- there is no grammar here, just
// a trimmed line put in front of a human -- so this command needs no fixture and
// adds nothing to the differential corpus.
var probeArgs = []string{"version", "--format", "{{.Server.Version}}"}

// Probe asks whether the daemon actually answers, and returns an updated Runtime.
//
// An installed CLI and a running daemon are different facts, and the gap between
// them is the single most common container-domain support call: docker is
// present, the socket is not, and every check in the domain reports the same
// connection error separately. Resolved once here, the domain reports unavailable
// with the daemon's own explanation.
//
// Never fails. An unreachable daemon is a host state, not a tool error.
func (r Runtime) Probe(ctx context.Context, run cmdrun.Runner) Runtime {
	if !r.Found() {
		return r
	}

	res, err := run.Run(ctx, r.Bin, probeArgs...)
	switch {
	case cmdrun.IsNotFound(err):
		// It was on PATH when Detect looked and is not now, or PATH differs
		// between the lookup and the run. Rare, and reporting it as absent is
		// both true and the same answer Detect would have given.
		return Runtime{Endpoint: r.Endpoint, Reason: notFound(r.Endpoint)}
	case cmdrun.IsTimeout(err):
		r.Reason = fmt.Sprintf("%s did not answer; the daemon may be starting or wedged", r.Bin)
		return r
	case err != nil:
		r.Reason = fmt.Sprintf("%s could not be run: %v", r.Bin, err)
		return r
	case !res.OK():
		// docker prints "Cannot connect to the Docker daemon at
		// unix:///var/run/docker.sock" here, socket path included. That is a
		// better sentence than anything written in advance, so it is passed
		// through rather than replaced.
		r.Reason = fmt.Sprintf("%s is installed but the daemon did not answer: %s", r.Bin, firstLine(res.Combined()))
		return r
	}

	r.Available = true
	r.Reason = available(r.Bin, strings.TrimSpace(res.Stdout), r.Endpoint)
	return r
}

func available(bin, version, endpoint string) string {
	if version == "" {
		// A daemon that answered without naming itself. It answered, which is
		// the fact that matters.
		version = "an unnamed version"
	} else {
		version = "version " + version
	}
	if endpoint == "" {
		return fmt.Sprintf("%s daemon %s", bin, version)
	}
	return fmt.Sprintf("%s daemon %s at %s=%s", bin, version, DockerHostEnv, endpoint)
}

// firstLine keeps a multi-line daemon error to one line. docker's connection
// error is followed by "Is the docker daemon running?", which is advice the
// operator staring at this line does not need twice.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
