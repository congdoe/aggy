package safefs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Dir is a directory opened once, for walking a tree through
// descriptors: every call names one entry of this directory, relative to
// its descriptor, and a subdirectory is opened relative to it too. So a
// folder of the tree swapped for a symlink after the walk looked at it
// is refused (ErrChanged), never followed, and nothing outside the tree
// is read. Every entry is checked against the credential paths and
// identities, like the rest of safefs.
type Dir struct {
	r    *os.Root
	path string // for errors and the credential path check
	g    *guard
}

// OpenDir opens the directory name, which must have want's identity (the
// caller's Lstat of name, so a symlink is refused), behind both checks.
func OpenDir(name string, want fs.FileInfo) (*Dir, error) {
	if want == nil || !want.IsDir() {
		return nil, &fs.PathError{Op: "OpenDir", Path: name, Err: errors.New("no directory identity to compare")}
	}
	t, err := resolve("OpenDir", name)
	if err != nil {
		return nil, err
	}
	var r *os.Root
	err = inParent("OpenDir", name, t.link, t.linkDir, func(dir *os.Root, base string) error {
		var err error
		r, err = dir.OpenRoot(base)
		return err
	})
	if err != nil {
		return nil, renamePath(err, name)
	}
	d := &Dir{r: r, path: name, g: t.g}
	if err := d.same(name, want); err != nil {
		_ = r.Close()
		return nil, err
	}
	return d, nil
}

// Close closes the directory's descriptor.
func (d *Dir) Close() error { return d.r.Close() }

// same checks that d, just opened, has want's identity and is not a
// credential.
func (d *Dir) same(full string, want fs.FileInfo) error {
	got, err := d.r.Stat(".")
	switch {
	case err != nil:
		return renamePath(err, full)
	case d.g.idMatch(got):
		return &AuthPathError{Op: "OpenDir", Path: full}
	case !got.IsDir() || !os.SameFile(want, got):
		return &fs.PathError{Op: "OpenDir", Path: full, Err: ErrChanged}
	}
	return nil
}

// entry checks that base is one entry name and returns its full path,
// refusing a credential path.
func (d *Dir) entry(op, base string) (string, error) {
	full := filepath.Join(d.path, base)
	if base == "" || base == "." || base == ".." || filepath.Base(base) != base {
		return "", &fs.PathError{Op: op, Path: full, Err: fmt.Errorf("bad entry name %q", base)}
	}
	if d.g.pathMatch(full) {
		return "", &AuthPathError{Op: op, Path: full}
	}
	return full, nil
}

// ReadDir returns the directory's entries sorted by name.
func (d *Dir) ReadDir() ([]fs.DirEntry, error) {
	f, err := d.r.Open(".")
	if err != nil {
		return nil, renamePath(err, d.path)
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, renamePath(err, d.path)
}

// Lstat is os.Lstat of the entry base, without following a symlink.
func (d *Dir) Lstat(base string) (fs.FileInfo, error) {
	full, err := d.entry("Lstat", base)
	if err != nil {
		return nil, err
	}
	fi, err := d.r.Lstat(base)
	if err != nil {
		return nil, renamePath(err, full)
	}
	if d.g.idMatch(fi) {
		return nil, &AuthPathError{Op: "Lstat", Path: full}
	}
	return fi, nil
}

// OpenDir opens the subdirectory base relative to d; it must still have
// want's identity (the caller's Lstat of it).
func (d *Dir) OpenDir(base string, want fs.FileInfo) (*Dir, error) {
	full, err := d.entry("OpenDir", base)
	if err != nil {
		return nil, err
	}
	if want == nil || !want.IsDir() {
		return nil, &fs.PathError{Op: "OpenDir", Path: full, Err: errors.New("no directory identity to compare")}
	}
	beforeAct("OpenDir")
	r, err := d.r.OpenRoot(base)
	if err != nil {
		return nil, renamePath(err, full)
	}
	sub := &Dir{r: r, path: full, g: d.g}
	if err := sub.same(full, want); err != nil {
		_ = r.Close()
		return nil, err
	}
	return sub, nil
}

// ReadFileLimit reads the regular file base of at most max bytes. The
// open never follows a symlink (O_NOFOLLOW where the system has it), and
// the opened file must have want's identity (the caller's Lstat of it):
// a swap is refused with ErrChanged, anything but a regular file with
// ErrNotRegular, a second hard link with ErrHardLink, and more than max
// bytes with ErrTooLarge.
func (d *Dir) ReadFileLimit(base string, want fs.FileInfo, max int64) ([]byte, error) {
	full, err := d.entry("ReadFile", base)
	if err != nil {
		return nil, err
	}
	if want == nil {
		return nil, &fs.PathError{Op: "ReadFile", Path: full, Err: errors.New("no identity to compare")}
	}
	beforeAct("ReadFile")
	f, err := d.r.OpenFile(base, os.O_RDONLY|oNoFollow|oNonBlock, 0)
	if err != nil {
		return nil, renamePath(err, full)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	switch {
	case err != nil:
		return nil, renamePath(err, full)
	case d.g.idMatch(fi):
		return nil, &AuthPathError{Op: "ReadFile", Path: full}
	case !fi.Mode().IsRegular():
		return nil, &fs.PathError{Op: "ReadFile", Path: full, Err: ErrNotRegular}
	case isMultiLinked(fi):
		return nil, &fs.PathError{Op: "ReadFile", Path: full, Err: ErrHardLink}
	case !os.SameFile(want, fi):
		return nil, &fs.PathError{Op: "ReadFile", Path: full, Err: ErrChanged}
	case fi.Size() > max:
		return nil, &fs.PathError{Op: "ReadFile", Path: full, Err: ErrTooLarge}
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, renamePath(err, full)
	}
	if int64(len(b)) > max {
		return nil, &fs.PathError{Op: "ReadFile", Path: full, Err: ErrTooLarge}
	}
	return b, nil
}
