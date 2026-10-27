package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the PR #2 review. Each one bypassed the old guard.

const secret = "fake"

// TestDotDotAfterSymlink: /w/s links to ~/.codex/sessions, and
// /w/s/../auth.json cleans lexically to /w/auth.json but the kernel opens
// ~/.codex/auth.json.
func TestDotDotAfterSymlink(t *testing.T) {
	home := fakeHome(t)
	writeFake(t, filepath.Join(home, ".codex", "auth.json"))
	if err := os.MkdirAll(filepath.Join(home, ".codex", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := t.TempDir()
	if err := os.Symlink(filepath.Join(home, ".codex", "sessions"), filepath.Join(w, "s")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(w, "s") + "/../auth.json"
	if b, err := os.ReadFile(p); err != nil || string(b) != secret {
		t.Fatalf("setup: kernel does not reach the credential file: %q %v", b, err)
	}
	assertRefused(t, p)
}

// TestHardLinkRefused: a hard link to a credential file in an artifact
// directory has a harmless name but the same inode.
func TestHardLinkRefused(t *testing.T) {
	home := fakeHome(t)
	auth := filepath.Join(home, ".codex", "auth.json")
	writeFake(t, auth)
	images := filepath.Join(t.TempDir(), "images")
	if err := os.MkdirAll(images, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(images, "x.png")
	if err := os.Link(auth, link); err != nil {
		t.Skipf("hard links not supported here: %v", err)
	}
	assertRefused(t, link)
	if b, _ := os.ReadFile(auth); string(b) != secret {
		t.Error("credential file changed")
	}
}

// TestSymlinkSwapRace: a symlink flipped between a harmless file and a
// credential file after the path check must never yield the secret.
func TestSymlinkSwapRace(t *testing.T) {
	home := fakeHome(t)
	auth := filepath.Join(home, ".grok", "auth.json")
	writeFake(t, auth)
	dir := t.TempDir()
	benign := filepath.Join(dir, "benign.txt")
	if err := os.WriteFile(benign, []byte("benign"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(benign, link); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		tmp := filepath.Join(dir, "link.tmp")
		for i := 0; !stop.Load(); i++ {
			to := benign
			if i%2 == 1 {
				to = auth
			}
			_ = os.Remove(tmp)
			if os.Symlink(to, tmp) == nil {
				_ = os.Rename(tmp, link)
			}
		}
	}()
	defer func() { stop.Store(true); <-done }()

	// Run for a fixed time rather than a fixed count, so slow runners
	// (-race, CI) still get enough swaps to land in the window.
	var leaks, benignReads int
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		b, err := ReadFile(link)
		if err == nil && string(b) == secret {
			leaks++
		}
		if err == nil && string(b) == "benign" {
			benignReads++
		}
		if f, err := Open(link); err == nil {
			buf := make([]byte, 16)
			n, _ := f.Read(buf)
			if string(buf[:n]) == secret {
				leaks++
			}
			_ = f.Close()
		}
	}
	if leaks > 0 {
		t.Fatalf("credential read %d times through a swapped symlink", leaks)
	}
	if benignReads == 0 {
		t.Log("race never landed on the benign file; the swap loop may be too fast")
	}
}

// TestSwapAfterCheck replaces the checked file at the exact point between
// the path check and the open: once with a symlink to a credential file and
// once with a hard link to it. Neither may return the secret.
func TestSwapAfterCheck(t *testing.T) {
	tests := []struct {
		name  string
		plant func(auth, at string) error
	}{
		{"symlink", func(auth, at string) error { return os.Symlink(auth, at) }},
		{"hard link", func(auth, at string) error { return os.Link(auth, at) }},
	}
	for _, tt := range tests {
		for _, op := range []string{"ReadFile", "Open", "ReadDir"} {
			t.Run(tt.name+" "+op, func(t *testing.T) {
				home := fakeHome(t)
				auth := filepath.Join(home, ".codex", "auth.json")
				writeFake(t, auth)
				p := filepath.Join(t.TempDir(), "notes.txt")
				if err := os.WriteFile(p, []byte("benign"), 0o600); err != nil {
					t.Fatal(err)
				}
				testHookBeforeOpen = func() {
					tmp := p + ".tmp"
					if err := tt.plant(auth, tmp); err != nil {
						t.Error(err)
						return
					}
					if err := os.Rename(tmp, p); err != nil {
						t.Error(err)
					}
				}
				t.Cleanup(func() { testHookBeforeOpen = nil })

				var got []byte
				var err error
				switch op {
				case "ReadFile":
					got, err = ReadFile(p)
				case "Open":
					var f *os.File
					if f, err = Open(p); err == nil {
						buf := make([]byte, 16)
						n, _ := f.Read(buf)
						got = buf[:n]
						_ = f.Close()
					}
				case "ReadDir":
					_, err = ReadDir(p)
				}
				if string(got) == secret {
					t.Fatalf("%s returned the credential", op)
				}
				if err == nil {
					t.Fatalf("%s = %q, nil after a swap to the credential file, want an error", op, got)
				}
				if tt.name == "hard link" && !errors.Is(err, ErrAuthPath) {
					t.Errorf("%s = %v, want ErrAuthPath", op, err)
				}
			})
		}
	}
}

// TestUnicodeNormalization: APFS ignores normalization, so a home spelled
// in NFC is reachable through an NFD path that compares unequal as bytes.
func TestUnicodeNormalization(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("normalization-insensitive file names are an APFS behavior")
	}
	base := t.TempDir()
	home := filepath.Join(base, "José") // NFC
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("GROK_HOME", "")
	auth := filepath.Join(home, ".codex", "auth.json")
	writeFake(t, auth)
	nfd := filepath.Join(base, "José", ".codex", "auth.json")
	if b, err := os.ReadFile(nfd); err != nil || string(b) != secret {
		t.Skipf("file system is normalization-sensitive: %v", err)
	}
	assertRefused(t, nfd)
}

// TestFailClosedWithoutHome: with no absolute home the deny list would be
// empty or relative, so every call must fail.
func TestFailClosedWithoutHome(t *testing.T) {
	benign := filepath.Join(t.TempDir(), "benign.txt")
	if err := os.WriteFile(benign, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	altHome := t.TempDir()
	tests := []struct {
		name     string
		home     string
		lookup   string // what the user database returns
		env      map[string]string
		wantFail bool
	}{
		{name: "unset, no fallback", home: "", lookup: "", wantFail: true},
		{name: "relative, no fallback", home: "rel/home", lookup: "", wantFail: true},
		{name: "relative, relative fallback", home: "rel", lookup: "also/rel", wantFail: true},
		{name: "unset, absolute fallback", home: "", lookup: altHome},
		{name: "relative, absolute fallback", home: "rel", lookup: altHome},
		{name: "relative CODEX_HOME", home: altHome, env: map[string]string{"CODEX_HOME": "codex"}, wantFail: true},
		{name: "relative CLAUDE_CONFIG_DIR", home: altHome, env: map[string]string{"CLAUDE_CONFIG_DIR": "./c"}, wantFail: true},
		{name: "relative GROK_HOME", home: altHome, env: map[string]string{"GROK_HOME": "g"}, wantFail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeHome(t)
			t.Setenv("HOME", tt.home)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			old := lookupUserHome
			lookupUserHome = func() (string, error) {
				if tt.lookup == "" {
					return "", errors.New("no user database")
				}
				return tt.lookup, nil
			}
			t.Cleanup(func() { lookupUserHome = old })

			_, err := ReadFile(benign)
			if tt.wantFail {
				if !errors.Is(err, ErrLocate) {
					t.Fatalf("ReadFile = %v, want ErrLocate", err)
				}
				for op, err := range ops(benign) {
					if !errors.Is(err, ErrLocate) {
						t.Errorf("%s = %v, want ErrLocate", op, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadFile = %v, want success", err)
			}
			// The fallback home's credentials are refused.
			auth := filepath.Join(tt.lookup, ".claude", ".credentials.json")
			if err := Check(auth); !errors.Is(err, ErrAuthPath) {
				t.Errorf("Check(fallback credential) = %v, want ErrAuthPath", err)
			}
		})
	}
}

// TestErrorsNameCallerPath: errors report the path the caller gave.
func TestErrorsNameCallerPath(t *testing.T) {
	fakeHome(t)
	p := filepath.Join(t.TempDir(), "missing")
	_, err := ReadFile(p)
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), p) {
		t.Errorf("ReadFile = %v", err)
	}
}
