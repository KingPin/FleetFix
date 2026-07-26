// Package logging builds FleetFix's diagnostic logger.
//
// This is not the audit log and must never be confused with it. The audit log is
// an authoritative record of who did what, deliberately kept at a low noise floor
// -- only destructive actions, escalations, and lifecycle events. This is the
// channel for everything else: why a probe timed out, which config file was picked
// up, what the container-runtime detection decided. v1 had no such channel, only an
// env-gated _debugtime timer, so every other diagnostic went to a comment or to
// nowhere.
//
// Two hard rules about where it may write.
//
// Never stdout. `check --json` puts JSON on stdout and everything else on stderr,
// always, so a cron job's parser cannot be broken by a log line. A test in this
// package walks the package's own AST to enforce that, because the rule is one
// stray fmt.Println away from being false.
//
// Never the terminal while a TUI owns it. tcell puts the terminal in raw mode and
// draws by absolute cursor position; a stray line on stderr does not scroll past,
// it scribbles over the interface and stays there. So with a TUI attached, output
// goes to a file or nowhere.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
)

// DefaultLevel is what runs without --log-level.
//
// WARN, not INFO: the default has to be quiet enough that an operator running
// `fleetfix check --json` in cron never sees a line of it on stderr, while still
// surfacing the things they would want to know went wrong.
const DefaultLevel = slog.LevelWarn

// levels is the accepted set of --log-level values.
//
// "warning" is here because people type it. Accepting a synonym costs nothing;
// rejecting it costs an operator a confused minute at exactly the moment they are
// trying to diagnose something else.
var levels = map[string]slog.Level{
	"debug":   slog.LevelDebug,
	"info":    slog.LevelInfo,
	"warn":    slog.LevelWarn,
	"warning": slog.LevelWarn,
	"error":   slog.LevelError,
}

// LevelNames returns the accepted --log-level values, sorted, for flag help.
// One source for the parser and the help text so they cannot drift.
func LevelNames() []string {
	out := make([]string, 0, len(levels))
	for name := range levels {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ParseLevel resolves a --log-level value. An empty string is DefaultLevel, so a
// caller can pass the flag through unconditionally.
//
// Exported so the flag parser can reject a bad value before any work starts.
// Discovering the level was misspelled after a 25s probe ladder has already run is
// a worse experience than a usage error.
func ParseLevel(s string) (slog.Level, error) {
	if strings.TrimSpace(s) == "" {
		return DefaultLevel, nil
	}
	if lvl, ok := levels[strings.ToLower(strings.TrimSpace(s))]; ok {
		return lvl, nil
	}
	return 0, fmt.Errorf("logging: unknown level %q: want one of %s", s, strings.Join(LevelNames(), ", "))
}

// Options configures Init.
type Options struct {
	// Level is a --log-level value. Empty means DefaultLevel.
	Level string

	// File is a --log-file path. Empty means no file.
	File string

	// TUIAttached reports that a TUI owns the terminal, which makes stderr
	// unusable. With no File set this leaves the logger writing nowhere -- the
	// deliberate choice, because a corrupted interface is a worse failure than a
	// missing diagnostic, and the operator can always add --log-file.
	TUIAttached bool

	// Stderr overrides the stream used when nothing else applies. Nil means
	// os.Stderr. Only tests should set it; it exists so a test can prove nothing
	// was written rather than assert it.
	Stderr io.Writer
}

// Handle is a live logger plus what a caller needs to describe and release it.
type Handle struct {
	Logger *slog.Logger

	// Destination describes where records go, for `fleetfix doctor` and for the
	// startup debug line. One of DestStderr, DestDiscarded, or a file path.
	Destination string

	// Level is the resolved level, so doctor can report it without re-parsing.
	Level slog.Level

	file *os.File
}

// Sentinel Destination values for the two non-file cases.
const (
	DestStderr    = "stderr"
	DestDiscarded = "discarded"
)

// Init builds a logger and installs it as slog's default.
//
// Installing the default matters most in the TUI case: any package that reaches
// for slog.Default() -- ours or a dependency's -- would otherwise write to the real
// stderr and corrupt the interface. Pointing the default at the same handler makes
// that structurally impossible rather than a convention.
//
// The returned Handle must be closed to flush and release a log file.
func Init(o Options) (*Handle, error) {
	lvl, err := ParseLevel(o.Level)
	if err != nil {
		return nil, err
	}

	dest := DestStderr
	w := o.Stderr
	if w == nil {
		w = os.Stderr
	}
	var file *os.File

	switch {
	case o.File != "":
		// Append, never truncate: a restarting agent must not erase the history that
		// explains why it restarted.
		//
		// 0600 because a diagnostic line can carry paths and command lines belonging
		// to other users' processes -- material that is readable to root's log
		// reader by necessity, not to everyone with a shell on the box.
		//
		// The final path component is followed if it is a symlink. That is
		// acceptable while --log-file is a command-line argument, since it carries
		// the invoking operator's own authority; when M3 makes log paths
		// config-file-driven, a root-run agent following an operator-writable
		// symlink stops being the same question and this needs revisiting.
		f, err := os.OpenFile(o.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			// Surfaced, never swallowed. A logger that silently wrote nowhere because
			// the path had a typo is how an operator spends an afternoon wondering
			// where their diagnostics went.
			return nil, fmt.Errorf("logging: cannot open log file %s: %w", o.File, err)
		}
		file = f
		w = f
		dest = o.File
	case o.TUIAttached:
		w = io.Discard
		dest = DestDiscarded
	}

	handler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: lvl,
		// Source locations only at debug. They are what makes a debug trace
		// actionable, and pure noise at the levels an operator actually sees.
		AddSource: lvl <= slog.LevelDebug,
	})

	logger := slog.New(handler)
	slog.SetDefault(logger)

	return &Handle{Logger: logger, Destination: dest, Level: lvl, file: file}, nil
}

// Close releases the log file, if there is one. Safe on a Handle with no file and
// safe to call twice.
func (h *Handle) Close() error {
	if h == nil || h.file == nil {
		return nil
	}
	f := h.file
	h.file = nil
	return f.Close()
}
