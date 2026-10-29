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
			uid := -1
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				uid = int(st.Uid)
			}
			if err := trustedEntry(d, fi.Mode(), uid, os.Getuid(), d == q); err != nil {
				return err
			}
			if d == filepath.Dir(d) {
				break
			}
		}
	}
	return nil
}

// trustedEntry checks one entry on the way to the binary: d, with its
// mode and owner uid (-1 when unknown), for the user me; leaf is the
// binary itself.
//
// Every entry, a symlink too, must be the user's or root's: in a sticky
// folder the owner of an entry can remove and recreate it, so another
// user's symlink there could later point elsewhere. A non-symlink must
// also be writable by neither group nor others, except a sticky folder
// (/tmp) above the binary: nobody else can rename or remove an entry in
// it they do not own, and the entry below it is checked in turn.
func trustedEntry(d string, mode fs.FileMode, uid, me int, leaf bool) error {
	if uid != me && uid != 0 {
		return fmt.Errorf("%s belongs to another user, so aggy will not have every CLI session run it", d)
	}
	if mode&fs.ModeSymlink != 0 {
		return nil // a symlink's own mode bits mean nothing
	}
	if mode.Perm()&0o022 != 0 && (leaf || mode&fs.ModeSticky == 0 || !mode.IsDir()) {
		return fmt.Errorf("%s is writable by others (mode %#o), so aggy will not have every CLI session run aggy from below it", d, mode.Perm())
	}
	return nil
}
