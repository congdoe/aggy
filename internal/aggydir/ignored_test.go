//go:build unix

package aggydir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestMkdirIgnored (T6926): the folder right below .aggy gets a .gitignore
// of "*"; one already there that is not "*" is kept with a warning, and a
// symlink there is refused.
func TestMkdirIgnored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tests := []struct {
		name    string
		plant   func(t *testing.T, top string)
		elems   []string
		want    string // the .gitignore after
		warn    bool
		errLink bool
	}{
		{"fresh", nil, []string{"research"}, "*\n", false, false},
		{"nested", nil, []string{"assets", "public", "img"}, "*\n", false, false},
		{"star kept", func(t *testing.T, top string) { write(t, filepath.Join(top, ".gitignore"), "*") }, []string{"assets"}, "*", false, false},
		{"other kept", func(t *testing.T, top string) { write(t, filepath.Join(top, ".gitignore"), "!*\n") }, []string{"assets", "x"}, "!*\n", true, false},
		{"symlink refused", func(t *testing.T, top string) {
			link(t, filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(top, ".gitignore"))
		}, []string{"research"}, "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			top := filepath.Join(repo, Name, tt.elems[0])
			if tt.plant != nil {
				if err := os.MkdirAll(top, 0o700); err != nil {
					t.Fatal(err)
				}
				tt.plant(t, top)
			}
			p, fi, warn, err := MkdirIgnored(repo, 0o700, tt.elems...)
			if tt.errLink {
				if !errors.Is(err, ErrLink) {
					t.Fatalf("err = %v, want ErrLink", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(append([]string{repo, Name}, tt.elems...)...); p != want || fi == nil || !fi.IsDir() {
				t.Errorf("path %s (%v), want %s", p, fi, want)
			}
			if (warn != "") != tt.warn {
				t.Errorf("warn = %q, want one: %v", warn, tt.warn)
			}
			if b, _ := os.ReadFile(filepath.Join(top, ".gitignore")); string(b) != tt.want {
				t.Errorf(".gitignore = %q, want %q", b, tt.want)
			}
			if _, err := os.Stat(filepath.Join(repo, ".gitignore")); !os.IsNotExist(err) {
				t.Errorf("the repo's .gitignore: %v", err)
			}
		})
	}
	if _, _, _, err := MkdirIgnored(t.TempDir(), 0o700); err == nil {
		t.Error("no folder: want an error")
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}
