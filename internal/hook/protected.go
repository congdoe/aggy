package hook

import (
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/congdoe/aggy/internal/safefs"
)

// SameAsProtected reports p, or any folder on its way up, being the same
// file as one of protected, by device and inode (#133 review). A spelling
// comparison misses a path in other case, or in another Unicode
// normalisation, on a file system that folds them (macOS by default), where
// it names the very same folder; identity does not. stat reads a protected
// path, following a link to it (aggy's state folder may be one); lstat reads
// each part of p. A protected path that does not exist yet adds nothing, as
// nothing is there to read; a part of p that cannot be read is passed over,
// and the callers also compare spellings.
func SameAsProtected(p string, protected []string, stat, lstat func(string) (fs.FileInfo, error)) bool {
	type id struct{ dev, ino uint64 }
	var ids []id
	for _, q := range protected {
		if q == "" {
			continue
		}
		if fi, err := stat(q); err == nil {
			if d, i, ok := safefs.FileID(fi); ok {
				ids = append(ids, id{d, i})
			}
		}
	}
	if len(ids) == 0 {
		return false
	}
	for cur := filepath.Clean(p); ; {
		if fi, err := lstat(cur); err == nil {
			if d, i, ok := safefs.FileID(fi); ok && slices.Contains(ids, id{d, i}) {
				return true
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}
