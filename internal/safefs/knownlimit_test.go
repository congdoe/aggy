//go:build unix

package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHardLinkBeforeRotation documents a known gap (TODO(T6724)): a hard
// link made to a credential before the vendor rotates its token keeps the
// old inode, with one link and the old token, and safefs reads it. The
// gap is closed at the M1 artifact boundary, not here. The test skips
// while the gap is open, and fails if safefs starts refusing it, so the
// doc is updated together with the behavior.
func TestHardLinkBeforeRotation(t *testing.T) {
	home := fakeHome(t)
	auth := filepath.Join(home, ".codex", "auth.json")
	writeFake(t, auth)
	must(t, Check(filepath.Join(t.TempDir(), "x")))
	link := filepath.Join(t.TempDir(), "old-token.png")
	if err := os.Link(auth, link); err != nil {
		t.Skipf("hard links not supported here: %v", err)
	}
	// While the second link exists, the link count refuses it.
	if _, err := ReadFile(link); !errors.Is(err, ErrAuthPath) {
		t.Errorf("ReadFile(link before rotation) = %v, want ErrAuthPath", err)
	}
	tmp := auth + ".tmp"
	must(t, os.WriteFile(tmp, []byte("new token"), 0o600))
	must(t, os.Rename(tmp, auth))
	if _, err := ReadFile(link); errors.Is(err, ErrAuthPath) || errors.Is(err, ErrHardLink) {
		t.Fatal("safefs now refuses a link made before a rotation: update the ErrHardLink doc and this test")
	}
	b, err := os.ReadFile("safefs.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "TODO(T6724)") {
		t.Error("the ErrHardLink doc lacks the TODO(T6724) note for this gap")
	}
	t.Skip("known limit TODO(T6724): a hard link made before a token rotation is read; closed at the M1 artifact boundary")
}

// TestNoIdentityHistoryFile: safefs keeps no state file of its own; a
// new file aggy creates is never refused for an inode a credential once
// had (ext4 reuses inode numbers at once).
func TestNoIdentityHistoryFile(t *testing.T) {
	home := fakeHome(t)
	dir := filepath.Join(home, ".codex")
	auth := filepath.Join(dir, "auth.json")
	for i := range 20 {
		writeFake(t, auth)
		must(t, Check(filepath.Join(t.TempDir(), "x")))
		must(t, os.Remove(auth))
		p := filepath.Join(dir, "new.toml")
		if err := WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("round %d: WriteFile = %v", i, err)
		}
		if _, err := ReadFile(p); err != nil {
			t.Fatalf("round %d: ReadFile = %v", i, err)
		}
		must(t, Remove(p))
	}
	entries, err := os.ReadDir(os.Getenv("AGGY_HOME"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("safefs wrote state: %v", entries)
	}
}
