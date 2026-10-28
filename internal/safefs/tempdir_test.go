package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMkdirTempAndRemoveAll(t *testing.T) {
	home := fakeHome(t)
	dir, err := MkdirTemp(t.TempDir(), "aggy-x-")
	if err != nil || !strings.Contains(filepath.Base(dir), "aggy-x-") {
		t.Fatalf("MkdirTemp = %q, %v", dir, err)
	}
	assertMode(t, dir, 0o700)
	// A symlink inside the tree is removed, not followed.
	outside := filepath.Join(t.TempDir(), "keep")
	must(t, os.WriteFile(outside, []byte("k"), 0o600))
	must(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o700))
	must(t, os.Symlink(filepath.Dir(outside), filepath.Join(dir, "sub", "link")))
	if err := RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tree left: %v", err)
	}
	assertContent(t, outside, "k")
	// Credential paths are refused.
	auth := filepath.Join(home, ".codex", "auth.json")
	writeFake(t, auth)
	if err := RemoveAll(auth); !errors.Is(err, ErrAuthPath) {
		t.Errorf("RemoveAll(credential) = %v", err)
	}
	if _, err := MkdirTemp(auth, "x"); !errors.Is(err, ErrAuthPath) {
		t.Errorf("MkdirTemp(in credential) = %v", err)
	}
}
