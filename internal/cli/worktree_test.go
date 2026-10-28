package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/store"
)

// editRepo is gitRepo with every program the repo could make aggy run
// pointed at a marker: fsmonitor, textconv, diff.external (gitRepo), a
// filter driver its attributes name, and a post-checkout hook. It
// returns the markers.
func editRepo(t *testing.T, dir string) []string {
	t.Helper()
	markers := []string{gitRepo(t, dir)}
	filter := filepath.Join(t.TempDir(), "filter")
	hook := filepath.Join(t.TempDir(), "hook")
	markers = append(markers, filter, hook)
	gitIn(t, dir, "config", "filter.evil.smudge", "sh -c 'touch "+filter+"; cat'")
	gitIn(t, dir, "config", "filter.evil.clean", "sh -c 'touch "+filter+"; cat'")
	gitIn(t, dir, "config", "filter.evil.required", "true")
	mustWrite(t, filepath.Join(dir, ".git", "info", "attributes"), "* filter=evil\n", 0o600)
	mustWrite(t, filepath.Join(dir, ".git", "hooks", "post-checkout"), "#!/bin/sh\ntouch "+hook+"\n", 0o755)
	return markers
}

// gitIn runs the test's own git in dir, with filters switched off so the
// setup itself runs nothing.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "core.hooksPath=/dev/null", "-c", "filter.evil.clean=cat", "-c", "filter.evil.smudge=cat"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func noMarkers(t *testing.T, markers []string) {
	t.Helper()
	for _, m := range markers {
		if _, err := os.Stat(m); err == nil {
			t.Errorf("a program from the repo ran (%s)", m)
		}
	}
}

func newWorktrees(t *testing.T) (*gitWorktrees, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	state, _ := filepath.EvalSymlinks(t.TempDir())
	return &gitWorktrees{env: gitEnv(), stateDir: state, out: io.Discard}, state
}

// TestWorktreeCreateRemove (T6687): an edit worktree is made under the
// state dir on aggy's own branch from HEAD, and removed with git, branch
// and all; no hook, filter, or fsmonitor from the repo runs either way.
func TestWorktreeCreateRemove(t *testing.T) {
	g, state := newWorktrees(t)
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	markers := editRepo(t, repo)
	head := gitIn(t, repo, "rev-parse", "HEAD")
	ctx := context.Background()
	wt, err := g.Create(ctx, repo, "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if wt.Path != filepath.Join(state, worktreesDir, "0123456789abcdef") || wt.Branch != "aggy/edit-0123456789abcdef" ||
		wt.Base != head || wt.Repo != repo || wt.State != store.WorktreePending || !strings.HasPrefix(wt.GitDir, filepath.Join(repo, ".git", "worktrees")) {
		t.Errorf("worktree %+v", wt)
	}
	// HEAD's content, not the uncommitted change in the main checkout.
	if data, err := os.ReadFile(filepath.Join(wt.Path, "a.go")); err != nil || strings.Contains(string(data), "Uncommitted") {
		t.Errorf("a.go = %q, %v", data, err)
	}
	if b := gitIn(t, repo, "branch", "--list", wt.Branch); b == "" {
		t.Error("no branch")
	}
	if err := g.Remove(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("worktree left: %v", err)
	}
	if b := gitIn(t, repo, "branch", "--list", wt.Branch); b != "" {
		t.Errorf("branch left: %q", b)
	}
	noMarkers(t, markers)
	// A directory already gone is pruned, and its branch still deleted.
	wt, err = g.Create(ctx, repo, "1123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wt.Path); err != nil {
		t.Fatal(err)
	}
	if err := g.Remove(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if b := gitIn(t, repo, "branch", "--list", wt.Branch); b != "" {
		t.Errorf("branch left: %q", b)
	}
}

// TestWorktreeRemoveOwned (T6687): Remove acts only on a path and branch
// aggy made, whatever the record says.
func TestWorktreeRemoveOwned(t *testing.T) {
	g, state := newWorktrees(t)
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	gitRepo(t, repo)
	other := t.TempDir()
	tests := []store.Worktree{
		{Path: other, Branch: "aggy/edit-0123456789abcdef", Repo: repo},
		{Path: filepath.Join(state, worktreesDir, "0123456789abcdef"), Branch: "main", Repo: repo},
		{Path: filepath.Join(state, worktreesDir, "0123456789abcdef"), Branch: "aggy/edit-1123456789abcdef", Repo: repo},
		{Path: filepath.Join(state, worktreesDir, "..", "x"), Branch: "aggy/edit-x", Repo: repo},
	}
	for _, wt := range tests {
		if err := g.Remove(context.Background(), wt); err == nil || !strings.Contains(err.Error(), "not a worktree aggy made") {
			t.Errorf("%+v: %v", wt, err)
		}
	}
	if b := gitIn(t, repo, "branch", "--list", "main"); b == "" {
		t.Error("main was deleted")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error(err)
	}
}

// TestBorrowEditCommand (T6687): aggy borrow edit runs Codex in its own
// worktree with its workspace-write sandbox, tells it paths relative to
// the worktree, records the worktree pending, and says how to accept or
// reject it. Claude and Grok are refused: their edit modes are unverified.
func TestBorrowEditCommand(t *testing.T) {
	f := newBorrowFixture(t, adapter.Codex, adapter.Grok, adapter.Claude)
	markers := editRepo(t, f.repo)
	f.env.gitPath, f.env.gitRun = gitEnv().gitPath, gitEnv().gitRun
	f.scenario["codex"] = `{"events":[{"type":"item.completed","item":{"type":"agent_message","text":"Added Helper to a.go."}}]}`
	code, out, errOut := f.run(t, "edit", "add a helper", "--path", "a.go", "--vendor", "codex")
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	var child fakecli.Record
	for _, r := range fakecli.Records(t, f.record) {
		if r.Mode == "headless" {
			child = r
		}
	}
	wtRoot := resolvePath(filepath.Join(f.state, worktreesDir))
	if !strings.HasPrefix(child.Cwd, wtRoot+string(filepath.Separator)) || !strings.Contains(strings.Join(child.Args, " "), "--sandbox workspace-write") {
		t.Errorf("child %+v", child)
	}
	if !strings.Contains(child.Stdin, "- a.go\n") || strings.Contains(child.Stdin, f.repo) {
		t.Errorf("prompt paths:\n%s", child.Stdin)
	}
	id := filepath.Base(child.Cwd)
	for _, w := range []string{"edit on codex", "branch aggy/edit-" + id, "aggy accept " + id, "aggy reject " + id} {
		if !strings.Contains(out, w) {
			t.Errorf("stdout lacks %q:\n%s", w, out)
		}
	}
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	wt, err := s.BorrowWorktree(context.Background(), id)
	if err != nil || wt.Path != child.Cwd || wt.State != store.WorktreePending || wt.Repo != f.repo || wt.GitDir == "" {
		t.Errorf("stored %+v, %v", wt, err)
	}
	noMarkers(t, markers)

	for _, v := range []string{"grok", "claude"} {
		code, out, errOut = f.run(t, "edit", "add a helper", "--vendor", v)
		if code != 1 || !strings.Contains(errOut, "unverified") {
			t.Errorf("%s: code %d: %s %s", v, code, out, errOut)
		}
	}
	entries, _ := os.ReadDir(wtRoot)
	if len(entries) != 1 {
		t.Errorf("worktrees: %v", entries)
	}
}
