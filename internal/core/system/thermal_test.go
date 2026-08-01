package system

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

// Expectations measured against v1's src/fleetfix/modules/system/thermal.py, by
// materialising testdata/thermal/zones.json into a temp directory and handing
// read_zones the Path.

// zoneFS builds a /sys/class/thermal from a name -> file-contents map, so a case
// reads as the tree it is describing. A name with no files is a zone directory
// that exists and holds nothing, which is what a driver that registered and then
// failed looks like.
func zoneFS(zones map[string]map[string]string) fstest.MapFS {
	mapfs := fstest.MapFS{}
	for zone, files := range zones {
		mapfs[zone] = &fstest.MapFile{Mode: fs.ModeDir | 0o555}
		for name, data := range files {
			mapfs[zone+"/"+name] = &fstest.MapFile{Data: []byte(data)}
		}
	}
	return mapfs
}

// deniedFS is a filesystem where one path exists and refuses to open, the shape
// fstest.MapFS has no way to express. It is what a sysfs attribute whose driver
// returns EACCES looks like, and it is the only input that makes ReadZones fail
// rather than skip.
type deniedFS struct {
	fs.FS
	denied string
	err    error
}

func (d deniedFS) Open(name string) (fs.File, error) {
	if name == d.denied {
		return nil, &fs.PathError{Op: "open", Path: name, Err: d.err}
	}
	return d.FS.Open(name)
}

func TestReadZonesFixture(t *testing.T) {
	got, err := ReadZones(fixture.Tree(t, "thermal/zones.json"), ".")
	if err != nil {
		t.Fatalf("ReadZones() error = %v", err)
	}
	want := []ThermalZone{
		// Lexicographic, so thermal_zone10 lands third and thermal_zone9 last.
		{Name: "thermal_zone0", Type: "x86_pkg_temp", TempC: 55.123},
		// Both files are stripped.
		{Name: "thermal_zone1", Type: "acpitz", TempC: 42},
		// An empty type file is an empty label, not a fallback to the name: the file
		// exists, so v1 reads it and gets "".
		{Name: "thermal_zone10", Type: "", TempC: 1},
		// No type file, so the directory name stands in.
		{Name: "thermal_zone4", Type: "thermal_zone4", TempC: -273.15},
		// str.strip() removes the U+001C the fixture puts in front of the digits;
		// int()'s own whitespace skip would not have.
		{Name: "thermal_zone5", Type: "thermal_zone5", TempC: 7},
		{Name: "thermal_zone6", Type: "thermal_zone6", TempC: 42},
		{Name: "thermal_zone9", Type: "x86_pkg_temp", TempC: 55.123},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ReadZones() mismatch (-want +got):\n%s", diff)
	}
}

// TestReadZonesFixtureSkips names what the fixture holds that does not come back,
// so a change that starts reporting one of them fails here rather than showing up
// as an extra row somebody has to account for.
func TestReadZonesFixtureSkips(t *testing.T) {
	got, err := ReadZones(fixture.Tree(t, "thermal/zones.json"), ".")
	if err != nil {
		t.Fatalf("ReadZones() error = %v", err)
	}
	seen := map[string]bool{}
	for _, z := range got {
		seen[z.Name] = true
	}
	skipped := map[string]string{
		"cooling_device0": "not a thermal_zone, so the glob never sees it",
		"thermal_zone11":  "an empty zone directory: no temp file",
		"thermal_zone2":   "a type file and no temp file",
		"thermal_zone3":   "a temperature int() cannot read",
		"thermal_zone7":   "an empty temp file",
		"thermal_zone8":   "a regular file where a zone directory should be",
	}
	for name, why := range skipped {
		if seen[name] {
			t.Errorf("ReadZones() reported %s, which should be skipped: %s", name, why)
		}
	}
}

func TestReadZonesTemperature(t *testing.T) {
	tests := []struct {
		name string
		temp string
		want []ThermalZone
	}{
		{name: "millidegrees", temp: "42000\n", want: []ThermalZone{{Name: "thermal_zone0", Type: "z", TempC: 42}}},
		{name: "no trailing newline", temp: "42000", want: []ThermalZone{{Name: "thermal_zone0", Type: "z", TempC: 42}}},
		{name: "fractional", temp: "42123\n", want: []ThermalZone{{Name: "thermal_zone0", Type: "z", TempC: 42.123}}},
		{name: "zero", temp: "0\n", want: []ThermalZone{{Name: "thermal_zone0", Type: "z", TempC: 0}}},
		{name: "negative", temp: "-273150\n", want: []ThermalZone{{Name: "thermal_zone0", Type: "z", TempC: -273.15}}},
		{name: "plus sign", temp: "+42000\n", want: []ThermalZone{{Name: "thermal_zone0", Type: "z", TempC: 42}}},
		// int() reads positional underscores and every Unicode decimal digit.
		{name: "underscores", temp: "4_2000\n", want: []ThermalZone{{Name: "thermal_zone0", Type: "z", TempC: 42}}},
		{name: "unicode digits", temp: "٤٢٠٠٠\n", want: []ThermalZone{{Name: "thermal_zone0", Type: "z", TempC: 42}}},
		// str.strip()'s whitespace set, which reaches past unicode.IsSpace into
		// U+001C..U+001F. The trim runs before int(), whose own skip does not.
		{name: "surrounded by whitespace", temp: "  42000  \n", want: []ThermalZone{{Name: "thermal_zone0", Type: "z", TempC: 42}}},
		{name: "file separator", temp: "\x1c42000\x1f", want: []ThermalZone{{Name: "thermal_zone0", Type: "z", TempC: 42}}},
		// Everything int() refuses drops the zone rather than failing the walk.
		{name: "empty", temp: "", want: []ThermalZone{}},
		{name: "whitespace only", temp: "  \n", want: []ThermalZone{}},
		{name: "not a number", temp: "banana\n", want: []ThermalZone{}},
		{name: "float", temp: "42.5\n", want: []ThermalZone{}},
		{name: "two numbers", temp: "42000 43000\n", want: []ThermalZone{}},
		{name: "hex", temp: "0x2a\n", want: []ThermalZone{}},
		// A departure from v1, whose int is arbitrary-precision: past int64 the zone
		// is skipped rather than carried through as a very warm room.
		{name: "past int64", temp: "99999999999999999999\n", want: []ThermalZone{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := zoneFS(map[string]map[string]string{
				"thermal_zone0": {"temp": tc.temp, "type": "z"},
			})
			got, err := ReadZones(fsys, ".")
			if err != nil {
				t.Fatalf("ReadZones() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ReadZones() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestReadZonesEmptyAnswers covers the three roots v1 reaches by two different
// routes -- `if not root.exists(): return []` and a glob that matches nothing --
// and that this reaches by one. All three mean "this host has no thermal
// sensors", which is most VMs and every container.
func TestReadZonesEmptyAnswers(t *testing.T) {
	tests := []struct {
		name string
		fsys fs.FS
		dir  string
	}{
		{name: "root is missing", fsys: fstest.MapFS{}, dir: "class/thermal"},
		{
			name: "root is a file",
			fsys: fstest.MapFS{"class/thermal": &fstest.MapFile{Data: []byte("not a directory")}},
			dir:  "class/thermal",
		},
		{
			name: "nothing in it is a zone",
			fsys: zoneFS(map[string]map[string]string{"cooling_device0": {"cur_state": "0\n"}}),
			dir:  ".",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadZones(tc.fsys, tc.dir)
			if err != nil {
				t.Fatalf("ReadZones() error = %v", err)
			}
			if len(got) != 0 {
				t.Errorf("ReadZones() = %+v, want no zones", got)
			}
		})
	}
}

// TestReadZonesSurvivesAnUnreadableTemperature is the tolerance v1's try/except
// buys: one zone whose driver will not answer must not cost the zones around it.
func TestReadZonesSurvivesAnUnreadableTemperature(t *testing.T) {
	fsys := deniedFS{
		FS: zoneFS(map[string]map[string]string{
			"thermal_zone0": {"temp": "40000\n", "type": "a"},
			"thermal_zone1": {"temp": "50000\n", "type": "b"},
			"thermal_zone2": {"temp": "60000\n", "type": "c"},
		}),
		denied: "thermal_zone1/temp",
		err:    fs.ErrPermission,
	}
	got, err := ReadZones(fsys, ".")
	if err != nil {
		t.Fatalf("ReadZones() error = %v", err)
	}
	want := []ThermalZone{
		{Name: "thermal_zone0", Type: "a", TempC: 40},
		{Name: "thermal_zone2", Type: "c", TempC: 60},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ReadZones() mismatch (-want +got):\n%s", diff)
	}
}

// TestReadZonesFailsOnAnUnreadableType pins the asymmetry rather than tidying it:
// v1 wraps only the temperature read, so an existing type file that will not read
// propagates out and loses the zones already collected. Reproduced, because the
// alternative -- swallowing it -- reports a host with unreadable sysfs as a host
// with no sensors, and those are different problems.
func TestReadZonesFailsOnAnUnreadableType(t *testing.T) {
	fsys := deniedFS{
		FS: zoneFS(map[string]map[string]string{
			"thermal_zone0": {"temp": "40000\n", "type": "a"},
			"thermal_zone1": {"temp": "50000\n", "type": "b"},
		}),
		denied: "thermal_zone1/type",
		err:    fs.ErrPermission,
	}
	got, err := ReadZones(fsys, ".")
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("ReadZones() error = %v, want fs.ErrPermission", err)
	}
	if got != nil {
		t.Errorf("ReadZones() = %+v, want no partial result alongside the error", got)
	}
}

// TestReadZonesReadsAnAbsentTypeAsTheZoneName is the other half of the same read:
// missing is not a failure, because v1 asks exists() first and falls back to the
// directory name.
func TestReadZonesReadsAnAbsentTypeAsTheZoneName(t *testing.T) {
	fsys := zoneFS(map[string]map[string]string{"thermal_zone0": {"temp": "40000\n"}})
	got, err := ReadZones(fsys, ".")
	if err != nil {
		t.Fatalf("ReadZones() error = %v", err)
	}
	want := []ThermalZone{{Name: "thermal_zone0", Type: "thermal_zone0", TempC: 40}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ReadZones() mismatch (-want +got):\n%s", diff)
	}
}

// TestReadZonesReadsANestedRoot checks the dir argument is honoured rather than
// being a decoration on a walk that always starts at the root -- the shipping
// caller passes SysThermal, and every other test here passes ".".
func TestReadZonesReadsANestedRoot(t *testing.T) {
	fsys := fstest.MapFS{
		SysThermal + "/thermal_zone0/temp":       &fstest.MapFile{Data: []byte("40000\n")},
		SysThermal + "/thermal_zone0/type":       &fstest.MapFile{Data: []byte("acpitz\n")},
		"class/net/eth0/thermal_zone0/temp":      &fstest.MapFile{Data: []byte("99000\n")},
		"class/thermalzone/thermal_zone0/temp":   &fstest.MapFile{Data: []byte("98000\n")},
		"class/thermal/other/thermal_zone0/temp": &fstest.MapFile{Data: []byte("97000\n")},
	}
	got, err := ReadZones(fsys, SysThermal)
	if err != nil {
		t.Fatalf("ReadZones() error = %v", err)
	}
	// One level only: the glob's * does not cross a separator, so the zone nested
	// under class/thermal/other is not a zone.
	want := []ThermalZone{{Name: "thermal_zone0", Type: "acpitz", TempC: 40}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ReadZones() mismatch (-want +got):\n%s", diff)
	}
}

// TestReadZonesRejectsAGlobMetacharacterInTheRoot is the one failure that is the
// caller's mistake rather than the host's: dir is joined into a glob pattern, so a
// root holding an unclosed character class is not a directory that matched
// nothing, it is a pattern nobody can evaluate. Reported rather than silently
// answered with no zones, which would read as a host without sensors.
func TestReadZonesRejectsAGlobMetacharacterInTheRoot(t *testing.T) {
	got, err := ReadZones(fstest.MapFS{}, "class/[thermal")
	if !errors.Is(err, path.ErrBadPattern) {
		t.Fatalf("ReadZones() error = %v, want path.ErrBadPattern", err)
	}
	if got != nil {
		t.Errorf("ReadZones() = %+v, want nothing alongside the error", got)
	}
}

func TestHottestFixture(t *testing.T) {
	zones, err := ReadZones(fixture.Tree(t, "thermal/zones.json"), ".")
	if err != nil {
		t.Fatalf("ReadZones() error = %v", err)
	}
	got, ok := Hottest(zones)
	if !ok {
		t.Fatal("Hottest() reported no zones")
	}
	// thermal_zone9 reads the same 55.123, and thermal_zone0 wins because it came
	// first. Python's max() keeps its incumbent on a tie; a caller rendering the
	// winner's name should not see it flap between two identical sensors.
	want := ThermalZone{Name: "thermal_zone0", Type: "x86_pkg_temp", TempC: 55.123}
	if got != want {
		t.Errorf("Hottest() = %+v, want %+v", got, want)
	}
}

func TestHottest(t *testing.T) {
	tests := []struct {
		name  string
		zones []ThermalZone
		want  ThermalZone
		wonOK bool
	}{
		{name: "no zones", zones: nil, wonOK: false},
		{name: "no zones, empty slice", zones: []ThermalZone{}, wonOK: false},
		{
			name:  "one zone",
			zones: []ThermalZone{{Name: "a", TempC: 40}},
			want:  ThermalZone{Name: "a", TempC: 40},
			wonOK: true,
		},
		{
			name:  "warmest is last",
			zones: []ThermalZone{{Name: "a", TempC: 40}, {Name: "b", TempC: 90}},
			want:  ThermalZone{Name: "b", TempC: 90},
			wonOK: true,
		},
		{
			name:  "warmest is first",
			zones: []ThermalZone{{Name: "a", TempC: 90}, {Name: "b", TempC: 40}},
			want:  ThermalZone{Name: "a", TempC: 90},
			wonOK: true,
		},
		{
			name:  "tie keeps the incumbent",
			zones: []ThermalZone{{Name: "a", TempC: 50}, {Name: "b", TempC: 50}, {Name: "c", TempC: 50}},
			want:  ThermalZone{Name: "a", TempC: 50},
			wonOK: true,
		},
		{
			name:  "all below zero",
			zones: []ThermalZone{{Name: "a", TempC: -40}, {Name: "b", TempC: -10}},
			want:  ThermalZone{Name: "b", TempC: -10},
			wonOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Hottest(tc.zones)
			if ok != tc.wonOK {
				t.Fatalf("Hottest() ok = %v, want %v", ok, tc.wonOK)
			}
			if got != tc.want {
				t.Errorf("Hottest() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestThermalZoneWireShape(t *testing.T) {
	got, err := json.Marshal(ThermalZone{Name: "thermal_zone0", Type: "acpitz", TempC: 42.5})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `{"name":"thermal_zone0","type":"acpitz","temp_c":42.5}`
	if string(got) != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}
}
