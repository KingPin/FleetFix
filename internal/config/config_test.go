package config

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

// captureWarnings installs a logger that records WARN-and-above into a buffer for
// the duration of the test. The distinction between a file that is missing and one
// that is broken is only visible in the log, so it has to be asserted on.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestReadProbesYAML(t *testing.T) {
	log := captureWarnings(t)
	got, err := ReadProbesYAML(fixture.Path("probes/full_example.yml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ping, ok := got["ping"].(map[string]any)
	if !ok {
		t.Fatalf("ping is %T, want a mapping: %#v", got["ping"], got)
	}
	if want := int64(5); ping["count"] != want {
		t.Errorf("ping.count = %#v, want %#v", ping["count"], want)
	}
	if want := 0.3; ping["interval_s"] != want {
		t.Errorf("ping.interval_s = %#v, want %#v", ping["interval_s"], want)
	}
	if diff := cmp.Diff([]any{"10.0.0.1", "8.8.8.8"}, ping["targets"]); diff != "" {
		t.Errorf("ping.targets (-want +got):\n%s", diff)
	}
	if log.Len() != 0 {
		t.Errorf("a readable file logged: %s", log)
	}
}

func TestReadPathsYAML(t *testing.T) {
	got, err := ReadPathsYAML(fixture.Path("paths/target_user.yml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{"target_user": "appuser", "stale_age_days": int64(30)}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestReadPerfYAML(t *testing.T) {
	got, err := ReadPerfYAML(fixture.Path("perf/reduce_animations_true.yml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{"reduce_animations": true}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

// The four loaders are separate functions holding one implementation, so they must
// agree until one of them grows its own validation.
//
// otel.yml is in here rather than in a reader test of its own because v1 kept a
// second, byte-identical copy of the loader in audit/otel.py: the claim worth
// testing is that collapsing the two changed nothing, and that is a claim about
// agreement.
func TestLoadersAgree(t *testing.T) {
	path := fixture.Path("probes/full_example.yml")
	probes, _ := ReadProbesYAML(path)
	others := map[string]map[string]any{
		"paths": mustRead(t, ReadPathsYAML, path),
		"perf":  mustRead(t, ReadPerfYAML, path),
		"otel":  mustRead(t, ReadOtelYAML, path),
	}
	for name, got := range others {
		if diff := cmp.Diff(probes, got); diff != "" {
			t.Errorf("probes vs %s (-probes +%s):\n%s", name, name, diff)
		}
	}
}

func mustRead(t *testing.T, read func(string) (map[string]any, error), path string) map[string]any {
	t.Helper()
	got, err := read(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return got
}

// A per-host override that is simply absent is the normal case, not a problem, so
// v1 says nothing about it and neither does this.
func TestReadYAMLMappingMissingFileIsSilent(t *testing.T) {
	log := captureWarnings(t)
	got, err := ReadProbesYAML(filepath.Join(t.TempDir(), "nope.yml"))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %#v, want an empty map", got)
	}
	if got == nil {
		t.Error("returned a nil map; callers index it without checking")
	}
	if log.Len() != 0 {
		t.Errorf("a missing file logged: %s", log)
	}
}

func TestReadYAMLMappingUnreadableFileIsSilent(t *testing.T) {
	log := captureWarnings(t)
	// A directory: os.ReadFile fails with EISDIR, which is an OSError to v1.
	got, err := ReadProbesYAML(t.TempDir())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %#v, want an empty map", got)
	}
	if log.Len() != 0 {
		t.Errorf("an unreadable file logged: %s", log)
	}
}

// A file that was found and then ignored is the one case the operator has to be
// told about -- silently running on defaults is how a threshold override gets
// debugged for an hour. v1 logged it; this logs it and returns it, so `check --json`
// can carry it in config_warnings[].
func TestReadYAMLMappingParseFailureWarnsAndReturns(t *testing.T) {
	for _, rel := range []string{"probes/malformed.yml", "probes/broken_mapping.yml", "paths/malformed.yml", "perf/malformed.yml"} {
		t.Run(rel, func(t *testing.T) {
			log := captureWarnings(t)
			got, err := ReadProbesYAML(fixture.Path(rel))
			if err == nil {
				t.Fatal("want an error")
			}
			if len(got) != 0 {
				t.Errorf("got %#v, want an empty map", got)
			}
			if s := log.String(); !strings.Contains(s, "failed to parse config file") || !strings.Contains(s, rel) {
				t.Errorf("warning does not name the file: %s", s)
			}
		})
	}
}

// `data if isinstance(data, dict) else {}`: a top-level list parses fine and is
// then discarded, with nothing logged, because nothing failed.
func TestReadYAMLMappingNonMappingIsEmpty(t *testing.T) {
	log := captureWarnings(t)
	got, err := ReadProbesYAML(fixture.Path("probes/top_level_list.yml"))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %#v, want an empty map", got)
	}
	if log.Len() != 0 {
		t.Errorf("a non-mapping document logged: %s", log)
	}
}
