package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/execrun"
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
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "filter.evil.clean=", "-c", "filter.evil.smudge=", "-c", "filter.evil.required=false"}, args...)...)
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

// TestWorktreeCopy (T6688): only listed files that are ignored, regular,
// and reached through no symlink are copied into a new worktree; nothing
// is replaced.
func TestWorktreeCopy(t *testing.T) {
	g, _ := newWorktrees(t)
	var out strings.Builder
	g.out = &out
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	gitRepo(t, repo)
	mustWrite(t, filepath.Join(repo, ".gitignore"), "*.env\nlocal/\n", 0o600)
	gitIn(t, repo, "add", ".gitignore")
	gitIn(t, repo, "commit", "-q", "-m", "ignore")
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.env"), "OUTSIDE=1\n", 0o600)
	mustWrite(t, filepath.Join(repo, ".env"), "KEY=1\n", 0o600)
	mustWrite(t, filepath.Join(repo, "local", "deep", "x.env"), "DEEP=1\n", 0o600)
	mustWrite(t, filepath.Join(repo, "hard.env"), "HARD=1\n", 0o600)
	if err := os.Link(filepath.Join(repo, "hard.env"), filepath.Join(outside, "hard2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.env"), filepath.Join(repo, "link.env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "local", "out")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, ".aggy", "config.toml"),
		`[worktree]
copy = [".env", "local/deep/x.env", "link.env", "local/out/secret.env", "hard.env", "a.go", "missing.env"]
`, 0o600)
	wt, err := g.Create(context.Background(), repo, "2123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{".env": "KEY=1\n", "local/deep/x.env": "DEEP=1\n"} {
		if data, err := os.ReadFile(filepath.Join(wt.Path, rel)); err != nil || string(data) != want {
			t.Errorf("%s = %q, %v", rel, data, err)
		}
	}
	for _, rel := range []string{"link.env", "local/out", "hard.env"} {
		if _, err := os.Lstat(filepath.Join(wt.Path, rel)); err == nil {
			t.Errorf("%s was copied", rel)
		}
	}
	// a.go is tracked: the worktree keeps HEAD's, not the main checkout's.
	if data, _ := os.ReadFile(filepath.Join(wt.Path, "a.go")); strings.Contains(string(data), "Uncommitted") {
		t.Error("a.go was replaced")
	}
	for _, w := range []string{"link.env: not a regular file", "local/out/secret.env: local/out is not a real directory", "hard.env:", "a.go: not ignored", "missing.env: not in the main checkout"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("output lacks %q:\n%s", w, out.String())
		}
	}
}

// TestWorktreeSetup (T6688): the repo's setup command is shown, and runs
// only when the person's config allows that command for that repo; a
// failing setup fails the worktree, which is removed.
func TestWorktreeSetup(t *testing.T) {
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	setup := `[worktree]
setup = ["/bin/sh", "-c", "echo ran > setup.out; exit $FAIL"]
`
	cmd := []string{"/bin/sh", "-c", "echo ran > setup.out; exit $FAIL"}
	tests := []struct {
		name    string
		allow   []config.SetupApproval
		ran     bool
		wantOut string
	}{
		{"not allowed", nil, false, "was not run"},
		{"another command allowed", []config.SetupApproval{{Repo: repo, Command: []string{"/bin/sh", "-c", "true"}}}, false, "[[worktree.allow_setup]]"},
		{"another repo allowed", []config.SetupApproval{{Repo: t.TempDir(), Command: cmd}}, false, "was not run"},
		{"allowed", []config.SetupApproval{{Repo: repo, Command: cmd}}, true, "running the repo's worktree setup command"},
	}
	gitRepo(t, repo)
	mustWrite(t, filepath.Join(repo, ".gitignore"), "setup.out\n", 0o600)
	gitIn(t, repo, "add", ".gitignore")
	gitIn(t, repo, "commit", "-q", "-m", "ignore")
	mustWrite(t, filepath.Join(repo, ".aggy", "config.toml"), setup, 0o600)
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, _ := newWorktrees(t)
			g.env.setupRun = execrun.ExecRunner(time.Minute)
			var out strings.Builder
			g.out, g.allow = &out, config.UserWorktree{AllowSetup: tt.allow}
			wt, err := g.Create(context.Background(), repo, fmt.Sprintf("%016x", 0x3123456789abcd00+i))
			if err != nil {
				t.Fatal(err)
			}
			_, serr := os.Stat(filepath.Join(wt.Path, "setup.out"))
			if (serr == nil) != tt.ran || !strings.Contains(out.String(), tt.wantOut) || !strings.Contains(out.String(), `"/bin/sh" "-c"`) {
				t.Errorf("ran %v, output:\n%s", serr == nil, out.String())
			}
		})
	}
	t.Run("failing", func(t *testing.T) {
		g, state := newWorktrees(t)
		g.env.setupRun = execrun.ExecRunner(time.Minute)
		g.allow = config.UserWorktree{AllowSetup: []config.SetupApproval{{Repo: repo, Command: []string{"/bin/sh", "-c", "echo broke >&2; exit 3"}}}}
		mustWrite(t, filepath.Join(repo, ".aggy", "config.toml"), "[worktree]\nsetup = [\"/bin/sh\", \"-c\", \"echo broke >&2; exit 3\"]\n", 0o600)
		_, err := g.Create(context.Background(), repo, "4123456789abcdef")
		if err == nil || !strings.Contains(err.Error(), "exit 3: broke") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(filepath.Join(state, worktreesDir, "4123456789abcdef")); !os.IsNotExist(err) {
			t.Errorf("worktree left: %v", err)
		}
		if b := gitIn(t, repo, "branch", "--list", "aggy/edit-4123456789abcdef"); b != "" {
			t.Errorf("branch left: %q", b)
		}
	})
}

// editBorrow runs an edit borrow on the fake Codex and returns its id
// and worktree path.
func editBorrow(t *testing.T, f *borrowFixture) (string, string) {
	t.Helper()
	before := len(fakecli.Records(t, f.record))
	if code, out, errOut := f.run(t, "edit", "change a.go", "--vendor", "codex"); code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	recs := fakecli.Records(t, f.record)
	for _, r := range recs[before:] {
		if r.Mode == "headless" {
			return filepath.Base(r.Cwd), r.Cwd
		}
	}
	t.Fatal("no child ran")
	return "", ""
}

// editFixture is a borrow fixture on an edit repo, with git found once
// and PATH then pointed at an empty dir.
func editFixture(t *testing.T) (*borrowFixture, []string) {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	f := newBorrowFixture(t, adapter.Codex)
	markers := editRepo(t, f.repo)
	f.env.gitPath = func() (string, error) { return gitBin, nil }
	f.env.gitRun = execrun.ExecRunner(gitTimeout)
	f.scenario["codex"] = `{"events":[{"type":"item.completed","item":{"type":"agent_message","text":"done"}}]}`
	// PATH holds git alone, for the test's own git calls.
	bin := t.TempDir()
	if err := os.Symlink(gitBin, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return f, markers
}

func accept(f *borrowFixture, in string, tty bool, args ...string) (int, string, string) {
	var out, errb strings.Builder
	code := acceptWith(args, f.env, strings.NewReader(in), tty, &out, &errb)
	return code, out.String(), errb.String()
}

// TestAccept (T6689): accept shows the stat, applies only on y or
// --yes, refuses a main checkout with changes in the touched paths, and
// applies once; nothing from the repo's config runs.
func TestAccept(t *testing.T) {
	f, markers := editFixture(t)
	id, wt := editBorrow(t, f)
	mustWrite(t, filepath.Join(wt, "a.go"), "package a\n\nfunc Edited() {}\n", 0o600)
	mustWrite(t, filepath.Join(wt, "b.go"), "package a\n", 0o600)

	// The main checkout's a.go has an uncommitted change.
	if code, out, errOut := accept(f, "", false, id, "--yes"); code != 1 || !strings.Contains(errOut, "has changes in paths the diff touches (a.go)") {
		t.Errorf("dirty: code %d: %s %s", code, out, errOut)
	}
	gitIn(t, f.repo, "checkout", "--", "a.go")
	if code, out, _ := accept(f, "", false, id); code != 1 || !strings.Contains(out, "a.go") || !strings.Contains(out, "Not applied") {
		t.Errorf("no tty: code %d: %s", code, out)
	}
	if code, out, _ := accept(f, "n\n", true, id); code != 1 || !strings.Contains(out, "Not applied.") {
		t.Errorf("answered n: code %d: %s", code, out)
	}
	if data, _ := os.ReadFile(filepath.Join(f.repo, "a.go")); strings.Contains(string(data), "Edited") {
		t.Fatal("applied without a yes")
	}
	// The child can rewrite the worktree's .git file; aggy never reads it.
	evil := t.TempDir()
	gitIn(t, evil, "init", "-q")
	gitIn(t, evil, "config", "core.fsmonitor", "touch "+markers[0])
	mustWrite(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(evil, ".git")+"\n", 0o600)
	code, out, errOut := accept(f, "y\n", true, id[:8])
	if code != 0 || !strings.Contains(out, "2 files changed") {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	if data, _ := os.ReadFile(filepath.Join(f.repo, "a.go")); !strings.Contains(string(data), "Edited") {
		t.Errorf("a.go = %q", data)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "b.go")); err != nil {
		t.Error(err)
	}
	if code, _, errOut := accept(f, "", false, id, "--yes"); code != 1 || !strings.Contains(errOut, "already accepted") {
		t.Errorf("second accept: code %d: %s", code, errOut)
	}
	noMarkers(t, markers)
}

// TestAcceptRefuses (T6689): a diff that touches .git, a symlink, a
// submodule, or hook, config, or instruction files is never applied.
func TestAcceptRefuses(t *testing.T) {
	f, markers := editFixture(t)
	gitIn(t, f.repo, "checkout", "--", "a.go")
	before := gitIn(t, f.repo, "status", "--porcelain")
	tests := []struct {
		name string
		make func(wt string)
		want string
	}{
		{"claude settings", func(wt string) { mustWrite(t, filepath.Join(wt, ".claude", "settings.json"), "{}", 0o600) }, ".claude/settings.json"},
		{"nested grok hooks", func(wt string) { mustWrite(t, filepath.Join(wt, "sub", ".grok", "hooks", "h.json"), "{}", 0o600) }, "sub/.grok/hooks/h.json"},
		{"aggy config", func(wt string) { mustWrite(t, filepath.Join(wt, ".aggy", "config.toml"), "", 0o600) }, ".aggy/config.toml"},
		{"agents file", func(wt string) { mustWrite(t, filepath.Join(wt, "AGENTS.md"), "x", 0o600) }, "AGENTS.md"},
		{"gitattributes", func(wt string) { mustWrite(t, filepath.Join(wt, ".gitattributes"), "* filter=evil\n", 0o600) }, ".gitattributes"},
		{"symlink", func(wt string) {
			if err := os.Symlink("/etc", filepath.Join(wt, "etc")); err != nil {
				t.Fatal(err)
			}
		}, "symlink"},
		{"nested repo", func(wt string) {
			gitIn(t, wt, "init", "-q", "inner")
			mustWrite(t, filepath.Join(wt, "inner", "x"), "x", 0o600)
			gitIn(t, filepath.Join(wt, "inner"), "add", "x")
			gitIn(t, filepath.Join(wt, "inner"), "commit", "-q", "-m", "x")
		}, "submodule"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, wt := editBorrow(t, f)
			tt.make(wt)
			code, out, errOut := accept(f, "", false, id, "--yes")
			if code != 1 || !strings.Contains(errOut, "refusing") || !strings.Contains(errOut, tt.want) {
				t.Errorf("code %d: %s %s", code, out, errOut)
			}
		})
	}
	if st := gitIn(t, f.repo, "status", "--porcelain"); st != before {
		t.Errorf("main checkout changed:\n%s", st)
	}
	noMarkers(t, markers)
}

// TestReject (T6689): reject removes the worktree and aggy's branch with
// git, once, and leaves the main checkout alone.
func TestReject(t *testing.T) {
	f, markers := editFixture(t)
	id, wt := editBorrow(t, f)
	mustWrite(t, filepath.Join(wt, "a.go"), "package a\n// edited\n", 0o600)
	var out, errb strings.Builder
	if code := rejectWith([]string{id}, f.env, &out, &errb); code != 0 {
		t.Fatalf("code %d: %s %s", code, out.String(), errb.String())
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree left: %v", err)
	}
	if b := gitIn(t, f.repo, "branch", "--list", "aggy/edit-"+id); b != "" {
		t.Errorf("branch left: %q", b)
	}
	if data, _ := os.ReadFile(filepath.Join(f.repo, "a.go")); !strings.Contains(string(data), "Uncommitted") {
		t.Errorf("main a.go changed: %q", data)
	}
	errb.Reset()
	if code := rejectWith([]string{id}, f.env, &out, &errb); code != 1 || !strings.Contains(errb.String(), "already rejected") {
		t.Errorf("second reject: code %d: %s", code, errb.String())
	}
	if code, _, errOut := accept(f, "", false, id, "--yes"); code != 1 || !strings.Contains(errOut, "already rejected") {
		t.Errorf("accept after reject: code %d: %s", code, errOut)
	}
	noMarkers(t, markers)
}
