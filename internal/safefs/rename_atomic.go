//go:build linux || darwin

package safefs

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// renameAtFd runs rename with the descriptor of dir, the checked parent.
// EINVAL, ENOSYS, and ENOTSUP (a file system without the no-replace
// flag) become ErrNoAtomicRename: nothing was renamed, and there is no
// fallback that could replace a name.
func renameAtFd(dir *os.Root, to string, rename func(fd int) error) error {
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
	if err := rc.Control(func(fd uintptr) { rerr = rename(int(fd)) }); err != nil {
		return err
	}
	switch {
	case rerr == nil:
		return nil
	case errors.Is(rerr, syscall.EINVAL), errors.Is(rerr, syscall.ENOSYS), errors.Is(rerr, syscall.ENOTSUP):
		return &fs.PathError{Op: "Rename", Path: to, Err: ErrNoAtomicRename}
	}
	return &fs.PathError{Op: "Rename", Path: to, Err: rerr}
}
