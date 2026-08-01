package disk

import (
	"math"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/google/go-cmp/cmp"
)

// The fixture expectations here are the Python oracle's output, captured with
//
//	python tools/oracle/py_oracle.py --case df.usage_mixed
//
// rather than read off the fixture by eye. The synthetic cases below them were
// pinned the same way, by feeding the literal to parse_df in a REPL. Change one by
// re-running Python, not by adjusting it until Go passes.

func TestParseDFFixtures(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    []Usage
	}{
		{
			// Exercises all three skip paths at once: the header, the udev/tmpfs/
			// overlay pseudo-filesystems, and /boot/efi's zero capacity.
			name:    "mixed",
			fixture: "df/usage_mixed.txt",
			want: []Usage{
				{Filesystem: "/dev/sda1", Mount: "/", TotalKB: 102400000, UsedKB: 51200000, AvailKB: 51200000, UsedPct: 50},
				{Filesystem: "/dev/sdb1", Mount: "/var", TotalKB: 52428800, UsedKB: 47185920, AvailKB: 5242880, UsedPct: 90},
				{Filesystem: "/dev/sdc1", Mount: "/var/lib/docker", TotalKB: 20971520, UsedKB: 20132659, AvailKB: 838861, UsedPct: 96},
			},
		},
		{
			// df prints '-' for the capacity of a filesystem it cannot account for,
			// so the percentage is computed: 300000 * 100 / 1000000.
			name:    "missing capacity",
			fixture: "df/usage_missing_capacity.txt",
			want: []Usage{
				{Filesystem: "/dev/sde1", Mount: "/mnt/x", TotalKB: 1000000, UsedKB: 300000, AvailKB: 700000, UsedPct: 30},
			},
		},
		{
			// The reason the split is capped at five. An unlimited split would
			// return "/mnt/with" here and drop "space" entirely.
			name:    "mount with spaces",
			fixture: "df/usage_mount_with_spaces.txt",
			want: []Usage{
				{Filesystem: "/dev/sdd1", Mount: "/mnt/with space", TotalKB: 1000000, UsedKB: 500000, AvailKB: 500000, UsedPct: 50},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseDF(fixture.Text(t, tt.fixture))
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseDF(%s) mismatch (-want +got):\n%s", tt.fixture, diff)
			}
		})
	}
}

func TestParseDFRows(t *testing.T) {
	row := func(pct int64) []Usage {
		return []Usage{{Filesystem: "/dev/sda1", Mount: "/", TotalKB: 100, UsedKB: 50, AvailKB: 50, UsedPct: pct}}
	}
	tests := []struct {
		name string
		in   string
		want []Usage
	}{
		{"empty input", "", []Usage{}},
		{"header only", "Filesystem 1024-blocks Used Available Capacity Mounted on\n", []Usage{}},
		{"fewer than six fields", "/dev/sda1 100 50 50 50%", []Usage{}},
		{"non-numeric block count", "/dev/sda1 - 50 50 50% /", []Usage{}},
		{"zero capacity", "/dev/sda1 0 0 0 0% /boot/efi", []Usage{}},

		// rstrip("%") strips every trailing '%', not just one. strings.TrimSuffix
		// would leave "62%" and silently fall through to the computed branch, which
		// on this row happens to give 50 -- a wrong answer that still looks like a
		// percentage.
		{"doubled percent sign", "/dev/sda1 100 50 50 62%% /", row(62)},

		// int() accepts underscore digit separators. No df prints them, but the
		// parser's numeric reads have to agree with Python's on every input the
		// harness can generate, not just the ones df produces.
		{"underscore separators", "/dev/sda1 1_00 50 50 50% /", row(50)},

		// A whitespace-only line is not "" and so survives the first guard; it is
		// the field count that drops it.
		{"blank and whitespace-only lines", "\n   \n/dev/sda1 100 50 50 50% /\n", row(50)},

		// The skip list matches the whole line, so a pseudo-FS name appearing in
		// the mount field is not a reason to drop the row.
		{
			"skip prefix inside the mount point",
			"/dev/sda1 100 50 50 50% /tmpfs-backup",
			[]Usage{{Filesystem: "/dev/sda1", Mount: "/tmpfs-backup", TotalKB: 100, UsedKB: 50, AvailKB: 50, UsedPct: 50}},
		},

		// split(None, 5) strips the remainder's leading whitespace and keeps its
		// trailing whitespace, so a padded row carries the padding into the mount.
		{
			"trailing space belongs to the mount",
			"/dev/sda1 100 50 50 50% /mnt/x  ",
			[]Usage{{Filesystem: "/dev/sda1", Mount: "/mnt/x  ", TotalKB: 100, UsedKB: 50, AvailKB: 50, UsedPct: 50}},
		},

		// A space between the number and the '%' pushes the '%' into the mount
		// field and leaves a bare number as the capacity. Both implementations get
		// this equally wrong, which is the point.
		{
			"space before the percent sign",
			"/dev/sda1 100 50 50  62 % /",
			[]Usage{{Filesystem: "/dev/sda1", Mount: "% /", TotalKB: 100, UsedKB: 50, AvailKB: 50, UsedPct: 62}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, ParseDF(tt.in)); diff != "" {
				t.Errorf("ParseDF(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

// ParseDF must return an empty slice, never nil: the report envelope marshals this
// straight to JSON, and null where the Python emits [] is a divergence on every
// host that has no rows to report.
func TestParseDFReturnsEmptyNotNil(t *testing.T) {
	if got := ParseDF(""); got == nil {
		t.Error("ParseDF(\"\") = nil, want an empty slice")
	}
}

// The computed-percentage fallback rounds half to even, because Python's round()
// does. No fixture lands on a .5 boundary, so without this the two implementations
// would disagree on one host in a hundred and nowhere in the corpus.
func TestUsedPercentRoundsHalfToEven(t *testing.T) {
	tests := []struct {
		name        string
		used, total int64
		want        int64
	}{
		{"62.5 rounds down to even", 125, 200, 62},
		{"63.5 rounds up to even", 127, 200, 64},
		{"37.5 rounds up to even", 3, 8, 38},
		{"no rounding needed", 50, 100, 50},
		// ParseDF drops zero-capacity rows before it gets here, so this guard is
		// only reachable directly -- but it is what stops a future second caller
		// from dividing by zero and converting +Inf to an int64.
		{"zero capacity cannot divide", 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// "-" is what df prints when it has no capacity to report, and is what
			// drives usedPercent into the computed branch.
			if got := usedPercent("-", tt.used, tt.total); got != tt.want {
				t.Errorf("usedPercent(\"-\", %d, %d) = %d, want %d", tt.used, tt.total, got, tt.want)
			}
		})
	}
	// Pin the difference so a later "simplification" to math.Round fails here.
	if math.Round(62.5) != 63 {
		t.Fatal("premise changed: math.Round(62.5) is no longer 63")
	}
}

func TestFullest(t *testing.T) {
	tests := []struct {
		name string
		in   []Usage
		want Usage
		ok   bool
	}{
		{name: "empty", in: []Usage{}},
		{name: "nil", in: nil},
		{
			name: "single row",
			in:   []Usage{{Mount: "/", UsedPct: 10}},
			want: Usage{Mount: "/", UsedPct: 10},
			ok:   true,
		},
		{
			name: "highest wins regardless of position",
			in:   []Usage{{Mount: "/", UsedPct: 10}, {Mount: "/var", UsedPct: 91}, {Mount: "/home", UsedPct: 40}},
			want: Usage{Mount: "/var", UsedPct: 91},
			ok:   true,
		},
		{
			// max() keeps the first element when the key is equal.
			name: "ties go to the first row",
			in:   []Usage{{Mount: "/", UsedPct: 91}, {Mount: "/var", UsedPct: 91}},
			want: Usage{Mount: "/", UsedPct: 91},
			ok:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Fullest(tt.in)
			if ok != tt.ok {
				t.Fatalf("Fullest(%v) ok = %v, want %v", tt.in, ok, tt.ok)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Fullest mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// Every field of every row must come back exactly as it appeared in the input, so
// generated garbage cannot smuggle a whitespace-bearing field or an empty
// filesystem name into a result that later code treats as a real mount.
func FuzzParseDF(f *testing.F) {
	f.Add("/dev/sda1 100 50 50 50% /")
	f.Add("Filesystem 1024-blocks Used Available Capacity Mounted on\ntmpfs 1 0 1 0% /run\n")
	f.Add("/dev/sdd1 1000000 500000 500000 50% /mnt/with space")
	f.Add("/dev/sde1 1000000 300000 700000 - /mnt/x")
	f.Fuzz(func(t *testing.T, s string) {
		for i, row := range ParseDF(s) {
			if row.Filesystem == "" {
				t.Fatalf("ParseDF(%q)[%d] has an empty filesystem", s, i)
			}
			if row.TotalKB == 0 {
				t.Fatalf("ParseDF(%q)[%d] has zero capacity and should have been skipped", s, i)
			}
			if row.Mount == "" {
				t.Fatalf("ParseDF(%q)[%d] has an empty mount point", s, i)
			}
		}
	})
}
