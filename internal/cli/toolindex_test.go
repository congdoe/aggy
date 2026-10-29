package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/tooldef"
)

// fakeCodegraph puts a fake codegraph alone on PATH: init <dir> makes
// <dir>/.codegraph/codegraph.db, counts its runs in .codegraph/inits,
// and writes the environment it got to .codegraph/env; exit sets its
// exit code. It never runs the real codegraph.
func fakeCodegraph(t *testing.T, exit string) string {
	t.Helper()
	bin := t.TempDir()
	writeAt(t, filepath.Join(bin, "codegraph"), `#!/bin/sh
[ "$1" = init ] || exit 9
/bin/mkdir -p "$2/.codegraph" || exit 8
echo run >> "$2/.codegraph/inits"
/usr/bin/env > "$2/.codegraph/env"
printf db > "$2/.codegraph/codegraph.db"
exit `+exit+"\n", 0o755)
	// git too, for the test's own git calls; nothing else.
	if git, err := exec.LookPath("git"); err == nil {
		if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return bin
}

func lines(t *testing.T, p string) int {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

// TestCodegraphIndexOnFirstOn (T6711): aggy tool on codegraph --repo
// runs codegraph init in the main checkout the first time only, with an
// environment built from scratch that keeps the telemetry opt-out, and
// makes git ignore the index; a repo it is never turned on for, or that
// turns it off, is never indexed; a missing or failing codegraph refuses
// the switch and says how to install the pinned version.
func TestCodegraphIndexOnFirstOn(t *testing.T) {
	isolate(t)
	fakeCodegraph(t, "0")
	t.Setenv("DO_NOT_TRACK", "1")
	t.Setenv("AGGY_SECRET_TEST", "leak")
	repo := switchRepo(t, "")
	other := switchRepo(t, "")
	inits := filepath.Join(repo, ".codegraph", "inits")

	code, out, errOut := toolRun("", false, "on", "codegraph", "--repo", repo)
	if code != 0 || !strings.Contains(out, "Indexing") || lines(t, inits) != 1 {
		t.Fatalf("first on: %d %q %q, %d runs", code, out, errOut, lines(t, inits))
	}
	env, _ := os.ReadFile(filepath.Join(repo, ".codegraph", "env"))
	if !strings.Contains(string(env), "DO_NOT_TRACK=1") || strings.Contains(string(env), "AGGY_SECRET_TEST") {
		t.Errorf("init env: %s", env)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, ".codegraph", ".gitignore")); string(b) != "*\n" {
		t.Errorf(".gitignore = %q", b)
	}
	if s := gitIn(t, repo, "status", "--porcelain"); s != "" {
		t.Errorf("the index shows in git status: %q", s)
	}
	// Once indexed, a later on (another vendor) runs nothing.
	if code, _, errOut := toolRun("", false, "on", "codegraph", "--repo", repo, "--vendor", "codex"); code != 0 || lines(t, inits) != 1 {
		t.Fatalf("second on: %d %q, %d runs", code, errOut, lines(t, inits))
	}
	// Off runs nothing; a repo never turned on is never indexed.
	if code, _, errOut := toolRun("", false, "off", "codegraph", "--repo", other); code != 0 || exists(t, filepath.Join(other, ".codegraph")) {
		t.Fatalf("off: %d %q", code, errOut)
	}
	// A repo that turns it off refuses on before any init.
	writeAt(t, filepath.Join(other, ".aggy", "config.toml"), `tools_off = ["codegraph"]`, 0o600)
	if code, _, errOut := toolRun("", false, "on", "codegraph", "--repo", other); code != 1 || !strings.Contains(errOut, "tool.repo_off") || exists(t, filepath.Join(other, ".codegraph")) {
		t.Fatalf("repo off: %d %q", code, errOut)
	}

	// Missing: refused, nothing recorded, the pinned install named.
	third := switchRepo(t, "")
	t.Setenv("PATH", t.TempDir())
	code, _, errOut = toolRun("", false, "on", "codegraph", "--repo", third)
	if code != 1 || !strings.Contains(errOut, "tool.index_failed") || !strings.Contains(errOut, "npm install -g @colbymchenry/codegraph@1.6.2") {
		t.Fatalf("missing: %d %q", code, errOut)
	}
	if _, out, _ := toolRun("", false, "status", "--repo", third); !strings.HasPrefix(statusLine(t, out, "codegraph"), "codegraph off off off") {
		t.Errorf("recorded although refused: %s", out)
	}
	// Failing: refused, and nothing recorded.
	fakeCodegraph(t, "1")
	if code, _, errOut := toolRun("", false, "on", "codegraph", "--repo", third); code != 1 || !strings.Contains(errOut, "tool.index_failed") || !strings.Contains(errOut, "exit 1") {
		t.Fatalf("failing init: %d %q", code, errOut)
	}
	if _, out, _ := toolRun("", false, "status", "--repo", third); !strings.HasPrefix(statusLine(t, out, "codegraph"), "codegraph off off off") {
		t.Errorf("recorded although init failed: %s", out)
	}
}

// indexFixture is a main checkout with a codegraph index and a linked
// worktree of it, and codegraph's built-in state as on.
func indexFixture(t *testing.T) (toolRepoAt, switchboard.State) {
	t.Helper()
	isolate(t)
	main := switchRepo(t, "")
	writeAt(t, filepath.Join(main, ".codegraph", "codegraph.db"), "db", 0o600)
	writeAt(t, filepath.Join(main, ".codegraph", "codegraph.db-wal"), "wal", 0o600)
	writeAt(t, filepath.Join(main, ".codegraph", "daemon.pid"), "1", 0o600)
	writeAt(t, filepath.Join(main, ".codegraph", ".gitignore"), "*\n", 0o600)
	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, main, "worktree", "add", "-q", wt)
	repo, err := toolRepo(wt, true)
	if err != nil || repo.root == repo.main {
		t.Fatalf("toolRepo: %+v %v", repo, err)
	}
	r, _, err := tooldef.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	cg, _ := r.Get("codegraph")
	return repo, switchboard.State{Tool: cg, Vendor: adapter.Claude, On: true, Parts: cg.PartsFor(adapter.Claude)}
}

// TestCopyIndexes (T6711): a session in a linked worktree with
// codegraph on gets a copy of the main checkout's index (its database
// and WAL, never the server's pid file), ignored by git; with codegraph
// off, or not wired, it gets none; a symlink in the index, or an index
// over the cap, refuses the copy and leaves nothing behind.
func TestCopyIndexes(t *testing.T) {
	ctx := context.Background()
	idx := func(repo toolRepoAt) string { return filepath.Join(repo.root, ".codegraph") }

	t.Run("on", func(t *testing.T) {
		repo, st := indexFixture(t)
		if w := copyIndexes(ctx, []switchboard.State{st}, []string{"codegraph"}, repo); len(w) != 0 {
			t.Fatal(w)
		}
		if b, _ := os.ReadFile(filepath.Join(idx(repo), "codegraph.db")); string(b) != "db" {
			t.Errorf("db = %q", b)
		}
		if !exists(t, filepath.Join(idx(repo), "codegraph.db-wal")) || exists(t, filepath.Join(idx(repo), "daemon.pid")) {
			t.Error("want the WAL and no pid file")
		}
		if s := gitIn(t, repo.root, "status", "--porcelain"); s != "" {
			t.Errorf("the copy shows in git status: %q", s)
		}
		// A second launch leaves the worktree's own index alone.
		writeAt(t, filepath.Join(idx(repo), "codegraph.db"), "branch", 0o600)
		_ = copyIndexes(ctx, []switchboard.State{st}, []string{"codegraph"}, repo)
		if b, _ := os.ReadFile(filepath.Join(idx(repo), "codegraph.db")); string(b) != "branch" {
			t.Errorf("replaced: %q", b)
		}
	})
	t.Run("off", func(t *testing.T) {
		repo, st := indexFixture(t)
		_ = copyIndexes(ctx, nil, nil, repo)
		_ = copyIndexes(ctx, []switchboard.State{st}, nil, repo) // on, but not wired
		if exists(t, idx(repo)) {
			t.Error("copied while off")
		}
	})
	t.Run("main checkout", func(t *testing.T) {
		repo, st := indexFixture(t)
		repo.root = repo.main
		if w := copyIndexes(ctx, []switchboard.State{st}, []string{"codegraph"}, repo); len(w) != 0 {
			t.Fatal(w)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		repo, st := indexFixture(t)
		secret := filepath.Join(t.TempDir(), "secret")
		writeAt(t, secret, "s", 0o600)
		if err := os.Symlink(secret, filepath.Join(repo.main, ".codegraph", "extra")); err != nil {
			t.Fatal(err)
		}
		w := copyIndexes(ctx, []switchboard.State{st}, []string{"codegraph"}, repo)
		if len(w) != 1 || !strings.Contains(w[0], "symlink") || !strings.Contains(w[0], "tool.index_copy_refused") || exists(t, idx(repo)) {
			t.Errorf("warns %q; copy left: %v", w, exists(t, idx(repo)))
		}
	})
	t.Run("symlinked db", func(t *testing.T) {
		repo, st := indexFixture(t)
		db := filepath.Join(repo.main, ".codegraph", "codegraph.db")
		_ = os.Remove(db)
		if err := os.Symlink(filepath.Join(t.TempDir(), "x"), db); err != nil {
			t.Fatal(err)
		}
		if w := copyIndexes(ctx, []switchboard.State{st}, []string{"codegraph"}, repo); len(w) != 1 || exists(t, idx(repo)) {
			t.Errorf("warns %q", w)
		}
	})
	t.Run("symlinked folder", func(t *testing.T) {
		repo, st := indexFixture(t)
		dir := filepath.Join(repo.main, ".codegraph")
		moved := filepath.Join(t.TempDir(), "cg")
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, dir); err != nil {
			t.Fatal(err)
		}
		if w := copyIndexes(ctx, []switchboard.State{st}, []string{"codegraph"}, repo); len(w) != 1 || exists(t, idx(repo)) {
			t.Errorf("warns %q", w)
		}
	})
	t.Run("over the cap", func(t *testing.T) {
		repo, st := indexFixture(t)
		old := maxIndexCopy
		maxIndexCopy = 4 // db (2) and wal (3) bytes
		t.Cleanup(func() { maxIndexCopy = old })
		w := copyIndexes(ctx, []switchboard.State{st}, []string{"codegraph"}, repo)
		if len(w) != 1 || !strings.Contains(w[0], "over") || exists(t, idx(repo)) {
			t.Errorf("warns %q", w)
		}
	})
}

// TestBorrowEditGetsCodegraph (T6711): an edit child gets codegraph's
// server, and its worktree a copy of the main checkout's index before it
// starts, which never lands in the edit's diff; a review child gets no
// server.
func TestBorrowEditGetsCodegraph(t *testing.T) {
	f := toolsFixture(t, `["codegraph"]`)
	editRepo(t, f.repo)
	f.env.gitPath, f.env.gitRun = gitEnv().gitPath, gitEnv().gitRun
	mustWrite(t, filepath.Join(f.repo, ".codegraph", "codegraph.db"), "db", 0o600)
	mustWrite(t, filepath.Join(f.repo, ".codegraph", ".gitignore"), "*\n", 0o600)
	f.scenario["codex"] = `{"events":[{"type":"item.completed","item":{"type":"agent_message","text":"done"}}],"cwd_files":[{"path":"a.go","content":"package a\n// edited\n"}]}`
	if code, out, errOut := f.run(t, "edit", "add a helper", "--vendor", "codex"); code != 0 {
		t.Fatalf("edit: %d %s %s", code, out, errOut)
	}
	child := lastHeadless(t, f)
	if !strings.Contains(strings.Join(child.Args, " "), `mcp_servers.codegraph.args=["tool", "exec", `) {
		t.Errorf("edit child args: %v", child.Args)
	}
	if b, _ := os.ReadFile(filepath.Join(child.Cwd, ".codegraph", "codegraph.db")); string(b) != "db" {
		t.Errorf("the worktree has no copy: %q", b)
	}
	if s := gitIn(t, child.Cwd, "status", "--porcelain"); strings.Contains(s, ".codegraph") {
		t.Errorf("the copy is in the edit's diff: %q", s)
	}

	if code, out, errOut := f.run(t, "review", "look", "--vendor", "codex"); code != 0 {
		t.Fatalf("review: %d %s %s", code, out, errOut)
	}
	if args := strings.Join(lastHeadless(t, f).Args, " "); strings.Contains(args, "mcp_servers") {
		t.Errorf("review child got a server: %s", args)
	}
}

func lastHeadless(t *testing.T, f *borrowFixture) fakecli.Record {
	t.Helper()
	recs := fakecli.Records(t, f.record)
	slices.Reverse(recs)
	if i := slices.IndexFunc(recs, func(r fakecli.Record) bool { return r.Mode == "headless" }); i >= 0 {
		return recs[i]
	}
	t.Fatal("no child ran")
	return fakecli.Record{}
}

// TestPrebuildsHaveIndex (T6711): every built-in with a prebuild has an
// index folder aggy knows, or aggy tool on refuses it.
func TestPrebuildsHaveIndex(t *testing.T) {
	isolate(t)
	r, _, err := tooldef.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range r.List() {
		if _, ok := toolIndexes[tl.Name]; ok != (len(tl.Install.Prebuild) > 0) {
			t.Errorf("%s: prebuild %v, index known %v", tl.Name, tl.Install.Prebuild, ok)
		}
	}
}
