package main

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/core/system"
)

func TestSystemErrNamesThePythonException(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		// Wrapped, because the readers add the file name and the field to the
		// sentinel and an == comparison here would pass on a bare sentinel while
		// failing on everything the readers actually return.
		{"short file", fmt.Errorf("uptime: %w", system.ErrShortFile), "IndexError"},
		{"bad number", fmt.Errorf("uptime: %w", system.ErrBadNumber), "ValueError"},
		{"absent", &fs.PathError{Op: "open", Path: "uptime", Err: fs.ErrNotExist}, "FileNotFoundError"},
		{"denied", &fs.PathError{Op: "open", Path: "uptime", Err: fs.ErrPermission}, "PermissionError"},
		// Not a code CPython can produce, which is the point: a failure this
		// mapping does not recognise is a bug in the port, not a reproduction of
		// v1 behaviour, and must not be able to compare equal to one.
		{"anything else", errors.New("boom"), "GoError"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorCode(systemErr(tt.err)); got != tt.want {
				t.Errorf("errorCode(systemErr(%v)) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestSystemAdaptersReportAnUnreadableFileAsPythonDoes(t *testing.T) {
	// The oracle hands each adapter a real path, so a missing one is the only
	// failure reachable through the adapter rather than through the reader.
	missing := filepath.Join(t.TempDir(), "not-here")
	for _, fn := range []string{"system.read_uptime", "system.read_loadavg", "system.read_meminfo"} {
		t.Run(fn, func(t *testing.T) {
			v, err := dispatch[fn].run(missing, nil)
			if got := errorCode(err); got != "FileNotFoundError" {
				t.Errorf("dispatch[%q] on a missing file = %q, want FileNotFoundError", fn, got)
			}
			if v != nil {
				t.Errorf("dispatch[%q] returned %v alongside its error, want nil", fn, v)
			}
		})
	}
}
