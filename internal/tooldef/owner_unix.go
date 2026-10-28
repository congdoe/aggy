//go:build unix

package tooldef

import (
	"io/fs"
	"os"
	"syscall"
)

// notMine says why fi is not the current user's own file, or "".
func notMine(fi fs.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return "its owner cannot be read"
	}
	if int(st.Uid) != os.Getuid() {
		return "owned by another user"
	}
	return ""
}
