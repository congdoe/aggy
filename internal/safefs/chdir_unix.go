//go:build unix

package safefs

import (
	"errors"
	"io/fs"
	"os"
)

// ChdirNoFollow moves the process to dir, which must still be the
// directory want describes (the caller's Lstat of dir, F74-1): dir is
// opened with O_NOFOLLOW, so a symlink swapped in after that
// Lstat is refused, the descriptor is checked (fstat, SameFile) against
// want, so another directory swapped in is refused (ErrChanged), and
// the process moves with fchdir, to the folder checked and no other.
func ChdirNoFollow(dir string, want fs.FileInfo) error {
	if want == nil || !want.IsDir() {
		return &fs.PathError{Op: "Chdir", Path: dir, Err: errors.New("no directory identity to compare")}
	}
	// O_NONBLOCK: a FIFO swapped in does not block the open; the fstat
	// below refuses anything but want's directory.
	f, err := os.OpenFile(dir, os.O_RDONLY|oNoFollow|oNonBlock, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	got, err := f.Stat()
	if err != nil {
		return err
	}
	if !got.IsDir() || !os.SameFile(want, got) {
		return &fs.PathError{Op: "Chdir", Path: dir, Err: ErrChanged}
	}
	return f.Chdir()
}
