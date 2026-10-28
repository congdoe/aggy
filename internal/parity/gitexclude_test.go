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

// gitDir makes dir look like a git dir: HEAD, and objects/ for a common
// dir.
func gitDir(t *testing.T, dir string, common bool) {
	t.Helper()
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	if common {
		must(t, os.MkdirAll(filepath.Join(dir, "objects"), 0o755))
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
	gitDir(t, filepath.Join(main, ".git"), true)
	// A linked worktree: .git is a file, and the worktree's git dir names
	// the common dir.
	wtGit := filepath.Join(main, ".git", "worktrees", "wt")
	gitDir(t, wtGit, false)
	must(t, os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o644))
	wt := filepath.Join(base, "wt")
	must(t, os.MkdirAll(wt, 0o755))
	must(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGit+"\r\n"), 0o644))
	// A submodule with a relative gitdir and no commondir.
	modGit := filepath.Join(main, ".git", "modules", "sub")
	gitDir(t, modGit, true)
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
	gitDir(t, filepath.Join(repo, ".git"), true)
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

// TestGitExcludeRefusesCraftedGitDirs (PR #6 finding 6): a crafted .git
// cannot make aggy append to ~/.bashrc or create info/exclude in ~/.ssh
// or ~/.codex. On a8b4c37 all three were planned.
func TestGitExcludeRefusesCraftedGitDirs(t *testing.T) {
	isolate(t)
	h := os.Getenv("HOME")
	must(t, os.MkdirAll(filepath.Join(h, ".ssh"), 0o700))
	must(t, os.MkdirAll(filepath.Join(h, ".codex"), 0o700))
	bashrc := filepath.Join(h, ".bashrc")
	must(t, os.WriteFile(bashrc, []byte("alias ll='ls -l'\n"), 0o644))
	base := t.TempDir()
	repo := func(name, dotgit string) string {
		r := filepath.Join(base, name)
		must(t, os.MkdirAll(r, 0o755))
		must(t, os.WriteFile(filepath.Join(r, ".git"), []byte("gitdir: "+dotgit+"\n"), 0o644))
		return r
	}
	// info is a symlink to HOME, so info/exclude would be ~/exclude; and
	// a git dir whose info/exclude is a symlink to ~/.bashrc.
	linkedInfo := filepath.Join(base, "linkedinfo")
	gitDir(t, linkedInfo, true)
	must(t, os.Symlink(h, filepath.Join(linkedInfo, "info")))
	linkedExclude := filepath.Join(base, "linkedexclude")
	gitDir(t, linkedExclude, true)
	must(t, os.MkdirAll(filepath.Join(linkedExclude, "info"), 0o755))
	must(t, os.Symlink(bashrc, filepath.Join(linkedExclude, "info", "exclude")))
	for _, root := range []string{
		repo("ssh", filepath.Join(h, ".ssh")),
		repo("codex", filepath.Join(h, ".codex")),
		repo("home", h),
		repo("li", linkedInfo),
		repo("le", linkedExclude),
	} {
		if p, err := GitExcludePatch(root); err == nil {
			t.Errorf("GitExcludePatch(%s) = %s, want a refusal", root, p.File)
		}
	}
	for _, p := range []string{filepath.Join(h, ".ssh", "info"), filepath.Join(h, ".codex", "info")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was created", p)
		}
	}
	if b, _ := os.ReadFile(bashrc); string(b) != "alias ll='ls -l'\n" {
		t.Errorf(".bashrc = %q", b)
	}
}

// TestGitMainCheckout (S63-11): only git's own worktree link names a
// main checkout; a crafted .git file or folder keys the work tree by its
// own root.
func TestGitMainCheckout(t *testing.T) {
	isolate(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	// other is a real repo whose settings a crafted link would borrow.
	other := filepath.Join(base, "other")
	gitDir(t, filepath.Join(other, ".git"), true)
	// A real linked worktree of main: wt/.git names
	// main/.git/worktrees/wt, which names wt/.git back.
	main := filepath.Join(base, "main")
	gitDir(t, filepath.Join(main, ".git"), true)
	wtGit := filepath.Join(main, ".git", "worktrees", "wt")
	gitDir(t, wtGit, false)
	wt := filepath.Join(base, "wt")
	must(t, os.MkdirAll(wt, 0o755))
	must(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(wtGit, "gitdir"), []byte(filepath.Join(wt, ".git")+"\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o644))

	crafted := func(name string, setup func(r string)) string {
		r := filepath.Join(base, name)
		must(t, os.MkdirAll(r, 0o755))
		setup(r)
		return r
	}
	// A: a gitdir: file naming other's own git dir (no back-link).
	a := crafted("a", func(r string) {
		must(t, os.WriteFile(filepath.Join(r, ".git"), []byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o644))
	})
	// B: a .git folder with HEAD and a commondir naming other's.
	b := crafted("b", func(r string) {
		gitDir(t, filepath.Join(r, ".git"), false)
		must(t, os.WriteFile(filepath.Join(r, ".git", "commondir"), []byte(filepath.Join(other, ".git")+"\n"), 0o644))
	})
	// C: a gitdir naming a worktree of other whose back-link names
	// another work tree.
	cGit := filepath.Join(other, ".git", "worktrees", "c")
	gitDir(t, cGit, false)
	must(t, os.WriteFile(filepath.Join(cGit, "commondir"), []byte("../..\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(cGit, "gitdir"), []byte(filepath.Join(base, "elsewhere", ".git")+"\n"), 0o644))
	c := crafted("c", func(r string) {
		must(t, os.WriteFile(filepath.Join(r, ".git"), []byte("gitdir: "+cGit+"\n"), 0o644))
	})
	// D: a back-link that matches, but a git dir outside the common
	// dir's worktrees folder.
	dGit := filepath.Join(base, "dgit")
	gitDir(t, dGit, false)
	must(t, os.WriteFile(filepath.Join(dGit, "commondir"), []byte(filepath.Join(other, ".git")+"\n"), 0o644))
	d := crafted("d", func(r string) {
		must(t, os.WriteFile(filepath.Join(r, ".git"), []byte("gitdir: "+dGit+"\n"), 0o644))
		must(t, os.WriteFile(filepath.Join(dGit, "gitdir"), []byte(filepath.Join(r, ".git")+"\n"), 0o644))
	})
	for _, k := range []struct{ root, want string }{
		{main, main}, {wt, main}, {a, a}, {b, b}, {c, c}, {d, d},
	} {
		got, err := GitMainCheckout(k.root)
		if err != nil || got != k.want {
			t.Errorf("GitMainCheckout(%s) = %q, %v; want %q", filepath.Base(k.root), got, err, k.want)
		}
	}
}
