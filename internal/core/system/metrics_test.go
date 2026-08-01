package system

import (
	"errors"
	"io/fs"
	"math"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

// Expectations measured against v1's src/fleetfix/modules/system/metrics.py.

const procFile = "metrics"

// procFS is the one-file /proc these readers are pointed at. fstest.MapFS is the
// whole fixture: no temp directory, and no /proc on the CI host to depend on.
func procFS(text string) fs.FS {
	return fstest.MapFS{procFile: &fstest.MapFile{Data: []byte(text)}}
}

// emptyFS has no files at all, so any read of procFile is fs.ErrNotExist.
func emptyFS() fs.FS { return fstest.MapFS{} }

func TestReadUptimeFixture(t *testing.T) {
	got, err := ReadUptime(procFS(fixture.Text(t, "proc/uptime.txt")), procFile)
	if err != nil {
		t.Fatalf("ReadUptime() error = %v", err)
	}
	if want := 12345.67; got != want {
		t.Errorf("ReadUptime() = %v, want %v", got, want)
	}
}

func TestReadUptimeFields(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    float64
		wantErr error
	}{
		{name: "normal", text: "12345.67 98765.43\n", want: 12345.67},
		{name: "no trailing newline", text: "12345.67 98765.43", want: 12345.67},
		{name: "one field", text: "12345.67", want: 12345.67},
		{name: "integer", text: "42 1", want: 42},
		{name: "negative", text: "-42 1", want: -42},
		{name: "leading whitespace", text: "   12345.67 1", want: 12345.67},
		{name: "exponent", text: "1e3 1", want: 1000},
		{name: "plus sign", text: "+42 1", want: 42},
		{name: "trailing dot", text: "42. 1", want: 42},
		{name: "leading dot", text: ".5 1", want: 0.5},
		// float() permits underscores between digits and reads non-ASCII decimal
		// digits, both of which int() does too.
		{name: "underscores", text: "1_000.5 1", want: 1000.5},
		{name: "unicode digits", text: "٤٢ 1", want: 42},
		// The field split is str.split(), whose whitespace set reaches past
		// unicode.IsSpace at both ends: the C1 file separators and NBSP.
		{name: "exotic boundary as separator", text: "42\u001c1", want: 42},
		{name: "no-break space separator", text: "42\u00a01", want: 42},

		{name: "empty", text: "", wantErr: ErrShortFile},
		{name: "whitespace only", text: "   \n", wantErr: ErrShortFile},
		{name: "not a number", text: "banana 1", wantErr: ErrBadNumber},
		// Go's ParseFloat reads a hex float. Python's float() does not, and
		// pytext.Float follows Python.
		{name: "hex float", text: "0x1p3 1", wantErr: ErrBadNumber},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadUptime(procFS(tt.text), procFile)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ReadUptime(%q) error = %v, want %v", tt.text, err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if got != tt.want {
				t.Errorf("ReadUptime(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestReadUptimeNonFiniteFields(t *testing.T) {
	// float() accepts these and so does the port; they are separated from the
	// table above only because NaN is not comparable with ==.
	tests := []struct {
		name string
		text string
		inf  int // +1, -1, or 0 for NaN
	}{
		{name: "inf", text: "inf 1", inf: 1},
		{name: "negative infinity", text: "-Infinity 1", inf: -1},
		// Overflow is not an error to float(), which saturates to infinity. A
		// reader that treated Go's ErrRange as unparseable would report ValueError.
		{name: "overflow saturates", text: strings.Repeat("9", 400) + " 1", inf: 1},
		{name: "nan", text: "nan 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadUptime(procFS(tt.text), procFile)
			if err != nil {
				t.Fatalf("ReadUptime(%q) error = %v", tt.text, err)
			}
			if tt.inf == 0 {
				if !math.IsNaN(got) {
					t.Errorf("ReadUptime(%q) = %v, want NaN", tt.text, got)
				}
				return
			}
			if !math.IsInf(got, tt.inf) {
				t.Errorf("ReadUptime(%q) = %v, want %+d infinity", tt.text, got, tt.inf)
			}
		})
	}
}

func TestReadUptimeOnAnUnreadableFile(t *testing.T) {
	// Neither ErrShortFile nor ErrBadNumber: v1 lets the OSError out of
	// read_text() untouched, and so does this.
	_, err := ReadUptime(emptyFS(), procFile)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadUptime() error = %v, want fs.ErrNotExist", err)
	}
	if errors.Is(err, ErrShortFile) || errors.Is(err, ErrBadNumber) {
		t.Errorf("ReadUptime() reported a read failure as a parse failure: %v", err)
	}
}

func TestReadLoadavgFixture(t *testing.T) {
	got, err := ReadLoadavg(procFS(fixture.Text(t, "proc/loadavg.txt")), procFile)
	if err != nil {
		t.Fatalf("ReadLoadavg() error = %v", err)
	}
	want := LoadAverage{One: 0.12, Five: 0.34, Fifteen: 0.56}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ReadLoadavg() mismatch (-want +got):\n%s", diff)
	}
}

func TestReadLoadavgFields(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    LoadAverage
		wantErr error
	}{
		{
			name: "normal", text: "0.12 0.34 0.56 1/123 4567\n",
			want: LoadAverage{One: 0.12, Five: 0.34, Fifteen: 0.56},
		},
		{
			name: "three fields only", text: "0.12 0.34 0.56",
			want: LoadAverage{One: 0.12, Five: 0.34, Fifteen: 0.56},
		},
		{name: "integers", text: "1 2 3 1/1 1", want: LoadAverage{One: 1, Five: 2, Fifteen: 3}},
		{
			name: "negative", text: "-0.12 0.34 0.56",
			want: LoadAverage{One: -0.12, Five: 0.34, Fifteen: 0.56},
		},
		{
			name: "unicode digits", text: "٠.١٢ 0.34 0.56",
			want: LoadAverage{One: 0.12, Five: 0.34, Fifteen: 0.56},
		},
		{
			name: "no-break space separated", text: "0.12\u00a00.34\u00a00.56",
			want: LoadAverage{One: 0.12, Five: 0.34, Fifteen: 0.56},
		},

		{name: "empty", text: "", wantErr: ErrShortFile},
		{name: "one field only", text: "0.12", wantErr: ErrShortFile},
		{name: "two fields", text: "0.12 0.34", wantErr: ErrShortFile},
		{name: "third field bad", text: "0.12 0.34 banana", wantErr: ErrBadNumber},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadLoadavg(procFS(tt.text), procFile)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ReadLoadavg(%q) error = %v, want %v", tt.text, err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ReadLoadavg(%q) mismatch (-want +got):\n%s", tt.text, diff)
			}
		})
	}
}

func TestReadLoadavgConvertsBeforeItIndexesTheNextField(t *testing.T) {
	// The distinction a bounds check up front would erase. v1 evaluates the three
	// constructor arguments left to right, so a single unparseable field is a
	// ValueError even though the file is also too short to satisfy the reader --
	// float(parts[0]) raises before parts[1] is reached.
	tests := []struct {
		name    string
		text    string
		wantErr error
	}{
		{name: "one bad field", text: "banana", wantErr: ErrBadNumber},
		{name: "two fields, second bad", text: "0.12 banana", wantErr: ErrBadNumber},
		{name: "one good field", text: "0.12", wantErr: ErrShortFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReadLoadavg(procFS(tt.text), procFile)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ReadLoadavg(%q) error = %v, want %v", tt.text, err, tt.wantErr)
			}
		})
	}
}

func TestReadLoadavgOnAnUnreadableFile(t *testing.T) {
	_, err := ReadLoadavg(emptyFS(), procFile)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadLoadavg() error = %v, want fs.ErrNotExist", err)
	}
}

func TestReadMeminfoFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		want MemoryInfo
	}{
		{
			name: "a full meminfo", file: "proc/meminfo/full.txt",
			want: MemoryInfo{
				TotalKB: 16384000, AvailableKB: 8192000, UsedKB: 8192000,
				SwapTotalKB: 4194304, SwapUsedKB: 1048576,
			},
		},
		{
			name: "without MemAvailable", file: "proc/meminfo/no_memavailable.txt",
			want: MemoryInfo{TotalKB: 1000, AvailableKB: 250, UsedKB: 750},
		},
		{
			name: "without swap", file: "proc/meminfo/no_swap.txt",
			want: MemoryInfo{TotalKB: 1000, AvailableKB: 500, UsedKB: 500},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadMeminfo(procFS(fixture.Text(t, tt.file)), procFile)
			if err != nil {
				t.Fatalf("ReadMeminfo() error = %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ReadMeminfo() mismatch (-want +got):\n%s", diff)
			}
			checkMemory(t, got)
		})
	}
}

func TestReadMeminfoLines(t *testing.T) {
	tests := []struct {
		name string
		text string
		want MemoryInfo
	}{
		{
			name: "normal",
			text: "MemTotal:       16384 kB\nMemAvailable:    8192 kB\n" +
				"SwapTotal:       2048 kB\nSwapFree:        1024 kB\n",
			want: MemoryInfo{
				TotalKB: 16384, AvailableKB: 8192, UsedKB: 8192,
				SwapTotalKB: 2048, SwapUsedKB: 1024,
			},
		},
		// An empty file is all zeros rather than an error: unlike uptime and
		// loadavg, every field here has a fallback.
		{name: "empty"},
		{
			name: "MemAvailable absent falls back to MemFree",
			text: "MemTotal: 100 kB\nMemFree: 40 kB\n",
			want: MemoryInfo{TotalKB: 100, AvailableKB: 40, UsedKB: 60},
		},
		{
			name: "MemAvailable wins over MemFree",
			text: "MemTotal: 100 kB\nMemFree: 40 kB\nMemAvailable: 60 kB\n",
			want: MemoryInfo{TotalKB: 100, AvailableKB: 60, UsedKB: 40},
		},
		{
			name: "neither available nor free", text: "MemTotal: 100 kB\n",
			want: MemoryInfo{TotalKB: 100, UsedKB: 100},
		},
		{
			name: "available above total clamps used to zero",
			text: "MemTotal: 100 kB\nMemAvailable: 300 kB\n",
			want: MemoryInfo{TotalKB: 100, AvailableKB: 300},
		},
		{
			name: "swap free above swap total clamps used to zero",
			text: "SwapTotal: 100 kB\nSwapFree: 300 kB\n",
			want: MemoryInfo{SwapTotalKB: 100},
		},
		{
			name: "the key is stripped", text: "  MemTotal  : 100 kB\n",
			want: MemoryInfo{TotalKB: 100, UsedKB: 100},
		},
		{
			name: "no unit", text: "MemTotal: 100\n",
			want: MemoryInfo{TotalKB: 100, UsedKB: 100},
		},
		{
			name: "a duplicate key, last wins",
			text: "MemTotal: 100 kB\nMemTotal: 200 kB\n",
			want: MemoryInfo{TotalKB: 200, UsedKB: 200},
		},
		{
			name: "keys nothing reads are ignored",
			text: "Hugepagesize: 2048 kB\nMemTotal: 100 kB\n",
			want: MemoryInfo{TotalKB: 100, UsedKB: 100},
		},
		{
			name: "non-ASCII decimal digits", text: "MemTotal: ٤٢ kB\n",
			want: MemoryInfo{TotalKB: 42, UsedKB: 42},
		},
		// splitlines(), so a form feed ends a line where a byte-wise scan for \n
		// would run the two rows together and lose both.
		{
			name: "exotic line boundary", text: "MemTotal: 100 kB\u000cMemFree: 40 kB",
			want: MemoryInfo{TotalKB: 100, AvailableKB: 40, UsedKB: 60},
		},

		// Skipped lines. Each of these leaves the field at its zero, which for
		// MemTotal is indistinguishable from a host that reported nothing.
		{name: "no colon", text: "MemTotal 100 kB\n"},
		{name: "a colon with nothing after it", text: "MemTotal:\n"},
		{name: "a colon then whitespace", text: "MemTotal:   \n"},
		{name: "the value is a word", text: "MemTotal: banana kB\n"},
		// isdigit() is false for all of these, so int() is never reached even
		// though it would accept the last three.
		{name: "a negative value", text: "MemTotal: -100 kB\n"},
		{name: "a signed value", text: "MemTotal: +100 kB\n"},
		{name: "underscores in the value", text: "MemTotal: 1_000 kB\n"},
		{name: "a second colon inside the value", text: "MemTotal: 100:200 kB\n"},
		{name: "keys are case-sensitive", text: "memtotal: 100 kB\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadMeminfo(procFS(tt.text), procFile)
			if err != nil {
				t.Fatalf("ReadMeminfo(%q) error = %v", tt.text, err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ReadMeminfo(%q) mismatch (-want +got):\n%s", tt.text, diff)
			}
			checkMemory(t, got)
		})
	}
}

func TestReadMeminfoRejectsADigitIntCannotRead(t *testing.T) {
	// The isdigit-then-int gap, reproduced. Both of these satisfy str.isdigit()
	// and are then rejected by int(), so v1 raises ValueError out of a function
	// that otherwise never fails on content.
	for _, text := range []string{"MemTotal: ² kB\n", "MemTotal: ② kB\n"} {
		_, err := ReadMeminfo(procFS(text), procFile)
		if !errors.Is(err, ErrBadNumber) {
			t.Errorf("ReadMeminfo(%q) error = %v, want ErrBadNumber", text, err)
		}
	}
}

func TestReadMeminfoHugeValueSkipsTheField(t *testing.T) {
	// The departure. Python keeps the arbitrary-precision integer; int64 cannot,
	// so the field is skipped and reads as absent. Not reachable from a
	// kernel-written /proc/meminfo -- MemTotal is a u64 count of 4 KiB pages.
	got, err := ReadMeminfo(procFS("MemTotal: "+strings.Repeat("9", 30)+" kB\nMemFree: 40 kB\n"), procFile)
	if err != nil {
		t.Fatalf("ReadMeminfo() error = %v", err)
	}
	want := MemoryInfo{AvailableKB: 40}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ReadMeminfo() mismatch (-want +got):\n%s", diff)
	}
}

func TestReadMeminfoOnAnUnreadableFile(t *testing.T) {
	_, err := ReadMeminfo(emptyFS(), procFile)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadMeminfo() error = %v, want fs.ErrNotExist", err)
	}
}

func TestMemoryInfoPercentages(t *testing.T) {
	tests := []struct {
		name     string
		in       MemoryInfo
		used     float64
		swapUsed float64
	}{
		{
			name: "half of each",
			in:   MemoryInfo{TotalKB: 100, UsedKB: 50, SwapTotalKB: 200, SwapUsedKB: 100},
			used: 50, swapUsed: 50,
		},
		// No total is 0%, not a division by zero: a host with no swap is the
		// common case, and a host reporting no memory at all is a file we failed
		// to read rather than a machine at 100%.
		{name: "no totals", in: MemoryInfo{}},
		{name: "no swap", in: MemoryInfo{TotalKB: 100, UsedKB: 25}, used: 25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.UsedPct(); got != tt.used {
				t.Errorf("UsedPct() = %v, want %v", got, tt.used)
			}
			if got := tt.in.SwapUsedPct(); got != tt.swapUsed {
				t.Errorf("SwapUsedPct() = %v, want %v", got, tt.swapUsed)
			}
		})
	}
}

// checkMemory asserts what holds for every MemoryInfo, whatever produced it.
func checkMemory(t *testing.T, got MemoryInfo) {
	t.Helper()
	if got.UsedKB < 0 || got.SwapUsedKB < 0 {
		t.Errorf("ReadMeminfo() reported negative usage: %+v", got)
	}
	if got.UsedKB > got.TotalKB {
		t.Errorf("ReadMeminfo() reported %d KB used of %d KB total", got.UsedKB, got.TotalKB)
	}
	if got.SwapUsedKB > got.SwapTotalKB {
		t.Errorf("ReadMeminfo() reported %d KB swap used of %d KB total", got.SwapUsedKB, got.SwapTotalKB)
	}
	if pct := got.UsedPct(); pct < 0 || pct > 100 {
		t.Errorf("UsedPct() = %v, want a percentage", pct)
	}
	if pct := got.SwapUsedPct(); pct < 0 || pct > 100 {
		t.Errorf("SwapUsedPct() = %v, want a percentage", pct)
	}
}

func FuzzReadUptime(f *testing.F) {
	for _, s := range []string{"12345.67 98765.43\n", "", "banana", "1e3 1", "inf"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		got, err := ReadUptime(procFS(text), procFile)
		if err == nil {
			return
		}
		if got != 0 {
			t.Fatalf("ReadUptime(%q) = %v with error %v, want the zero value", text, got, err)
		}
		if !errors.Is(err, ErrShortFile) && !errors.Is(err, ErrBadNumber) {
			t.Fatalf("ReadUptime(%q) error = %v, want one of the two parse sentinels", text, err)
		}
	})
}

func FuzzReadLoadavg(f *testing.F) {
	for _, s := range []string{"0.12 0.34 0.56 1/123 4567\n", "", "0.12", "banana"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		got, err := ReadLoadavg(procFS(text), procFile)
		if err == nil {
			return
		}
		if got != (LoadAverage{}) {
			t.Fatalf("ReadLoadavg(%q) = %+v with error %v, want the zero value", text, got, err)
		}
		if !errors.Is(err, ErrShortFile) && !errors.Is(err, ErrBadNumber) {
			t.Fatalf("ReadLoadavg(%q) error = %v, want one of the two parse sentinels", text, err)
		}
	})
}

func FuzzReadMeminfo(f *testing.F) {
	for _, s := range []string{
		"MemTotal: 100 kB\nMemAvailable: 40 kB\n", "", "MemTotal:\n", "MemTotal: ² kB\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		got, err := ReadMeminfo(procFS(text), procFile)
		if err != nil {
			if !errors.Is(err, ErrBadNumber) {
				t.Fatalf("ReadMeminfo(%q) error = %v, want ErrBadNumber", text, err)
			}
			return
		}
		checkMemory(t, got)
	})
}
