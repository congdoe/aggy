//go:build unix

package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/congdoe/aggy/internal/safefs"
)

// trustedPath refuses p unless p, what it resolves to, and every
// directory above either belong to the user or root and are writable by
// neither group nor others: whoever could replace any of them would run
// code in every session of every CLI with aggy's hooks.
func trustedPath(p string) error {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return err
	}
	for _, q := range []string{p, real} {
		for d := q; ; d = filepath.Dir(d) {
			fi, err := safefs.Lstat(d)
			if err != nil {
				return err
			}
			if fi.Mode()&fs.ModeSymlink == 0 {
				st, ok := fi.Sys().(*syscall.Stat_t)
				if ok && int(st.Uid) != os.Getuid() && st.Uid != 0 {
					return fmt.Errorf("%s belongs to another user, so aggy will not have every CLI session run it", d)
				}
				if fi.Mode().Perm()&0o022 != 0 {
					return fmt.Errorf("%s is writable by others (mode %#o), so aggy will not have every CLI session run aggy from below it", d, fi.Mode().Perm())
				}
			}
			if d == filepath.Dir(d) {
				break
			}
		}
	}
	return nil
}
