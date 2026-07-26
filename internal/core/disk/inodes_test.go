package disk

import (
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/google/go-cmp/cmp"
)

// Pinned the same way as the ParseDF expectations: fixture rows come from
//
//	python tools/oracle/py_oracle.py --case df.inodes_mixed
//
// and the synthetic rows from feeding the literal to parse_df_inodes. Change one
// by re-running Python, not by adjusting it until Go passes.

func TestParseDFInodesFixtures(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    []InodeUsage
	}{
		{
			// Every skip path at once: header, udev/tmpfs/overlay, and /boot/efi's
			// zero inode count.
			name:    "mixed",
			fixture: "df/inodes_mixed.txt",
			want: []InodeUsage{
				{Filesystem: "/dev/sda1", Mount: "/", Total: 62500000, Used: 5800000, Free: 56700000, UsedPct: 10},
				{Filesystem: "/dev/sdb1", Mount: "/var", Total: 31250000, Used: 28000000, Free: 3250000, UsedPct: 90},
				{Filesystem: "/dev/sdc1", Mount: "/var/lib/docker", Total: 15625000, Used: 15000000, Free: 625000, UsedPct: 96},
			},
		},
		{
			// IUse% is '-', so the percentage is computed: 300000 * 100 / 1000000.
			name:    "missing iuse",
			fixture: "df/inodes_missing_iuse.txt",
			want: []InodeUsage{
				{Filesystem: "/dev/sde1", Mount: "/mnt/x", Total: 1000000, Used: 300000, Free: 700000, UsedPct: 30},
			},
		},
		{
			name:    "mount with spaces",
			fixture: "df/inodes_mount_with_spaces.txt",
			want: []InodeUsage{
				{Filesystem: "/dev/sdd1", Mount: "/mnt/with space", Total: 1000000, Used: 500000, Free: 500000, UsedPct: 50},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseDFInodes(fixture.Text(t, tt.fixture))
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseDFInodes(%s) mismatch (-want +got):\n%s", tt.fixture, diff)
			}
		})
	}
}

func TestParseDFInodesRows(t *testing.T) {
	row := func(mount string, pct int64) []InodeUsage {
		return []InodeUsage{{Filesystem: "/dev/sda1", Mount: mount, Total: 100, Used: 50, Free: 50, UsedPct: pct}}
	}
	tests := []struct {
		name string
		in   string
		want []InodeUsage
	}{
		{"empty input", "", []InodeUsage{}},
		{"header only", "Filesystem       Inodes   IUsed   IFree IUse% Mounted on\n", []InodeUsage{}},
		{"fewer than six fields", "/dev/sda1 100 10 90 10%\n", []InodeUsage{}},
		{"every count is a dash", "/dev/sda1 - - - - /\n", []InodeUsage{}},
		// btrfs and zfs allocate inodes dynamically and report zero. Dropped, or the
		// row would read as a filesystem that cannot create another file.
		{"zero inodes", "/dev/sda1 0 0 0 0% /\n", []InodeUsage{}},
		// Only IFree is unusable, and the row still goes: Python's int() raises on
		// the first of the three, so any one of them drops the line.
		{"free is a dash", "/dev/sda1 100 50 - 50% /\n", []InodeUsage{}},
		// rstrip("%") removes every trailing '%', not just one.
		{"doubled percent sign", "/dev/sda1 100 50 50 50%% /\n", row("/", 50)},
		// Python's int() accepts underscore digit separators; Go's strconv does not
		// unless the base is inferred, which is why pytext.Int exists.
		{"underscore separators", "/dev/sda1 1_00 50 50 50% /\n", row("/", 50)},
		{"blank and whitespace lines", "\n   \n/dev/sda1 100 50 50 50% /\n", row("/", 50)},
		// The skip list matches the whole line, so a mount named like a pseudo-fs is
		// unaffected -- it is the filesystem field that starts the line.
		{"mount named after a pseudo-fs", "/dev/sda1 100 50 50 50% /tmpfs-backup\n", row("/tmpfs-backup", 50)},
		// split(None, 5) loses the remainder's leading whitespace but keeps its
		// trailing whitespace, so the mount really does end in three spaces.
		{"mount keeps trailing space", "/dev/sda1 100 50 50 50% /mnt/x   \n", row("/mnt/x   ", 50)},
		// A space before the '%' shifts the columns: the percentage is "50" and the
		// mount becomes "% /". Both implementations agree, which is the point.
		{"space before the percent sign", "/dev/sda1 100 50 50 50 % /\n", row("% /", 50)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseDFInodes(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseDFInodes(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

func TestParseDFInodesReturnsEmptyNotNil(t *testing.T) {
	if got := ParseDFInodes(""); got == nil {
		t.Error("ParseDFInodes returned nil; the JSON would be null where Python's is []")
	}
}

// The invariants a row must satisfy whatever the input. A filesystem or mount
// that came back empty means a column was mis-assigned, and Total of zero means
// the dynamic-inode skip did not fire -- both would be reported as a full disk.
func FuzzParseDFInodes(f *testing.F) {
	for _, name := range []string{"inodes_mixed", "inodes_missing_iuse", "inodes_mount_with_spaces"} {
		f.Add(fixture.Text(f, "df/"+name+".txt"))
	}
	f.Fuzz(func(t *testing.T, in string) {
		for _, r := range ParseDFInodes(in) {
			if r.Filesystem == "" || r.Mount == "" {
				t.Errorf("row with an empty field from %q: %+v", in, r)
			}
			if r.Total == 0 {
				t.Errorf("zero-inode row survived from %q: %+v", in, r)
			}
		}
	})
}
