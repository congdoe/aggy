package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// Regression tests for the PR #4 review (T6723 S8-S11). Each fails on
// 522a431; S8 with the testHookAfterResolve call point added there.

// TestGuardCoversBothHomes (S11): a child that sets HOME elsewhere must not
// hide the credentials in the user database's home.
func TestGuardCoversBothHomes(t *testing.T) {
	fakeHome(t)
	real := t.TempDir()
	old := lookupUserHome
	lookupUserHome = func() (string, error) { return real, nil }
	t.Cleanup(func() { lookupUserHome = old })
	auth := filepath.Join(real, ".codex", "auth.json")
	writeFake(t, auth)
	assertRefused(t, auth)
	// A hard link to it is refused by identity, not only by link count.
	link := filepath.Join(t.TempDir(), "x.png")
	must(t, os.Link(auth, link))
	if _, err := Stat(link); !errors.Is(err, ErrAuthPath) {
		t.Errorf("Stat(hard link) = %v, want ErrAuthPath", err)
	}
}

// TestMissingCredentialSpellings (S10): a credential that does not exist
// yet has no identity, so the path itself must match in any Unicode
// normalization, and a path under it is refused too.
func TestMissingCredentialSpellings(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "José") // NFC
	must(t, os.MkdirAll(home, 0o700))
	fakeHome(t)
	t.Setenv("HOME", home)
	nfd := filepath.Join(base, "José", ".codex", "auth.json")
	if err := Check(nfd); !errors.Is(err, ErrAuthPath) {
		t.Errorf("Check(NFD spelling) = %v, want ErrAuthPath", err)
	}
	under := filepath.Join(home, ".grok", "auth.json", "x")
	if err := MkdirAll(under, 0o700); !errors.Is(err, ErrAuthPath) {
		t.Errorf("MkdirAll(under credential) = %v, want ErrAuthPath", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".grok", "auth.json")); err == nil {
		t.Error("a directory was created at the credential path")
	}
	if err := WriteFile(filepath.Join(home, ".claude", ".credentials.json", "y"), nil, 0o600); !errors.Is(err, ErrAuthPath) {
		t.Errorf("WriteFile(under credential) = %v, want ErrAuthPath", err)
	}
}

// TestSwapParentAfterResolve (S8): the parent's identity is taken when the
// path is checked, so a directory swapped for another directory between
// the check and the act is refused.
func TestSwapParentAfterResolve(t *testing.T) {
	fakeHome(t)
	w := t.TempDir()
	d := filepath.Join(w, "d")
	other := filepath.Join(w, "other")
	must(t, os.Mkdir(d, 0o700))
	must(t, os.Mkdir(other, 0o700))
	must(t, os.WriteFile(filepath.Join(d, "x"), []byte("mine"), 0o600))
	must(t, os.WriteFile(filepath.Join(other, "x"), []byte("keep"), 0o600))
	fired := false
	testHookAfterResolve = func(op string) {
		if op == "Remove" && !fired {
			fired = true
			must(t, os.Rename(d, d+".old"))
			must(t, os.Rename(other, d))
		}
	}
	t.Cleanup(func() { testHookAfterResolve = nil })
	if err := Remove(filepath.Join(d, "x")); !errors.Is(err, ErrChanged) {
		t.Errorf("Remove = %v, want ErrChanged", err)
	}
	if !fired {
		t.Fatal("hook never ran")
	}
	if b, err := os.ReadFile(filepath.Join(d, "x")); err != nil || string(b) != "keep" {
		t.Errorf("the swapped-in directory's file = %q, %v", b, err)
	}
}

// TestReadFileLimit (S9).
func TestReadFileLimit(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	ok := filepath.Join(dir, "config.toml")
	must(t, os.WriteFile(ok, []byte("a = 1\n"), 0o600))
	if b, err := ReadFileLimit(ok, 6); err != nil || string(b) != "a = 1\n" {
		t.Errorf("ReadFileLimit = %q, %v", b, err)
	}
	if _, err := ReadFileLimit(ok, 5); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over the limit = %v, want ErrTooLarge", err)
	}
	link := filepath.Join(dir, "link.toml")
	must(t, os.Symlink(ok, link))
	if _, err := ReadFileLimit(link, 100); !errors.Is(err, ErrNotRegular) {
		t.Errorf("symlink = %v, want ErrNotRegular", err)
	}
	if _, err := ReadFileLimit(dir, 100); !errors.Is(err, ErrNotRegular) {
		t.Errorf("directory = %v, want ErrNotRegular", err)
	}
	if runtime.GOOS != "windows" {
		fifo := filepath.Join(dir, "fifo")
		if err := syscall.Mkfifo(fifo, 0o600); err == nil {
			if _, err := ReadFileLimit(fifo, 100); !errors.Is(err, ErrNotRegular) {
				t.Errorf("fifo = %v, want ErrNotRegular", err)
			}
		}
	}
	if _, err := ReadFileLimit(filepath.Join(dir, "missing"), 1); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing = %v", err)
	}
}
