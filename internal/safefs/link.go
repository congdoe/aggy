package safefs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The calls in this file act on a symlink itself, never on what it
// points to: each works on the last element of name in its checked parent
// directory. Those that change the name take want, the Lstat of what the
// caller expects to find there (nil: nothing), and refuse with ErrChanged
// when the name has another identity, so a file swapped in since the
// caller looked is never replaced or removed.

// Readlink returns the target of the symlink name, behind both checks.
func Readlink(name string) (string, error) {
	t, err := resolve("Readlink", name)
	if err != nil {
		return "", err
	}
	var target string
	err = inParent("Readlink", name, t.link, t.linkDir, func(dir *os.Root, base string) error {
		if fi, err := dir.Lstat(base); err == nil && t.g.idMatch(fi) {
			return &AuthPathError{Op: "Readlink", Path: name}
		}
		var err error
		target, err = dir.Readlink(base)
		return err
	})
	return target, renamePath(err, name)
}

// Symlink makes name a symlink to target. With want nil, name must not
// exist. Otherwise the link is made under a temporary name and renamed
// over name, which must still have want's identity, so the swap is
// atomic.
//
// A target that is, or resolves to, a vendor credential file is refused
// (ErrAuthPath), whether or not it exists, and so is replacing a
// credential file. A relative target is judged from name's directory, as
// the kernel follows it. As with WriteFile, an error matching ErrDirSync
// means the link is in place but its directory was not synced.
func Symlink(target, name string, want fs.FileInfo) error {
	t, err := resolve("Symlink", name)
	if err != nil {
		return err
	}
	if err := checkTarget(name, filepath.Dir(t.link), target); err != nil {
		return err
	}
	var syncErr error
	err = inParent("Symlink", name, t.link, t.linkDir, func(dir *os.Root, base string) error {
		if err := expect(t.g, name, dir, base, want); err != nil {
			return err
		}
		if want == nil {
			if err := dir.Symlink(target, base); err != nil {
				return err
			}
			syncErr = syncDir(dir)
			return nil
		}
		tmp, err := tempName(base)
		if err != nil {
			return err
		}
		if err := dir.Symlink(target, tmp); err != nil {
			return err
		}
		if err := expect(t.g, name, dir, base, want); err != nil {
			_ = dir.Remove(tmp)
			return err
		}
		if err := dir.Rename(tmp, base); err != nil {
			_ = dir.Remove(tmp)
			return err
		}
		syncErr = syncDir(dir)
		return nil
	})
	if err != nil {
		return renamePath(err, name)
	}
	if syncErr != nil {
		return &DirSyncError{Path: name, Err: syncErr}
	}
	return nil
}

// CheckLinkTarget returns an error matching ErrAuthPath when a symlink
// at name pointing to target would point at a vendor credential file, by
// path or, when it exists, by identity. Symlink runs the same check; this
// is for a dry run.
func CheckLinkTarget(name, target string) error {
	t, err := resolve("Symlink", name)
	if err != nil {
		return err
	}
	return checkTarget(name, filepath.Dir(t.link), target)
}

// checkTarget judges target from dir, the link's resolved directory. A
// relative target is joined without cleaning, so a ".." after a symlink
// in it keeps the meaning the kernel gives it.
func checkTarget(name, dir, target string) error {
	if target == "" {
		return &fs.PathError{Op: "Symlink", Path: name, Err: errors.New("empty link target")}
	}
	abs := target
	if !filepath.IsAbs(abs) {
		abs = dir + string(filepath.Separator) + target
	}
	if err := Check(abs); errors.Is(err, ErrAuthPath) {
		return &AuthPathError{Op: "Symlink", Path: name}
	} else if err != nil {
		return err
	}
	return nil
}

// WriteFileNoFollow writes data to name atomically, like WriteFile, but
// replaces what is at name itself (a symlink is replaced, not written
// through), and only while name has want's identity (want nil: name must
// not exist). The new file gets perm.
func WriteFileNoFollow(name string, data []byte, perm fs.FileMode, want fs.FileInfo) error {
	t, err := resolve("WriteFile", name)
	if err != nil {
		return err
	}
	var syncErr error
	err = inParent("WriteFile", name, t.link, t.linkDir, func(dir *os.Root, base string) error {
		if err := expect(t.g, name, dir, base, want); err != nil {
			return err
		}
		tmpName, tmp, err := createTemp(dir, base)
		if err != nil {
			return err
		}
		ok := false
		defer func() {
			if !ok {
				_ = tmp.Close()
				_ = dir.Remove(tmpName)
			}
		}()
		if err := tmp.Chmod(perm); err != nil {
			return err
		}
		if _, err := tmp.Write(data); err != nil {
			return err
		}
		if err := tmp.Sync(); err != nil {
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := expect(t.g, name, dir, base, want); err != nil {
			return err
		}
		if err := dir.Rename(tmpName, base); err != nil {
			return err
		}
		ok = true
		syncErr = syncDir(dir)
		return nil
	})
	if err != nil {
		return renamePath(err, name)
	}
	if syncErr != nil {
		return &DirSyncError{Path: name, Err: syncErr}
	}
	return nil
}

// RemoveSame removes name, a file or the symlink itself, only while it
// has want's identity.
func RemoveSame(name string, want fs.FileInfo) error {
	if want == nil {
		return &fs.PathError{Op: "Remove", Path: name, Err: errors.New("no identity to compare")}
	}
	t, err := resolve("Remove", name)
	if err != nil {
		return err
	}
	return renamePath(inParent("Remove", name, t.link, t.linkDir, func(dir *os.Root, base string) error {
		if err := expect(t.g, name, dir, base, want); err != nil {
			return err
		}
		return dir.Remove(base)
	}), name)
}

// expect checks that base in dir has want's identity and type, or does
// not exist when want is nil, and is not a credential file.
func expect(g *guard, name string, dir *os.Root, base string, want fs.FileInfo) error {
	fi, err := dir.Lstat(base)
	if want == nil {
		if err == nil {
			return &fs.PathError{Op: "Symlink", Path: name, Err: fs.ErrExist}
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	if g.idMatch(fi) {
		return &AuthPathError{Op: "replace", Path: name}
	}
	if !os.SameFile(want, fi) || want.Mode().Type() != fi.Mode().Type() {
		return &fs.PathError{Op: "replace", Path: name, Err: ErrChanged}
	}
	return nil
}

// tempName returns a fresh hidden name next to base.
func tempName(base string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("safefs: temp name: %w", err)
	}
	return "." + base + ".aggy-" + hex.EncodeToString(b[:]), nil
}
