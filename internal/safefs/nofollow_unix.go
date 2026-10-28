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

// fileID returns the device and inode of the file fi describes.
func fileID(fi fs.FileInfo) (id, bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return id{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true
	}
	return id{}, false
}
