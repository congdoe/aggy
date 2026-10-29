//go:build linux || darwin

package safefs

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// atomicRenameFn is atomicRename; tests replace it to act as a file
// system without the no-replace flag.
var atomicRenameFn = atomicRename

// errNoAtomicRename means the file system refused the no-replace flag:
// NFS, 9p and WSL drvfs, and some FUSE file systems return EINVAL (or
// ENOSYS, ENOTSUP) for any rename flags.
var errNoAtomicRename = errors.New("the file system has no atomic no-replace rename")

// renameNoReplaceAt renames from to to in dir, the checked parent, and
// keeps a name made at to after the caller's check: with the no-replace
// rename, or where the file system has none, for a regular file with
// link(2), which refuses an existing name, then unlink(2) of from.
func renameNoReplaceAt(dir *os.Root, from, to string, isDir bool) error {
	f, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var rerr error
	if err := rc.Control(func(fd uintptr) { rerr = renameAt(int(fd), from, to, isDir) }); err != nil {
		return err
	}
	if errors.Is(rerr, errNoAtomicRename) {
		// shortcut: a folder has no link(2), so it is checked and then
		// renamed, and a name made in between can be replaced; upgrade if
		// these file systems gain a no-replace rename.
		return dir.Rename(from, to)
	}
	if rerr != nil {
		return &fs.PathError{Op: "Rename", Path: to, Err: rerr}
	}
	return nil
}

// renameAt is renameNoReplaceAt on the directory fd. It returns
// errNoAtomicRename only for a folder the file system cannot rename
// without replacing.
func renameAt(fd int, from, to string, isDir bool) error {
	err := atomicRenameFn(fd, from, to)
	if !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOSYS) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	if isDir {
		return errNoAtomicRename
	}
	if err := unix.Linkat(fd, from, fd, to, 0); err != nil {
		return err
	}
	if err := unix.Unlinkat(fd, from, 0); err != nil {
		_ = unix.Unlinkat(fd, to, 0) // the new name only; from is still there
		return err
	}
	return nil
}
