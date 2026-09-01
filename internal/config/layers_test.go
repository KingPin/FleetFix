package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolvePathsFollowsTheSpecWhereV1FollowedPythonsDefaultArgument is the
// measurement of issue #11.
//
// The "v1" column was produced by running v1's own expression --
// Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config")) / "fleetfix" --
// under each environment on the interpreter the release binary bundles. Where the
// two columns differ, this port is deliberately not reproducing v1: an empty or
// relative XDG variable made the config directory relative, so FleetFix read
// ./fleetfix/probes.yml out of whatever the working directory happened to be, and
// the same expression governs the state directory the audit log falls back to.
func TestResolvePathsFollowsTheSpecWhereV1FollowedPythonsDefaultArgument(t *testing.T) {
	const home = "/home/tester"
	tests := []struct {
		name string
		set  bool
		xdg  string
		v1   string // what v1 produced, for the record
		want string
	}{
		{"unset", false, "", "/home/tester/.config/fleetfix", "/home/tester/.config/fleetfix"},
		{"absolute", true, "/opt/cfg", "/opt/cfg/fleetfix", "/opt/cfg/fleetfix"},
		{"trailing slash", true, "/opt/cfg/", "/opt/cfg/fleetfix", "/opt/cfg/fleetfix"},
		// The two that diverge.
		{"empty", true, "", "fleetfix", "/home/tester/.config/fleetfix"},
		{"relative", true, "cfgdir", "cfgdir/fleetfix", "/home/tester/.config/fleetfix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			if tt.set {
				t.Setenv("XDG_CONFIG_HOME", tt.xdg)
			} else {
				os.Unsetenv("XDG_CONFIG_HOME")
			}
			got := ResolvePaths().UserDir
			if got != tt.want {
				t.Errorf("UserDir = %q, want %q (v1 gave %q)", got, tt.want, tt.v1)
			}
			if !filepath.IsAbs(got) {
				t.Errorf("UserDir %q is relative; config would be read from the working directory", got)
			}
		})
	}
}

// The cache and state directories run through the same resolver, and the state
// one matters most: it is where the audit log lands when /var/log is not
// writable, so a relative answer writes the trail next to wherever the process
// was started.
func TestEveryResolvedDirectoryIsAbsolute(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, env := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"} {
		t.Setenv(env, "")
	}
	p := ResolvePaths()
	dirs := map[string]string{
		"UserDir": p.UserDir, "CacheDir": p.CacheDir,
		"StateDir": p.StateDir, "SystemDir": p.SystemDir,
	}
	for name, dir := range dirs {
		if !filepath.IsAbs(dir) {
			t.Errorf("%s = %q, which is relative", name, dir)
		}
		if !strings.HasSuffix(dir, "fleetfix") {
			t.Errorf("%s = %q, want it under a fleetfix directory", name, dir)
		}
	}
	// v1's defaults, kept: .config, .cache, .local/state.
	if p.CacheDir != "/home/tester/.cache/fleetfix" {
		t.Errorf("CacheDir = %q", p.CacheDir)
	}
	if p.StateDir != "/home/tester/.local/state/fleetfix" {
		t.Errorf("StateDir = %q", p.StateDir)
	}
}

// HOME is resolved the way Path.home() does -- the variable, then the passwd
// database -- and a relative HOME is no more usable than a relative XDG value.
func TestHomeDirRefusesARelativeHome(t *testing.T) {
	t.Setenv("HOME", "not/absolute")
	got := homeDir()
	if !filepath.IsAbs(got) {
		t.Fatalf("homeDir() = %q, want an absolute path", got)
	}
	if got == "not/absolute" {
		t.Error("a relative HOME was taken literally")
	}
}

func writeYAML(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	return Paths{
		SystemDir: filepath.Join(root, "etc"),
		UserDir:   filepath.Join(root, "user"),
	}
}

// The direction of precedence is the compatibility story: an operator who has a
// personal probes.yml today must see exactly what they saw before v2 added the
// fleet-wide layer underneath it.
func TestUserLayerWinsOverTheFleetLayer(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.SystemDir, ProbesFile, "ping:\n  count: 10\n  interval_s: 1.0\n")
	writeYAML(t, p.UserDir, ProbesFile, "ping:\n  count: 3\n")

	got := p.Load(ProbesFile)
	ping, _ := got.Values["ping"].(map[string]any)
	if ping["count"] != int64(3) {
		t.Errorf("count = %v (%T), want the user's 3", ping["count"], ping["count"])
	}
	// The key the user did not mention survives from the fleet layer. Whole-file
	// replacement would silently drop four of a five-knob fleet policy the moment
	// one operator overrode the fifth.
	if ping["interval_s"] != 1.0 {
		t.Errorf("interval_s = %v, want the fleet layer's 1.0 to survive", ping["interval_s"])
	}
	if len(got.Warnings) != 0 {
		t.Errorf("warnings on a clean load: %v", got.Warnings)
	}
	if eff := got.Effective(); len(eff) != 2 {
		t.Errorf("Effective() = %v, want both layers", eff)
	}
}

// Lists replace rather than concatenate. Concatenation would make a shipped or
// fleet-wide default impossible to remove: an operator writing two ping targets
// would get five and have no way to say "only these".
func TestListsReplaceRatherThanConcatenate(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.SystemDir, ProbesFile, "ping:\n  targets: [a, b, c]\n")
	writeYAML(t, p.UserDir, ProbesFile, "ping:\n  targets: [z]\n")

	ping, _ := p.Load(ProbesFile).Values["ping"].(map[string]any)
	targets, _ := ping["targets"].([]any)
	if len(targets) != 1 || targets[0] != "z" {
		t.Errorf("targets = %v, want only the user's [z]", targets)
	}
}

// A scalar replacing a mapping, and a mapping replacing a scalar: both are the
// operator changing the shape of a key, and neither can be merged.
func TestAChangeOfShapeReplaces(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.SystemDir, PerfFile, "reduce_animations: {mode: auto}\nother: 1\n")
	writeYAML(t, p.UserDir, PerfFile, "reduce_animations: true\nother: {nested: 1}\n")

	v := p.Load(PerfFile).Values
	if v["reduce_animations"] != true {
		t.Errorf("reduce_animations = %v, want the user's bool to replace the mapping", v["reduce_animations"])
	}
	nested, ok := v["other"].(map[string]any)
	if !ok || nested["nested"] != int64(1) {
		t.Errorf("other = %v, want the user's mapping to replace the scalar", v["other"])
	}
}

func TestLoadWithNoFilesAtAllIsUsable(t *testing.T) {
	p := testPaths(t)
	got := p.Load(ThresholdsFile)
	if got.Values == nil {
		t.Fatal("Values is nil; a caller indexing it would panic")
	}
	if len(got.Values) != 0 || len(got.Warnings) != 0 {
		t.Errorf("an empty host produced %v / %v", got.Values, got.Warnings)
	}
	// Both candidates are still reported, because doctor's job is to show the
	// operator the path being read -- a list of only the files that exist cannot
	// tell them they wrote to the wrong one.
	if len(got.Sources) != 2 {
		t.Fatalf("Sources = %v, want both candidates listed", got.Sources)
	}
	for _, s := range got.Sources {
		if s.Exists {
			t.Errorf("%s reported as existing", s.Path)
		}
	}
	if got.Effective() != nil {
		t.Errorf("Effective() = %v, want nothing in play", got.Effective())
	}
}

// A file that will not parse contributes nothing, warns, and does not stop the
// other layer. A fleet tool that goes silent over a stray tab is worse than one
// running on the layer that did parse.
func TestAnUnparseableLayerDegradesAndSaysSo(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.SystemDir, ProbesFile, "ping: [unclosed\n")
	writeYAML(t, p.UserDir, ProbesFile, "ping:\n  count: 3\n")

	got := p.Load(ProbesFile)
	ping, _ := got.Values["ping"].(map[string]any)
	if ping["count"] != int64(3) {
		t.Errorf("the good layer did not load: %v", got.Values)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], p.SystemDir) {
		t.Fatalf("warnings = %v, want one naming the file that failed", got.Warnings)
	}
	// The source records the failure separately from absence: doctor has to be
	// able to say "found and ignored", which is a different fix from "not found".
	sys := got.Sources[0]
	if !sys.Exists || sys.Err == nil {
		t.Errorf("the bad layer reported as %+v, want exists with an error", sys)
	}
	if eff := got.Effective(); len(eff) != 1 || !strings.Contains(eff[0], p.UserDir) {
		t.Errorf("Effective() = %v, want only the layer that parsed", eff)
	}
	if desc := strings.Join(got.DescribeLayers(), "\n"); !strings.Contains(desc, "unparseable") ||
		!strings.Contains(desc, "in use") {
		t.Errorf("DescribeLayers is not legible:\n%s", desc)
	}
}

// A file that is there and will not open is the case v1 could not tell from a
// file that was never written, and the two want opposite things from the
// operator: one is a mode bit to fix, the other is a file to create. Reporting
// an unreadable layer as absent sends them to write a file that already exists.
//
// Staged as a directory where a file should be, so the case is the same for root
// -- who can read a 0000 file -- as for anyone else.
func TestAnUnreadableLayerIsFoundNotAbsent(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(filepath.Join(p.SystemDir, ProbesFile), 0o755); err != nil {
		t.Fatal(err)
	}
	writeYAML(t, p.UserDir, ProbesFile, "ping:\n  count: 3\n")

	got := p.Load(ProbesFile)
	ping, _ := got.Values["ping"].(map[string]any)
	if ping["count"] != int64(3) {
		t.Errorf("the readable layer did not load: %v", got.Values)
	}
	sys := got.Sources[0]
	if !sys.Exists || sys.Err == nil {
		t.Errorf("the unreadable layer reported as %+v, want exists with an error", sys)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], p.SystemDir) {
		t.Fatalf("warnings = %v, want one naming the file that could not be read", got.Warnings)
	}
	if eff := got.Effective(); len(eff) != 1 || !strings.Contains(eff[0], p.UserDir) {
		t.Errorf("Effective() = %v, want only the layer that was read", eff)
	}
	// Named apart from a parse failure: chmod is a different fix from a syntax
	// error, and telling an operator their file is unparseable sends them to read
	// a file they cannot open.
	desc := strings.Join(got.DescribeLayers(), "\n")
	if !strings.Contains(desc, "unreadable") || strings.Contains(desc, "absent") {
		t.Errorf("DescribeLayers does not tell unreadable from absent:\n%s", desc)
	}
}

// An empty file is an operator saying "nothing here", which is not the same as
// not having written one -- doctor should show it as in play.
func TestAnEmptyFileIsInPlayNotAbsent(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.UserDir, PathsFile, "")

	got := p.Load(PathsFile)
	if len(got.Values) != 0 {
		t.Errorf("an empty file contributed %v", got.Values)
	}
	user := got.Sources[1]
	if !user.Exists || user.Err != nil {
		t.Errorf("empty file reported as %+v, want exists with no error", user)
	}
	if eff := got.Effective(); len(eff) != 1 {
		t.Errorf("Effective() = %v, want the empty file counted as in play", eff)
	}
	if !strings.Contains(strings.Join(got.DescribeLayers(), "\n"), "absent") {
		t.Error("the layer that really is absent is not shown as absent")
	}
}

// A top level that is not a mapping is v1's "return {}" case, and it must not
// wipe out the layer below it.
func TestANonMappingTopLevelContributesNothing(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.SystemDir, ProbesFile, "ping:\n  count: 7\n")
	writeYAML(t, p.UserDir, ProbesFile, "- just\n- a\n- list\n")

	ping, _ := p.Load(ProbesFile).Values["ping"].(map[string]any)
	if ping["count"] != int64(7) {
		t.Errorf("a list-shaped user file destroyed the fleet layer: %v", ping)
	}
}

// Merging must not write through into either input: base is the accumulator and
// over is a decoded file, and a merge that mutated either would make the order
// of two independent Load calls matter.
func TestMergeDoesNotMutateItsInputs(t *testing.T) {
	base := map[string]any{"ping": map[string]any{"count": 10, "interval_s": 1.0}}
	over := map[string]any{"ping": map[string]any{"count": 3}}
	merged := mergeMappings(base, over)

	if b, _ := base["ping"].(map[string]any); b["count"] != 10 {
		t.Errorf("base was mutated: %v", base)
	}
	if o, _ := over["ping"].(map[string]any); len(o) != 1 {
		t.Errorf("over was mutated: %v", over)
	}
	if m, _ := merged["ping"].(map[string]any); m["count"] != 3 || m["interval_s"] != 1.0 {
		t.Errorf("merged = %v", merged)
	}
}

// Deeper than one level, because probes.yml nests and a merge that stopped at the
// first level would quietly drop the sibling knobs it was written to preserve.
func TestMergeIsRecursive(t *testing.T) {
	merged := mergeMappings(
		map[string]any{"a": map[string]any{"b": map[string]any{"c": 1, "d": 2}}},
		map[string]any{"a": map[string]any{"b": map[string]any{"c": 9}}},
	)
	a, _ := merged["a"].(map[string]any)
	b, _ := a["b"].(map[string]any)
	if b["c"] != 9 || b["d"] != 2 {
		t.Errorf("merged = %v, want c overridden and d kept", merged)
	}
}

// A misspelled key in YAML is silent -- the file parses, the value is stored, and
// nothing reads it -- so the operator's setting is ignored with no indication why.
func TestUnknownKeysAreReportedSorted(t *testing.T) {
	got := UnknownKeys(map[string]any{
		"ping": 1, "treshold": 2, "dns": 3, "aaa": 4,
	}, "ping", "dns")
	if len(got) != 2 || got[0] != "aaa" || got[1] != "treshold" {
		t.Errorf("UnknownKeys = %v, want [aaa treshold]", got)
	}
	if UnknownKeys(map[string]any{"ping": 1}, "ping") != nil {
		t.Error("a clean file produced unknown keys")
	}
	// Nothing known means everything is unknown, which is the honest answer for a
	// caller that has not said what it consumes.
	if len(UnknownKeys(map[string]any{"a": 1, "b": 2})) != 2 {
		t.Error("UnknownKeys with no known list reported nothing")
	}
}

func TestDescribeNamesEveryDirectory(t *testing.T) {
	p := Paths{SystemDir: "/etc/fleetfix", UserDir: "/u/c", CacheDir: "/u/ca", StateDir: "/u/s"}
	got := p.Describe()
	for _, want := range []string{"/etc/fleetfix", "/u/c", "/u/ca", "/u/s"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() omits %q:\n%s", want, got)
		}
	}
}

// A Paths with an empty directory skips that layer rather than reading the file
// name relative to the working directory -- the same failure issue #11 is about,
// arriving by a different route.
func TestAnEmptyLayerDirectoryIsSkipped(t *testing.T) {
	root := t.TempDir()
	p := Paths{SystemDir: "", UserDir: filepath.Join(root, "user")}
	writeYAML(t, p.UserDir, PerfFile, "reduce_animations: true\n")

	got := p.Load(PerfFile)
	if len(got.Sources) != 1 {
		t.Fatalf("Sources = %v, want only the configured layer", got.Sources)
	}
	if got.Values["reduce_animations"] != true {
		t.Errorf("Values = %v", got.Values)
	}
}
