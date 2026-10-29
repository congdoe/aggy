package safefs

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplaceAt renames from to to in dir with renameat2 and
// RENAME_NOREPLACE, so a name made at to after the caller's check is
// kept and the call fails with EEXIST.
func renameNoReplaceAt(dir *os.Root, from, to string) error {
	return renameAtFd(dir, to, func(fd int) error {
		return unix.Renameat2(fd, from, fd, to, unix.RENAME_NOREPLACE)
	})
}
