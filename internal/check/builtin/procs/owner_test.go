package procs

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// liveProc stages a process on the real filesystem rather than in a MapFS, which
// is what the owner lookup needs: fs.FileInfo has no way to report a uid, so
// lookupOwner stats a path and there has to be one.
func liveProc(t *testing.T, p proc) string {
	t.Helper()
	dir := t.TempDir()
	pidDir := filepath.Join(dir, strconv.FormatInt(p.pid, 10))
	if err := os.Mkdir(pidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(pidDir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("stat", p.statLine())
	write("statm", "0 "+strconv.FormatInt(p.pages, 10)+" 0 0 0 0 0\n")
	write("cmdline", p.cmdline)
	write(statusFile, "Name:\t"+p.comm+"\n")
	return dir
}

// liveSource reads a real directory, with the two seams the owner lookup needs
// left at their shipped values: no FS, so the walk goes through os.DirFS, and no
// Owner, so the uid comes off the filesystem.
func liveSource(dir string) Source {
	return Source{
		Dir:            dir,
		PageSize:       pageSize,
		ClockTicks:     100,
		CPUs:           func() int { return 1 },
		SampleInterval: time.Millisecond,
		Now:            steadyClock(time.Second),
	}
}

// The shipped path, end to end: no FS and no Owner, so the walk reads a real
// directory and the row's user comes out of this host's passwd database.
func TestTheLiveLookupNamesWhoeverOwnsTheProcess(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skipf("no current user on this host: %v", err)
	}
	dir := liveProc(t, proc{pid: 1, comm: "systemd", ticks: 10, pages: 100, cmdline: "/sbin/init"})

	rep := report(t, mustRun(t, liveSource(dir), nil))
	if rep.Total != 1 {
		t.Fatalf("total = %d, want the staged process", rep.Total)
	}
	got := rep.ByRSS[0].User
	if got == nil {
		t.Fatalf("user is nil, want %q: the test wrote the file, so it has an owner", me.Username)
	}
	if *got != me.Username {
		t.Errorf("user = %q, want %q", *got, me.Username)
	}
}

// A uid with no passwd entry is the normal state of affairs inside a container,
// and the row is still worth having with no name on it.
func TestAUIDWithNoPasswdEntryHasNoName(t *testing.T) {
	const orphan = 4294967000
	if _, err := user.LookupId(strconv.FormatUint(orphan, 10)); err == nil {
		t.Skipf("uid %d exists on this host", orphan)
	}
	if got := userName(orphan); got != nil {
		t.Errorf("userName(%d) = %q, want no name", orphan, *got)
	}
	// Again, off the cache this time: the absence is what was stored, not a miss
	// that gets re-asked once per process on a host running two thousand of them.
	if got := userName(orphan); got != nil {
		t.Errorf("cached userName(%d) = %q, want no name", orphan, *got)
	}
}

// A pid that is not there when the lookup runs is routine rather than a failure,
// and there is nothing to report about it either way.
func TestAnAbsentProcessHasNoOwner(t *testing.T) {
	if got := lookupOwner(t.TempDir(), 424242); got != nil {
		t.Errorf("lookupOwner = %q, want nil for a pid that is not there", *got)
	}
}
