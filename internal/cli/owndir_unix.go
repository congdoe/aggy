//go:build unix

package cli

import (
	"io/fs"
	"os"
	"syscall"
)

// ownPrivateDir reports whether fi (from Lstat) is a directory, not a
// symlink, owned by this user, with mode 0700.
func ownPrivateDir(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.IsDir() && fi.Mode().Perm() == 0o700 && int(st.Uid) == os.Getuid()
}
