package procs

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
)

// statusFile is what v1 stats for the owning uid (modules/procs/ranker.py:140).
//
// The pid directory itself would answer the same question, and this asks the file
// v1 asks so that the two agree about which inode's ownership is the process's.
const statusFile = "status"

// lookupOwner is the live owner lookup: stat the process's status file for its
// uid, then this host's user database for the name.
//
// A real path rather than the Source's fs.FS, because there is no way to ask an
// fs.FileInfo who owns a file -- Sys is nil under fstest.MapFS and holds a
// platform-specific struct everywhere else. That is what Source.Owner is for, and
// why the field exists rather than a second filesystem seam.
//
// Nil at every failure, and none of them are reported: a process that exited
// between the readdir and this stat is routine, and a uid with no passwd entry is
// the normal state of affairs in a container. The row is still worth having with
// no name on it.
func lookupOwner(dir string, pid int64) *string {
	info, err := os.Stat(filepath.Join(dir, strconv.FormatInt(pid, 10), statusFile))
	if err != nil {
		return nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return userName(uint64(st.Uid))
}

// names caches uid lookups for the life of the process.
//
// user.LookupId reads the passwd database, and without cgo that means parsing
// /etc/passwd -- once per process on a host running two thousand of them, for an
// answer that is the same every time. The cache is unbounded on purpose: it is
// bounded in practice by how many distinct uids a host has, which is a property of
// the passwd file rather than of the process table.
var names sync.Map // uint64 -> *string

func userName(uid uint64) *string {
	if cached, ok := names.Load(uid); ok {
		name, _ := cached.(*string)
		return name
	}
	var name *string
	if u, err := user.LookupId(strconv.FormatUint(uid, 10)); err == nil {
		n := u.Username
		name = &n
	}
	names.Store(uid, name)
	return name
}
