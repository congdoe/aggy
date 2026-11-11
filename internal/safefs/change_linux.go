//go:build linux

package safefs

import (
	"io/fs"
	"syscall"
)

// FileChange returns the status change time (ctime) of the file fi
// describes, in Unix nanoseconds, from metadata safefs returned. ok is false
// where it is not available. Nothing sets a ctime directly: the kernel moves
// it to now on any write, chmod, chown, link, or rename of the file. Linux
// stamps it from a coarse clock, so two changes a few milliseconds apart can
// share one.
func FileChange(fi fs.FileInfo) (nsec int64, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, false
	}
	n := st.Ctim.Nano()
	return n, n > 0
}
