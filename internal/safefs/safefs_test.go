package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeHome points HOME at a temp dir and clears the relocation variables,
// so no test can reach the real credential files.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGGY_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("GROK_HOME", "")
	return home
}

// writeFake creates a fake credential file with dummy content, using os
// directly because safefs would refuse it.
func writeFake(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ops runs every safefs entry point on path.
func ops(path string) map[string]error {
	errs := map[string]error{}
	_, errs["Open"] = Open(path)
	_, errs["OpenFile"] = OpenFile(path, os.O_RDONLY, 0)
	_, errs["ReadFile"] = ReadFile(path)
	errs["WriteFile"] = WriteFile(path, []byte("x"), 0o600)
	_, errs["Stat"] = Stat(path)
	_, errs["Lstat"] = Lstat(path)
	_, errs["ReadDir"] = ReadDir(path)
	errs["MkdirAll"] = MkdirAll(path, 0o700)
	errs["Remove"] = Remove(path)
	errs["Chmod"] = Chmod(path, 0o600)
	errs["Check"] = Check(path)
	return errs
}

func assertRefused(t *testing.T, path string) {
	t.Helper()
	for op, err := range ops(path) {
		if !errors.Is(err, ErrAuthPath) {
			t.Errorf("%s(%s) = %v, want ErrAuthPath", op, path, err)
		}
		var ape *AuthPathError
		if !errors.As(err, &ape) {
			t.Errorf("%s(%s) error %T is not *AuthPathError", op, path, err)
		}
	}
}

func TestRefusesAuthPaths(t *testing.T) {
	tests := []struct {
		name string
		env  string // relocation variable to set to a temp dir, if any
		path func(home, reloc string) string
	}{
		{"claude", "", func(h, _ string) string { return filepath.Join(h, ".claude", ".credentials.json") }},
		{"codex", "", func(h, _ string) string { return filepath.Join(h, ".codex", "auth.json") }},
		{"grok", "", func(h, _ string) string { return filepath.Join(h, ".grok", "auth.json") }},
		{"claude relocated", "CLAUDE_CONFIG_DIR", func(_, r string) string { return filepath.Join(r, ".credentials.json") }},
		{"codex relocated", "CODEX_HOME", func(_, r string) string { return filepath.Join(r, "auth.json") }},
		{"grok relocated", "GROK_HOME", func(_, r string) string { return filepath.Join(r, "auth.json") }},
		{"unclean path", "", func(h, _ string) string { return h + "/.codex/../.codex//./auth.json" }},
		{"codex mcp", "", func(h, _ string) string { return filepath.Join(h, ".codex", ".credentials.json") }},
		{"codex mcp relocated", "CODEX_HOME", func(_, r string) string { return filepath.Join(r, ".credentials.json") }},
		{"grok mcp", "", func(h, _ string) string { return filepath.Join(h, ".grok", "mcp_credentials.json") }},
		{"grok mcp relocated", "GROK_HOME", func(_, r string) string { return filepath.Join(r, "mcp_credentials.json") }},
	}
	for _, tt := range tests {
		for _, exists := range []bool{false, true} {
			name := tt.name
			if exists {
				name += " existing"
			}
			t.Run(name, func(t *testing.T) {
				home := fakeHome(t)
				reloc := ""
				if tt.env != "" {
					reloc = t.TempDir()
					t.Setenv(tt.env, reloc)
				}
				p := tt.path(home, reloc)
				if exists {
					writeFake(t, p)
				}
				assertRefused(t, p)
				if exists {
					if b, err := os.ReadFile(p); err != nil || string(b) != "fake" {
						t.Errorf("credential file changed: %q, %v", b, err)
					}
				}
			})
		}
	}
}

func TestRefusesRelativePath(t *testing.T) {
	home := fakeHome(t)
	t.Chdir(filepath.Join(home))
	assertRefused(t, ".codex/auth.json")
}

func TestRefusesSymlinks(t *testing.T) {
	home := fakeHome(t)
	auth := filepath.Join(home, ".claude", ".credentials.json")
	writeFake(t, auth)
	other := t.TempDir()

	fileLink := filepath.Join(other, "innocent.json")
	if err := os.Symlink(auth, fileLink); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(other, "dir")
	if err := os.Symlink(filepath.Join(home, ".claude"), dirLink); err != nil {
		t.Fatal(err)
	}
	chain := filepath.Join(other, "chain")
	if err := os.Symlink(fileLink, chain); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{fileLink, filepath.Join(dirLink, ".credentials.json"), chain} {
		t.Run(filepath.Base(p), func(t *testing.T) { assertRefused(t, p) })
	}
	if _, err := os.Lstat(fileLink); err != nil {
		t.Errorf("link was removed: %v", err)
	}
}

func TestRefusesLinkedVendorDir(t *testing.T) {
	// ~/.grok is a symlink to a directory elsewhere. The credential file at
	// its real location is refused.
	home := fakeHome(t)
	realDir := t.TempDir()
	if err := os.Symlink(realDir, filepath.Join(home, ".grok")); err != nil {
		t.Fatal(err)
	}
	writeFake(t, filepath.Join(realDir, "auth.json"))
	assertRefused(t, filepath.Join(realDir, "auth.json"))
	assertRefused(t, filepath.Join(home, ".grok", "auth.json"))
}

func TestCaseVariants(t *testing.T) {
	variants := []string{
		filepath.Join(".Claude", ".credentials.json"),
		filepath.Join(".claude", ".CREDENTIALS.JSON"),
		filepath.Join(".CODEX", "Auth.json"),
		filepath.Join(".Grok", "AUTH.JSON"),
	}
	for _, fold := range []bool{true, false} {
		for _, v := range variants {
			t.Run(v, func(t *testing.T) {
				old := foldCase
				foldCase = fold
				t.Cleanup(func() { foldCase = old })
				home := fakeHome(t)
				err := Check(filepath.Join(home, v))
				if fold && !errors.Is(err, ErrAuthPath) {
					t.Errorf("fold: Check = %v, want ErrAuthPath", err)
				}
				if !fold && err != nil {
					t.Errorf("no fold: Check = %v, want nil", err)
				}
			})
		}
	}
	if runtime.GOOS == "darwin" && !foldCase {
		t.Error("darwin must compare paths case-insensitively")
	}
}

func TestAllowsNormalPaths(t *testing.T) {
	home := fakeHome(t)
	allowed := []string{
		filepath.Join(home, ".aggy", "config.toml"),
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(home, ".grok", "auth.json.bak"),
		filepath.Join(home, "auth.json"),
		filepath.Join(home, "proj", ".claude", ".credentials.json"),
	}
	for _, p := range allowed {
		t.Run(strings.TrimPrefix(p, home), func(t *testing.T) {
			if err := MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := WriteFile(p, []byte("hello"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			got, err := ReadFile(p)
			if err != nil || string(got) != "hello" {
				t.Fatalf("ReadFile = %q, %v", got, err)
			}
			if _, err := Stat(p); err != nil {
				t.Errorf("Stat: %v", err)
			}
			f, err := Open(p)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			_ = f.Close()
			if _, err := ReadDir(filepath.Dir(p)); err != nil {
				t.Errorf("ReadDir: %v", err)
			}
			if err := Remove(p); err != nil {
				t.Errorf("Remove: %v", err)
			}
		})
	}
}

func TestWriteFileAtomic(t *testing.T) {
	fakeHome(t)
	dir := t.TempDir()

	t.Run("new file gets perm", func(t *testing.T) {
		p := filepath.Join(dir, "new.txt")
		if err := WriteFile(p, []byte("a"), 0o640); err != nil {
			t.Fatal(err)
		}
		assertMode(t, p, 0o640)
	})

	t.Run("existing file keeps mode", func(t *testing.T) {
		p := filepath.Join(dir, "keep.txt")
		if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o604); err != nil {
			t.Fatal(err)
		}
		if err := WriteFile(p, []byte("new"), 0o777); err != nil {
			t.Fatal(err)
		}
		assertMode(t, p, 0o604)
		assertContent(t, p, "new")
	})

	t.Run("symlink kept and target replaced", func(t *testing.T) {
		target := filepath.Join(dir, "target.txt")
		link := filepath.Join(dir, "link.txt")
		if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := WriteFile(link, []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(link)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("link replaced: %v %v", fi, err)
		}
		assertContent(t, target, "new")
	})

	t.Run("no temp files left", func(t *testing.T) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".aggy-") {
				t.Errorf("temp file left: %s", e.Name())
			}
		}
	})

	t.Run("directory refused", func(t *testing.T) {
		if err := WriteFile(dir, []byte("x"), 0o600); err == nil {
			t.Error("WriteFile on a directory succeeded")
		}
	})
}

func assertMode(t *testing.T, p string, want fs.FileMode) {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("mode = %o, want %o", got, want)
	}
}

func assertContent(t *testing.T, p, want string) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Errorf("content = %q, want %q", b, want)
	}
}
