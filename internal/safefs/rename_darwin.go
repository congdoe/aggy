package safefs

import "golang.org/x/sys/unix"

// atomicRename renames from to to in the directory fd with renameatx_np
// and RENAME_EXCL, so a name made at to after the caller's check is kept
// and the call fails with EEXIST.
func atomicRename(fd int, from, to string) error {
	return unix.RenameatxNp(fd, from, fd, to, unix.RENAME_EXCL)
}
