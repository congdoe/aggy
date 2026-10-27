//go:build unix

package patch

import (
	"os"
	"syscall"
	"testing"
)

// TestOwnFilesSingleLink: safefs refuses to read a file with more than one
// hard link, so the files aggy writes, and the backups it reads back on
// revert, must each have exactly one.
func TestOwnFilesSingleLink(t *testing.T) {
	orig := "a = 1\n"
	e := setup(t, ptr(orig))
	rec, err := Apply(Patch{File: e.file, ExpectedHash: Hash([]byte(orig)), Transform: addLine}, e.backups)
	if err != nil {
		t.Fatal(err)
	}
	// The same bytes backed up twice reuse one backup file, by copy, not
	// by link.
	if _, err := backup(e.backups, []byte(orig)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{rec.File, rec.BackupPath} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if n := fi.Sys().(*syscall.Stat_t).Nlink; n != 1 {
			t.Errorf("%s has %d links, want 1", p, n)
		}
	}
	if err := Revert(rec); err != nil {
		t.Fatalf("Revert = %v", err)
	}
}
