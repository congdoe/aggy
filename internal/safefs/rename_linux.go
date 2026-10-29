package safefs

import "golang.org/x/sys/unix"

// atomicRename renames from to to in the directory fd with renameat2 and
// RENAME_NOREPLACE, so a name made at to after the caller's check is
// kept and the call fails with EEXIST.
func atomicRename(fd int, from, to string) error {
	return unix.Renameat2(fd, from, fd, to, unix.RENAME_NOREPLACE)
}
