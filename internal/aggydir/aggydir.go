// Package aggydir is the one rule for a repo's .aggy folder, where aggy
// keeps the task file, research reports, and image sidecars.
//
// A cloned repo can ship anything in .aggy, so:
//
//   - No symlink anywhere under .aggy: .aggy itself, every directory
//     below it, and the file read or written must be real. A symlink is
//     refused, never followed, even one that points inside the repo.
//   - Directories are made through descriptors from the repo root
//     (safefs.MkdirChain), and a write lands only in the directory made
//     (safefs.WriteFileNoFollowIn).
//   - Nothing read from .aggy is aggy's own record on its word alone. It
//     is data (the task file, fenced and redacted in a brief), or it
//     counts only when the store under ~/.aggy confirms it (an image
//     sidecar names a session that --edit resumes only when the store
//     recorded that session for this repo and an image).
package aggydir

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/safefs"
)

// Name is the per-repo folder.
const Name = ".aggy"

// ErrLink means a path under .aggy is a symlink, or not a directory where
// one belongs.
var ErrLink = errors.New("a symlink or not a real directory under .aggy")

// MkdirAll makes base/.aggy/elems... and returns its path (under base as
// given) and its identity, for safefs.WriteFileNoFollowIn. base may be
// reached through symlinks (a repo under /tmp on macOS); nothing from
// .aggy down may be one.
func MkdirAll(base string, perm fs.FileMode, elems ...string) (string, fs.FileInfo, error) {
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", nil, err
	}
	fi, err := safefs.Lstat(real)
	if err != nil {
		return "", nil, err
	}
	chain := append([]string{Name}, elems...)
	dir, err := safefs.MkdirChain(real, fi, chain, perm)
	p := filepath.Join(append([]string{base}, chain...)...)
	var pe *fs.PathError
	if errors.Is(err, safefs.ErrNotDir) && errors.As(err, &pe) {
		return "", nil, fmt.Errorf("%s: %w; aggy will not write through it", pe.Path, ErrLink)
	}
	if err != nil {
		return "", nil, err
	}
	return p, dir, nil
}

// ReadFile reads base/.aggy/rel (slash-separated), a regular file of at
// most max bytes. .aggy and every directory down to the file must be real
// directories, and the file must not be a symlink. A missing file or
// folder is an error matching fs.ErrNotExist.
//
// shortcut: the directories are checked by path, and only the file's own
// directory is pinned (safefs.ReadConfig), so a same-uid racer could swap
// an upper level between the checks; what is read is untrusted data either
// way. Upgrade to a descriptor walk if a read ever becomes trusted.
func ReadFile(base, rel string, max int64) ([]byte, error) {
	parts := strings.Split(rel, "/")
	p := filepath.Join(base, Name)
	for _, e := range parts[:len(parts)-1] {
		if err := realDir(p); err != nil {
			return nil, err
		}
		p = filepath.Join(p, e)
	}
	if err := realDir(p); err != nil {
		return nil, err
	}
	return safefs.ReadConfig(filepath.Join(p, parts[len(parts)-1]), max)
}

// realDir returns nil when p is a real directory, an error matching
// fs.ErrNotExist when it is missing, and ErrLink otherwise.
func realDir(p string) error {
	fi, err := safefs.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s: %w", p, ErrLink)
	}
	return nil
}
