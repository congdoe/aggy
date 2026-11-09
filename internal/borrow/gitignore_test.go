package borrow

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestOutputStaysOutOfGit (T6926): after a research borrow and an image
// borrow in a real git repo, git status shows nothing under .aggy: the
// research and assets folders keep a .gitignore of "*", like the briefs
// folder, and the repo's own .gitignore and .git/info/exclude are left
// alone.
func TestOutputStaysOutOfGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	w := newWorld(t)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = w.repo
		cmd.Env = []string{"HOME=" + w.home, "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %q: %v\n%s", args, err, out)
		}
		return string(out)
	}
	if err := os.Remove(filepath.Join(w.repo, ".git")); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	exclude := filepath.Join(w.repo, ".git", "info", "exclude")
	excludeBefore, _ := os.ReadFile(exclude)

	w.scenario[adapter.Grok] = `{"json":{"result":"## Summary\nGo 1.27 shipped.\n## Sources\n- Go blog, https://go.dev/blog (2026-08-12)"}}`
	res, err := w.borrower().Run(context.Background(), w.req(adapter.Research, "What shipped in Go 1.27?"))
	if err != nil || len(res) != 1 || res[0].ResearchPath == "" {
		t.Fatalf("research: %+v, %v", res, err)
	}
	w.scenario[adapter.Codex] = `{"images":1}`
	req := w.req(adapter.Image, "a hero image")
	req.Vendor = string(adapter.Codex)
	req.Media = &Media{Out: filepath.Join(w.repo, "public", "hero.png")}
	if res, err = w.borrower().Run(context.Background(), req); err != nil || len(res) != 1 || res[0].Status != "done" {
		t.Fatalf("image: %+v, %v", res, err)
	}
	if len(res[0].Warnings) != 0 {
		t.Errorf("warnings: %q", res[0].Warnings)
	}
	for _, f := range []string{"research", "assets"} {
		if _, err := os.Stat(filepath.Join(w.repo, ".aggy", f)); err != nil {
			t.Fatalf("no .aggy/%s: %v", f, err)
		}
	}
	st := git("status", "--porcelain", "--untracked-files=all")
	for _, l := range strings.Split(st, "\n") {
		if strings.Contains(l, ".aggy") {
			t.Errorf("git status shows %q:\n%s", l, st)
		}
	}
	// The image itself is the user's: git sees it.
	if !strings.Contains(st, "public/hero.png") {
		t.Errorf("git status lacks the image:\n%s", st)
	}
	if after, _ := os.ReadFile(exclude); string(after) != string(excludeBefore) {
		t.Errorf(".git/info/exclude changed:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(w.repo, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("the repo's .gitignore was made: %v", err)
	}
}

// TestOutputIgnoreKept (T6926): a research folder's .gitignore that is
// not "*" is kept, and the borrow warns.
func TestOutputIgnoreKept(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.repo, ".aggy", "research")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("!*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.scenario[adapter.Grok] = `{"json":{"result":"## Summary\nok\n## Sources\n- x"}}`
	res, err := w.borrower().Run(context.Background(), w.req(adapter.Research, "q"))
	if err != nil || len(res) != 1 || res[0].ResearchPath == "" {
		t.Fatalf("research: %+v, %v", res, err)
	}
	if len(res[0].Warnings) != 1 || !strings.Contains(res[0].Warnings[0], `is not "*"`) {
		t.Errorf("warnings = %q", res[0].Warnings)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); string(b) != "!*\n" {
		t.Errorf(".gitignore = %q, want it kept", b)
	}
}
