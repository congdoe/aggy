package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Tests for the T6723 hardening. Each swaps the file system in the window
// between an operation's checks and its action, through testHookBeforeAct,
// and each fails against the code before T6723 with the same hook point.

// setHook installs fn as testHookBeforeAct for op only, once.
func setHook(t *testing.T, op string, fn func()) {
	t.Helper()
	fired := false
	testHookBeforeAct = func(got string) {
		if got == op && !fired {
			fired = true
			fn()
		}
	}
	t.Cleanup(func() {
		testHookBeforeAct = nil
		if !fired {
			t.Errorf("hook for %s never ran", op)
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// swapDirForLink replaces directory d with a symlink to target.
func swapDirForLink(t *testing.T, d, target string) {
	t.Helper()
	must(t, os.Rename(d, d+".moved"))
	must(t, os.Symlink(target, d))
}

func TestHardLinkedFileRefused(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	must(t, os.WriteFile(a, []byte("data"), 0o600))
	if err := os.Link(a, b); err != nil {
		t.Skipf("hard links not supported here: %v", err)
	}
	for _, p := range []string{a, b} {
		if _, err := ReadFile(p); !errors.Is(err, ErrHardLink) {
			t.Errorf("ReadFile(%s) = %v, want ErrHardLink", filepath.Base(p), err)
		}
		if _, err := Open(p); !errors.Is(err, ErrHardLink) {
			t.Errorf("Open(%s) = %v, want ErrHardLink", filepath.Base(p), err)
		}
		if _, err := OpenFile(p, os.O_RDWR, 0); !errors.Is(err, ErrHardLink) {
			t.Errorf("OpenFile(%s) = %v, want ErrHardLink", filepath.Base(p), err)
		}
		if err := Chmod(p, 0o644); !errors.Is(err, ErrHardLink) {
			t.Errorf("Chmod(%s) = %v, want ErrHardLink", filepath.Base(p), err)
		}
	}
	assertMode(t, a, 0o600)
	// Metadata stays available, and removing one name makes the other
	// readable again.
	if _, err := Stat(a); err != nil {
		t.Errorf("Stat = %v", err)
	}
	must(t, Remove(b))
	if got, err := ReadFile(a); err != nil || string(got) != "data" {
		t.Errorf("ReadFile after unlink = %q, %v", got, err)
	}
}

// TestCredentialCreatedAfterGuardLoad: the credential appears after the
// deny list was loaded, so its identity is unknown, and is hard-linked to
// the name being read.
func TestCredentialCreatedAfterGuardLoad(t *testing.T) {
	home := fakeHome(t)
	auth := filepath.Join(home, ".codex", "auth.json")
	p := filepath.Join(t.TempDir(), "notes.txt")
	must(t, os.WriteFile(p, []byte("benign"), 0o600))
	setHook(t, "ReadFile", func() {
		writeFake(t, auth)
		must(t, os.Remove(p))
		must(t, os.Link(auth, p))
	})
	got, err := ReadFile(p)
	if string(got) == secret {
		t.Fatal("ReadFile returned the credential")
	}
	if !errors.Is(err, ErrHardLink) {
		t.Fatalf("ReadFile = %q, %v, want ErrHardLink", got, err)
	}
}

func TestChmodSwapToSymlink(t *testing.T) {
	home := fakeHome(t)
	auth := filepath.Join(home, ".grok", "auth.json")
	writeFake(t, auth)
	p := filepath.Join(t.TempDir(), "aggy.db")
	must(t, os.WriteFile(p, nil, 0o600))
	setHook(t, "Chmod", func() {
		must(t, os.Remove(p))
		must(t, os.Symlink(auth, p))
	})
	if err := Chmod(p, 0o644); err == nil {
		t.Error("Chmod followed a symlink swapped in after the check")
	}
	assertMode(t, auth, 0o600)
}

func TestChmodSwapToHardLink(t *testing.T) {
	home := fakeHome(t)
	auth := filepath.Join(home, ".grok", "auth.json")
	writeFake(t, auth)
	p := filepath.Join(t.TempDir(), "aggy.db")
	must(t, os.WriteFile(p, nil, 0o600))
	setHook(t, "Chmod", func() {
		must(t, os.Remove(p))
		must(t, os.Link(auth, p))
	})
	if err := Chmod(p, 0o644); !errors.Is(err, ErrAuthPath) {
		t.Errorf("Chmod = %v, want ErrAuthPath", err)
	}
	assertMode(t, auth, 0o600)
}

func TestStatSwapToSymlink(t *testing.T) {
	home := fakeHome(t)
	auth := filepath.Join(home, ".claude", ".credentials.json")
	p := filepath.Join(t.TempDir(), "settings.json")
	must(t, os.WriteFile(p, []byte("{}"), 0o600))
	setHook(t, "Stat", func() {
		// A credential created after the deny list was loaded: its
		// identity is unknown to the guard.
		writeFake(t, auth)
		must(t, os.Remove(p))
		must(t, os.Symlink(auth, p))
	})
	fi, err := Stat(p)
	if err == nil {
		t.Fatalf("Stat followed a symlink swapped in after the check: size %d", fi.Size())
	}
	if !errors.Is(err, ErrChanged) {
		t.Errorf("Stat = %v, want ErrChanged", err)
	}
}

func TestStatDanglingSymlinkIsNotExist(t *testing.T) {
	fakeHome(t)
	p := filepath.Join(t.TempDir(), "dangling")
	must(t, os.Symlink(filepath.Join(t.TempDir(), "missing"), p))
	if _, err := Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(dangling) = %v, want ErrNotExist", err)
	}
}

func TestMkdirAllSwapParent(t *testing.T) {
	home := fakeHome(t)
	codex := filepath.Join(home, ".codex")
	must(t, os.MkdirAll(codex, 0o700))
	d := filepath.Join(t.TempDir(), "d")
	must(t, os.Mkdir(d, 0o700))
	setHook(t, "MkdirAll", func() { swapDirForLink(t, d, codex) })
	if err := MkdirAll(filepath.Join(d, "sub", "deeper"), 0o700); !errors.Is(err, ErrChanged) {
		t.Errorf("MkdirAll = %v, want ErrChanged", err)
	}
	if _, err := os.Lstat(filepath.Join(codex, "sub")); err == nil {
		t.Error("MkdirAll created a directory inside the vendor directory")
	}
}

func TestWriteFileSwapParent(t *testing.T) {
	home := fakeHome(t)
	codex := filepath.Join(home, ".codex")
	auth := filepath.Join(codex, "auth.json")
	must(t, os.MkdirAll(codex, 0o700))
	d := filepath.Join(t.TempDir(), "d")
	must(t, os.Mkdir(d, 0o700))
	setHook(t, "WriteFile", func() {
		// The credential appears after the deny list was loaded, and the
		// directory is swapped for a link to its folder.
		writeFake(t, auth)
		swapDirForLink(t, d, codex)
	})
	err := WriteFile(filepath.Join(d, "auth.json"), []byte("attacker"), 0o600)
	if !errors.Is(err, ErrChanged) {
		t.Errorf("WriteFile = %v, want ErrChanged", err)
	}
	assertContent(t, auth, secret)
	entries, _ := os.ReadDir(codex)
	if len(entries) != 1 {
		t.Errorf("vendor directory has %d entries, want only auth.json", len(entries))
	}
}

func TestRemoveSwapParent(t *testing.T) {
	home := fakeHome(t)
	codex := filepath.Join(home, ".codex")
	auth := filepath.Join(codex, "auth.json")
	writeFake(t, auth)
	d := filepath.Join(t.TempDir(), "d")
	must(t, os.Mkdir(d, 0o700))
	must(t, os.WriteFile(filepath.Join(d, "auth.json"), []byte("mine"), 0o600))
	setHook(t, "Remove", func() { swapDirForLink(t, d, codex) })
	if err := Remove(filepath.Join(d, "auth.json")); !errors.Is(err, ErrChanged) {
		t.Errorf("Remove = %v, want ErrChanged", err)
	}
	assertContent(t, auth, secret)
}

func TestWriteFileDirSyncError(t *testing.T) {
	fakeHome(t)
	p := filepath.Join(t.TempDir(), "config.toml")
	must(t, os.WriteFile(p, []byte("old"), 0o600))
	syncErr := errors.New("disk on fire")
	testSyncDir = func() error { return syncErr }
	t.Cleanup(func() { testSyncDir = nil })

	err := WriteFile(p, []byte("new"), 0o600)
	if !errors.Is(err, ErrDirSync) || !errors.Is(err, syncErr) {
		t.Fatalf("WriteFile = %v, want ErrDirSync wrapping the sync error", err)
	}
	var dse *DirSyncError
	if !errors.As(err, &dse) || dse.Path != p {
		t.Errorf("error %v does not name %s", err, p)
	}
	// The rename happened: the result is not lost.
	assertContent(t, p, "new")
}
