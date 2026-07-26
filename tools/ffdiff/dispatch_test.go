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

// TestUpdateAdaptersReturnPythonsTuple pins the wire shape of the two parsers
// that return a pair rather than a struct. Python's tuple decodes as a JSON
// array, so the Go side has to hand back one too -- an object with named fields
// would be a divergence on every case, and a bare count would silently compare
// equal to the first element of the pair.
func TestUpdateAdaptersReturnPythonsTuple(t *testing.T) {
	tests := []struct {
		name string
		fn   string
		text string
		want any
	}{
		{"notifier", "system.parse_notifier_text", "3 packages can be updated.\n5 security updates\n", []int64{3, 5}},
		// None, not (0, 0): the null is what the comparison has to see.
		{"notifier with no count", "system.parse_notifier_text", "Welcome to Ubuntu\n", nil},
		{"apt", "system.parse_apt_upgradable", "Listing...\nopenssl/jammy-security 3.0.2\n", []int64{1, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dispatch[tt.fn].run(tt.text, nil)
			if err != nil {
				t.Fatalf("dispatch[%q] returned %v", tt.fn, err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("dispatch[%q](%q) = %#v, want %#v", tt.fn, tt.text, got, tt.want)
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
