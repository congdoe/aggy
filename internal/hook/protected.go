package hook

import (
	"errors"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/congdoe/aggy/internal/safefs"
)

// SameAsProtected reports p, or any folder on its way up, being the same
// file as one of protected, by device and inode (#133 review). A spelling
// comparison misses a path in other case, or in another Unicode
// normalisation, on a file system that folds them (macOS by default), where
// it names the very same folder; identity does not. stat follows links: it
// reads each protected path (aggy's state folder may be one) and each folder
// above p, so a linked parent counts as where it leads; lstat reads p itself.
// It fails closed (#133 re-review): a part that does not exist adds nothing,
// but any other error reading a protected path or a part of p reports p as
// protected.
func SameAsProtected(p string, protected []string, stat, lstat func(string) (fs.FileInfo, error)) bool {
	type id struct{ dev, ino uint64 }
	var ids []id
	for _, q := range protected {
		if q == "" {
			continue
		}
		fi, err := stat(q)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return true
		}
		d, i, ok := safefs.FileID(fi)
		if !ok {
			return true
		}
		ids = append(ids, id{d, i})
	}
	if len(ids) == 0 {
		return false
	}
	read := lstat
	for cur := filepath.Clean(p); ; {
		fi, err := read(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return true
		default:
			d, i, ok := safefs.FileID(fi)
			if !ok || slices.Contains(ids, id{d, i}) {
				return true
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur, read = parent, stat
	}
}
