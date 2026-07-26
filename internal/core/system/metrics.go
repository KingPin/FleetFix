// Package system ports the host-level readers and parsers from
// src/fleetfix/modules/system: /proc/uptime, /proc/loadavg, /proc/meminfo, and
// the two apt update-count parsers.
package system

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/hostfs"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// Where the readers below look under a Host's Proc root.
const (
	ProcUptime  = "uptime"
	ProcLoadavg = "loadavg"
	ProcMeminfo = "meminfo"
)

// The two failure modes v1 raises out of these readers rather than absorbing.
//
// /proc/uptime and /proc/loadavg have no "no answer" shape: the kernel either
// wrote the file or the caller is not on Linux, and a zero uptime would be
// indistinguishable from a host that has just booted. So unlike the parsers,
// which return their zero value for unusable input, these report the error --
// and they report it in the same two flavours Python does, because the
// differential harness compares error *codes* and an IndexError that arrived as
// a ValueError is a real difference in what the reader concluded.
var (
	// ErrShortFile is v1's IndexError: the file has fewer whitespace-separated
	// fields than the reader indexes.
	ErrShortFile = errors.New("system: file has fewer fields than expected")

	// ErrBadNumber is v1's ValueError: a field is present but not a number.
	ErrBadNumber = errors.New("system: field is not a number")
)

// ReadUptime returns the host's uptime in seconds, the first field of
// /proc/uptime. The second field (idle time summed across CPUs) is ignored, as
// it is in v1.
func ReadUptime(fsys fs.FS, name string) (float64, error) {
	text, err := hostfs.ReadFile(fsys, name)
	if err != nil {
		return 0, err
	}
	fields := pytext.Fields(text)
	if len(fields) == 0 {
		return 0, fmt.Errorf("%w: %s has no fields", ErrShortFile, name)
	}
	seconds, err := pytext.Float(fields[0])
	if err != nil {
		return 0, fmt.Errorf("%w: %s starts with %q", ErrBadNumber, name, fields[0])
	}
	return seconds, nil
}

// LoadAverage is the 1/5/15-minute run-queue average from /proc/loadavg.
type LoadAverage struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

// ReadLoadavg returns the three load figures from /proc/loadavg. The running/total
// task counts and the last-created PID that follow them are ignored.
//
// Fields are indexed and converted one at a time, in order, rather than being
// bounds-checked up front. That is not incidental: v1 builds the dataclass from
// three positional arguments, Python evaluates them left to right, and so
// "banana" alone is a ValueError -- the conversion of field 0 happens before
// field 1 is ever indexed -- while "0.12 0.34" is an IndexError. Checking the
// length first would turn the first of those into ErrShortFile.
func ReadLoadavg(fsys fs.FS, name string) (LoadAverage, error) {
	text, err := hostfs.ReadFile(fsys, name)
	if err != nil {
		return LoadAverage{}, err
	}
	fields := pytext.Fields(text)

	var out LoadAverage
	for i, dst := range []*float64{&out.One, &out.Five, &out.Fifteen} {
		if i >= len(fields) {
			return LoadAverage{}, fmt.Errorf("%w: %s has %d fields, want at least 3", ErrShortFile, name, len(fields))
		}
		v, err := pytext.Float(fields[i])
		if err != nil {
			return LoadAverage{}, fmt.Errorf("%w: %s field %d is %q", ErrBadNumber, name, i, fields[i])
		}
		*dst = v
	}
	return out, nil
}

// MemoryInfo is the subset of /proc/meminfo the dashboard and the memory check
// read. Every figure is in kibibytes, the unit /proc/meminfo reports.
type MemoryInfo struct {
	TotalKB     int64 `json:"total_kb"`
	AvailableKB int64 `json:"available_kb"`
	UsedKB      int64 `json:"used_kb"`
	SwapTotalKB int64 `json:"swap_total_kb"`
	SwapUsedKB  int64 `json:"swap_used_kb"`
}

// UsedPct is memory in use as a percentage of total, and 0 on a host that
// reported no total rather than a division by zero.
func (m MemoryInfo) UsedPct() float64 {
	if m.TotalKB == 0 {
		return 0
	}
	return float64(m.UsedKB) / float64(m.TotalKB) * 100
}

// SwapUsedPct is swap in use as a percentage of swap total, and 0 on a host with
// no swap configured.
func (m MemoryInfo) SwapUsedPct() float64 {
	if m.SwapTotalKB == 0 {
		return 0
	}
	return float64(m.SwapUsedKB) / float64(m.SwapTotalKB) * 100
}

// ReadMeminfo parses /proc/meminfo. Unlike the two readers above it has a
// meaningful zero -- an empty or unparseable file is a MemoryInfo of all zeros,
// which is what v1 returns too.
//
// It can still fail, and on input a guard appears to rule out: a value of "²"
// satisfies str.isdigit() and is then rejected by int(). v1 raises ValueError
// there, so this returns ErrBadNumber. Not reachable from a kernel-written
// /proc/meminfo, but the guard is reproduced rather than tightened because the
// harness compares what the reader concluded, not what a real file contains.
//
// One departure: a value past int64 skips that field, where Python keeps the
// arbitrary-precision integer. MemTotal on a host with more than 8 ZiB of RAM.
func ReadMeminfo(fsys fs.FS, name string) (MemoryInfo, error) {
	text, err := hostfs.ReadFile(fsys, name)
	if err != nil {
		return MemoryInfo{}, err
	}

	fields := map[string]int64{}
	for _, line := range pytext.SplitLines(text) {
		// str.partition, so a line with no colon and a line ending in one are the
		// same non-answer: an empty remainder either way.
		key, rest, _ := strings.Cut(line, ":")
		if rest == "" {
			continue
		}
		value := pytext.Fields(rest)
		if len(value) == 0 || !pytext.IsDigitString(value[0]) {
			continue
		}
		n, err := pytext.Int(value[0])
		if err != nil {
			if errors.Is(err, pytext.ErrRange) {
				continue
			}
			return MemoryInfo{}, fmt.Errorf("%w: %s reports %s as %q", ErrBadNumber, name, key, value[0])
		}
		fields[strings.TrimFunc(key, pytext.IsSpace)] = n
	}

	// MemAvailable is the kernel's own estimate and is what a modern host should
	// be graded on; MemFree is the pre-3.14-kernel fallback and reads far lower, as it
	// excludes reclaimable page cache. Neither present means available stays 0 and
	// used comes out equal to total, which is at least the pessimistic direction.
	available, ok := fields["MemAvailable"]
	if !ok {
		available = fields["MemFree"]
	}
	total := fields["MemTotal"]
	swapTotal := fields["SwapTotal"]

	return MemoryInfo{
		TotalKB:     total,
		AvailableKB: available,
		UsedKB:      max(total-available, 0),
		SwapTotalKB: swapTotal,
		SwapUsedKB:  max(swapTotal-fields["SwapFree"], 0),
	}, nil
}
