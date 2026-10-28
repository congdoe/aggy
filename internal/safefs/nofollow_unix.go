//go:build unix

package safefs

import (
	"io/fs"
	"syscall"
)

const (
	oNoFollow = syscall.O_NOFOLLOW
	oNonBlock = syscall.O_NONBLOCK
)

// linkCount returns the number of hard links to the file fi describes.
func linkCount(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}

// canWrite reports whether access(2) allows creating files in dir.
func canWrite(dir string) bool { return syscall.Access(dir, 0x2) == nil } // W_OK
