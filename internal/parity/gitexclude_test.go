package parity

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/store"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("GROK_HOME", "")
}

// excludeCases are git exclude files as git's template and people leave
// them.
var excludeCases = []struct {
	name string
	in   []byte
}{
	{"exclude_missing", nil},
	{"exclude_empty", []byte("")},
	{"exclude_template", []byte("# git ls-files --others --exclude-from=.git/info/exclude\n# Lines that start with '#' are comments.\n# For a project mostly in C, the following would be a good set of\n# exclude patterns (uncomment them if you want to use them):\n# *.[oa]\n# *~\n")},
	{"exclude_crlf_no_trailing_newline", []byte("*.log\r\nbuild/")},
	{"exclude_no_trailing_newline", []byte("*.swp")},
}

func TestExcludeGolden(t *testing.T) {
	for _, tc := range excludeCases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := AddExcludeLine(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tc.name, tc.in, out)
			if again, _ := AddExcludeLine(out); !bytes.Equal(again, out) {
				t.Errorf("not idempotent: %q", again)
			}
		})
	}
}

func TestExcludeAlreadyThere(t *testing.T) {
	for _, in := range []string{".aggy/\n", "x\r\n/.aggy/\r\n", "  .aggy  \n", "/.aggy"} {
		if out, _ := AddExcludeLine([]byte(in)); string(out) != in {
			t.Errorf("AddExcludeLine(%q) = %q, want it unchanged", in, out)
		}
	}
}

func TestGitExcludePath(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	// A plain repo.
	main := filepath.Join(base, "main")
	must(t, os.MkdirAll(filepath.Join(main, ".git"), 0o755))
	// A linked worktree: .git is a file, and the worktree's git dir names
	// the common dir.
	wtGit := filepath.Join(main, ".git", "worktrees", "wt")
	must(t, os.MkdirAll(wtGit, 0o755))
	must(t, os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o644))
	wt := filepath.Join(base, "wt")
	must(t, os.MkdirAll(wt, 0o755))
	must(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGit+"\r\n"), 0o644))
	// A submodule with a relative gitdir and no commondir.
	modGit := filepath.Join(main, ".git", "modules", "sub")
	must(t, os.MkdirAll(modGit, 0o755))
	sub := filepath.Join(main, "sub")
	must(t, os.MkdirAll(sub, 0o755))
	must(t, os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: ../.git/modules/sub\n"), 0o644))
	// A .git symlink to a directory.
	linked := filepath.Join(base, "linked")
	must(t, os.MkdirAll(linked, 0o755))
	must(t, os.Symlink(filepath.Join(main, ".git"), filepath.Join(linked, ".git")))

	for _, tc := range []struct{ root, want string }{
		{main, filepath.Join(main, ".git", "info", "exclude")},
		{wt, filepath.Join(main, ".git", "info", "exclude")},
		{sub, filepath.Join(modGit, "info", "exclude")},
		{linked, filepath.Join(linked, ".git", "info", "exclude")},
	} {
		got, err := GitExcludePath(tc.root)
		if err != nil || got != tc.want {
			t.Errorf("GitExcludePath(%s) = %s, %v, want %s", tc.root, got, err, tc.want)
		}
	}
	if _, err := GitExcludePath(base); err == nil {
		t.Error("a directory without .git was accepted")
	}
	bad := filepath.Join(base, "bad")
	must(t, os.MkdirAll(bad, 0o755))
	must(t, os.WriteFile(filepath.Join(bad, ".git"), []byte("nonsense\n"), 0o644))
	if _, err := GitExcludePath(bad); err == nil {
		t.Error("a .git file without gitdir was accepted")
	}
}

// TestGitExcludeThroughManifest: the line goes in through the manifest,
// a second run changes nothing, and unwire removes the file and the info
// directory aggy created.
func TestGitExcludeThroughManifest(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	repo := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	s, err := store.OpenDefault(ctx)
	must(t, err)
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	must(t, err)
	for range 2 {
		p, err := GitExcludePatch(repo)
		must(t, err)
		_, err = m.Apply(ctx, "setup", p)
		must(t, err)
	}
	exclude := filepath.Join(repo, ".git", "info", "exclude")
	if b, _ := os.ReadFile(exclude); string(b) != ".aggy/\n" {
		t.Errorf("exclude = %q", b)
	}
	if _, err := os.Lstat(filepath.Join(repo, ".gitignore")); err == nil {
		t.Error(".gitignore was created")
	}
	undos, err := m.Unwire(ctx, manifest.Selection{All: true}, manifest.UnwireOptions{}, io.Discard)
	if err != nil || len(undos) != 1 {
		t.Fatalf("Unwire = %+v, %v", undos, err)
	}
	if _, err := os.Lstat(filepath.Join(repo, ".git", "info")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("info dir left behind: %v", err)
	}
}
