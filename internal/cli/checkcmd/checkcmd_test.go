package checkcmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/exitcode"
)

func TestRunEmitsAnUnknownResult(t *testing.T) {
	var out strings.Builder

	code, err := Run(&out, "2.0.0-rc0")
	if err != nil {
		t.Fatalf("Run errored: %v", err)
	}
	if code != exitcode.Unknown {
		t.Errorf("code = %d, want %d", code, exitcode.Unknown)
	}
	for _, want := range []string{
		`"schema": "` + SchemaUnimplemented + `"`,
		`"fleetfix_version": "2.0.0-rc0"`,
		`"status": "unknown"`,
		`"exit_code": 3`,
		`"checks": []`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not carry %s:\n%s", want, out.String())
		}
	}
}

// TestTheSchemaIsNotTheRealOne is the guard that stops this placeholder from being
// mistaken for the contract. A consumer pins on the schema string, and a document
// missing the host, operator, privilege and counts blocks must fail that pin.
func TestTheSchemaIsNotTheRealOne(t *testing.T) {
	if SchemaUnimplemented == "fleetfix.check/v1" {
		t.Fatal("the placeholder claims the real schema; a consumer pinning on v1 would accept an empty document")
	}
	if !strings.HasPrefix(SchemaUnimplemented, "fleetfix.check/") {
		t.Errorf("schema = %q, want it to stay in the fleetfix.check namespace so the failure is legible", SchemaUnimplemented)
	}
}

// TestKeyOrderIsExplicit pins the field order to the struct's declaration order.
// encoding/json alphabetises a map but preserves a struct's order, which is the
// reason the envelope is a struct -- v1's audit records depend on the same property
// and the report is compared byte-for-byte from M3 onward.
func TestKeyOrderIsExplicit(t *testing.T) {
	var out strings.Builder
	if _, err := Run(&out, "dev"); err != nil {
		t.Fatalf("Run errored: %v", err)
	}

	want := []string{"schema", "fleetfix_version", "status", "exit_code", "checks", "notes"}
	at := 0
	for _, key := range want {
		i := strings.Index(out.String()[at:], `"`+key+`"`)
		if i < 0 {
			t.Fatalf("key %q is missing or out of order:\n%s", key, out.String())
		}
		at += i
	}
}

// failingWriter is a stdout that will not accept writes -- a closed pipe, which is
// what `fleetfix check --json | head -1` produces.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestAFailedWriteIsReportedNotSwallowed(t *testing.T) {
	sentinel := errors.New("broken pipe")

	code, err := Run(failingWriter{err: sentinel}, "dev")
	if err == nil {
		t.Fatal("Run returned no error after stdout refused the write")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error %v does not wrap the cause", err)
	}
	// Still unknown: nothing was delivered, so the exit code must not claim an answer.
	if code != exitcode.Unknown {
		t.Errorf("code = %d, want %d", code, exitcode.Unknown)
	}
}

func TestUsageDescribesTheCommand(t *testing.T) {
	var out strings.Builder
	Usage(&out)

	for _, want := range []string{"Usage: fleetfix check", "--json", "unknown"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage does not mention %q:\n%s", want, out.String())
		}
	}
}
