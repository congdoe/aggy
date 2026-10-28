//go:build darwin || freebsd || netbsd

package safefs

import (
	"io/fs"
	"syscall"
)

// FileBirth returns the birth (creation) time of the file fi describes,
// in Unix nanoseconds, from metadata safefs returned. ok is false where
// the file system does not report one.
func FileBirth(fi fs.FileInfo) (nsec int64, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, false
	}
	n := st.Birthtimespec.Nano()
	return n, n > 0
}
