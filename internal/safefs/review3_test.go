package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Regression tests for the PR #4 re-reviews (T6725). Each fails on 2e1e234.

// TestFullCaseFolding: APFS folds the long s, sharp s, and Kelvin sign,
// so a credential that does not exist yet must match those spellings too.
// On 2e1e234, WriteFile(~/.claude/.credentialſ.json) created
// .credentials.json.
func TestFullCaseFolding(t *testing.T) {
	longS := string(rune(0x17f))   // LATIN SMALL LETTER LONG S
	sharpS := string(rune(0xdf))   // LATIN SMALL LETTER SHARP S
	kelvin := string(rune(0x212a)) // KELVIN SIGN
	old := foldCase
	foldCase = true
	t.Cleanup(func() { foldCase = old })
	home := fakeHome(t)
	strasse := filepath.Join(t.TempDir(), "Strasse")
	t.Setenv("CLAUDE_CONFIG_DIR", strasse)
	for _, p := range []string{
		filepath.Join(home, ".claude", ".credential"+longS+".json"),
		filepath.Join(home, ".gro"+kelvin, "auth.json"),
		filepath.Join(filepath.Dir(strasse), "Stra"+sharpS+"e", ".credentials.json"),
	} {
		if err := WriteFile(p, []byte("x"), 0o600); !errors.Is(err, ErrAuthPath) {
			t.Errorf("WriteFile(%q) = %v, want ErrAuthPath", p, err)
		}
		if err := Check(p); !errors.Is(err, ErrAuthPath) {
			t.Errorf("Check(%q) = %v, want ErrAuthPath", p, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", ".credentials.json")); err == nil {
		t.Error("a credential file was created")
	}
}

// TestRemoveAllRefusesTreeWithCredential: removing a directory that holds
// a credential file must be refused, and nothing removed. On 2e1e234,
// RemoveAll(~/.codex) deleted auth.json.
func TestRemoveAllRefusesTreeWithCredential(t *testing.T) {
	home := fakeHome(t)
	auth := filepath.Join(home, ".codex", "auth.json")
	writeFake(t, auth)
	other := filepath.Join(home, ".codex", "config.toml")
	must(t, os.WriteFile(other, []byte("x"), 0o600))
	for _, p := range []string{filepath.Join(home, ".codex"), home} {
		if err := RemoveAll(p); !errors.Is(err, ErrAuthPath) {
			t.Errorf("RemoveAll(%s) = %v, want ErrAuthPath", p, err)
		}
	}
	for _, p := range []string{auth, other} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	// A credential path that does not exist yet is matched lexically.
	grok := filepath.Join(home, ".grok")
	must(t, os.MkdirAll(grok, 0o700))
	if err := RemoveAll(grok); !errors.Is(err, ErrAuthPath) {
		t.Errorf("RemoveAll(%s) = %v, want ErrAuthPath", grok, err)
	}
}

// TestRemoveAllRefusesLinkedCredentialInTree: a tree elsewhere holding a
// hard link to a credential is refused by identity, before anything in it
// is removed.
func TestRemoveAllRefusesLinkedCredentialInTree(t *testing.T) {
	home := fakeHome(t)
	auth := filepath.Join(home, ".codex", "auth.json")
	writeFake(t, auth)
	tree := filepath.Join(t.TempDir(), "tree")
	must(t, os.MkdirAll(filepath.Join(tree, "a", "b"), 0o700))
	keep := filepath.Join(tree, "a", "keep.txt")
	must(t, os.WriteFile(keep, []byte("k"), 0o600))
	if err := os.Link(auth, filepath.Join(tree, "a", "b", "x.png")); err != nil {
		t.Skipf("hard links not supported here: %v", err)
	}
	if err := RemoveAll(tree); !errors.Is(err, ErrAuthPath) {
		t.Errorf("RemoveAll = %v, want ErrAuthPath", err)
	}
	if _, err := os.Lstat(keep); err != nil {
		t.Errorf("part of the tree was removed: %v", err)
	}
	// An ordinary tree is still removed.
	plain := filepath.Join(t.TempDir(), "plain")
	must(t, os.MkdirAll(filepath.Join(plain, "a"), 0o700))
	must(t, os.WriteFile(filepath.Join(plain, "a", "f"), []byte("f"), 0o600))
	if err := RemoveAll(plain); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(plain); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plain tree not removed: %v", err)
	}
}

// TestReadConfigRefusesLinkedDir (L5): a config file whose own directory
// is a symlink is refused; ReadFileLimit still allows it.
func TestReadConfigRefusesLinkedDir(t *testing.T) {
	fakeHome(t)
	base := t.TempDir()
	real := filepath.Join(base, "elsewhere")
	must(t, os.MkdirAll(real, 0o700))
	must(t, os.WriteFile(filepath.Join(real, "config.toml"), []byte("x = 1\n"), 0o600))
	repo := filepath.Join(base, "repo")
	must(t, os.MkdirAll(repo, 0o700))
	must(t, os.Symlink(real, filepath.Join(repo, ".aggy")))
	p := filepath.Join(repo, ".aggy", "config.toml")
	if _, err := ReadConfig(p, 100); !errors.Is(err, ErrLinkedDir) {
		t.Errorf("ReadConfig through a linked dir = %v, want ErrLinkedDir", err)
	}
	if b, err := ReadConfig(filepath.Join(real, "config.toml"), 100); err != nil || string(b) != "x = 1\n" {
		t.Errorf("ReadConfig = %q, %v", b, err)
	}
	if _, err := ReadConfig(filepath.Join(base, "missing", "config.toml"), 100); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing dir = %v, want ErrNotExist", err)
	}
}

// TestReadConfigDirSwappedAfterCheck: a directory replaced by a symlink
// between the check and the open is refused.
func TestReadConfigDirSwappedAfterCheck(t *testing.T) {
	fakeHome(t)
	base := t.TempDir()
	dir := filepath.Join(base, ".aggy")
	must(t, os.MkdirAll(dir, 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("a"), 0o600))
	other := filepath.Join(base, "other")
	must(t, os.MkdirAll(other, 0o700))
	must(t, os.WriteFile(filepath.Join(other, "config.toml"), []byte("b"), 0o600))
	// The first Lstat checks the directory; swap it right after, at the
	// second one (the file's own Lstat).
	lstats := 0
	testHookAfterResolve = func(op string) {
		if op == "Lstat" {
			lstats++
		}
		if op == "Lstat" && lstats == 2 {
			must(t, os.Rename(dir, filepath.Join(base, "moved")))
			must(t, os.Symlink(other, dir))
		}
	}
	t.Cleanup(func() { testHookAfterResolve = nil })
	b, err := ReadConfig(filepath.Join(dir, "config.toml"), 100)
	if !errors.Is(err, ErrChanged) {
		t.Errorf("ReadConfig after a swap = %q, %v, want ErrChanged", b, err)
	}
}

// TestCanonicalPath: spellings of one file share one canonical path.
func TestCanonicalPath(t *testing.T) {
	fakeHome(t)
	base := t.TempDir()
	a := filepath.Join(base, "a")
	must(t, os.MkdirAll(a, 0o700))
	must(t, os.Symlink(a, filepath.Join(base, "link")))
	c1, err := CanonicalPath(filepath.Join(a, "c.toml"))
	if err != nil {
		t.Fatal(err)
	}
	c2, err := CanonicalPath(filepath.Join(base, "link", "c.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Errorf("CanonicalPath differs: %s vs %s", c1, c2)
	}
	old := foldCase
	foldCase = true
	t.Cleanup(func() { foldCase = old })
	c3, _ := CanonicalPath(filepath.Join(a, "C.TOML"))
	c4, _ := CanonicalPath(filepath.Join(a, "c.toml"))
	if c3 != c4 {
		t.Errorf("folded CanonicalPath differs: %s vs %s", c3, c4)
	}
}
