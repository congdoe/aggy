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
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/tooldef"
)

// codegraphGitignore is the .codegraph/.gitignore codegraph 1.6.2's init
// writes (dist/directory.js, GITIGNORE_CONTENT): it leaves itself
// tracked.
const codegraphGitignore = `# CodeGraph data files — local to each machine, not for committing.
# Ignore everything in .codegraph/ except this file itself, so transient
# files (the database, daemon.pid, sockets, logs) never show up in git.
*
!.gitignore
`

// fakeCodegraph puts a fake codegraph alone on PATH (with git, for the
// test's own calls): init <dir> makes <dir>/.codegraph with codegraph's
// own .gitignore and codegraph.db, counts its runs in .codegraph/inits,
// and writes the environment it got to .codegraph/env. exit is its exit
// code, or "sleep": it then writes its pid to .codegraph/pid and sleeps.
// As codegraph 1.6.2's runInit does, init on a repo whose codegraph.db
// holds the schema (here: is not "schemaless") says it is already
// initialized and exits 0; index <dir> rebuilds such a database
// (counted in .codegraph/indexes) and fails on any other. It never runs
// the real codegraph.
func fakeCodegraph(t *testing.T, exit string) string {
	t.Helper()
	t.Setenv("AGGY_VENDOR", "")
	bin := t.TempDir()
	tail := "exit " + exit + "\n"
	if exit == "sleep" {
		tail = "echo $$ > \"$2/.codegraph/pid\"\nexec /bin/sleep 777\n"
	}
	writeAt(t, filepath.Join(bin, "codegraph"), `#!/bin/sh
db="$2/.codegraph/codegraph.db"
schema() { [ -f "$db" ] && [ "$(/bin/cat "$db")" != schemaless ]; }
if [ "$1" = index ]; then
	schema || { echo "CodeGraph not initialized" >&2; exit 1; }
	echo run >> "$2/.codegraph/indexes"
	printf db > "$db"
	exit 0
fi
[ "$1" = init ] || exit 9
if schema; then echo "Already initialized"; exit 0; fi
/bin/mkdir -p "$2/.codegraph" || exit 8
[ -e "$2/.codegraph/.gitignore" ] || /bin/cat > "$2/.codegraph/.gitignore" <<'EOF'
`+codegraphGitignore+`EOF
echo run >> "$2/.codegraph/inits"
/usr/bin/env > "$2/.codegraph/env"
printf db > "$2/.codegraph/codegraph.db"
`+tail, 0o755)
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

func codegraphOff(t *testing.T, repo string) {
	t.Helper()
	if _, out, _ := toolRun("", false, "status", "--repo", repo); !strings.HasPrefix(statusLine(t, out, "codegraph"), "codegraph off off off") {
		t.Errorf("recorded although refused: %s", out)
	}
}

// TestCodegraphCommittedIndex (T6820): a repo that commits
// .codegraph/codegraph.db and aggy's marker is not indexed until aggy
// builds it: status says so, and the first of three aggy tool on --repo
// runs the index, after which status says indexed and the others run
// nothing. With no git to ask, status notes it.
func TestCodegraphCommittedIndex(t *testing.T) {
	isolate(t)
	fakeCodegraph(t, "0")
	repo := switchRepo(t, "")
	writeAt(t, filepath.Join(repo, ".codegraph", "codegraph.db"), "db", 0o600)
	writeAt(t, filepath.Join(repo, ".codegraph", indexMarker), "x\n", 0o600)
	gitIn(t, repo, "add", "-f", ".codegraph")
	gitIn(t, repo, "commit", "-q", "-m", "index")

	if _, out, _ := toolRun("", false, "status", "--repo", repo); !strings.Contains(statusLine(t, out, "codegraph"), "index committed in the repo (not built by aggy)") {
		t.Errorf("status: %s", out)
	}
	indexes := filepath.Join(repo, ".codegraph", "indexes")
	for i, vendor := range []string{"", "codex", ""} {
		args := []string{"on", "codegraph", "--repo", repo}
		if vendor != "" {
			args = append(args, "--vendor", vendor)
		}
		if code, out, errOut := toolRun("", false, args...); code != 0 || lines(t, indexes) != 1 {
			t.Fatalf("on %d: %d %q %q, %d indexes", i+1, code, out, errOut, lines(t, indexes))
		}
	}
	if _, out, _ := toolRun("", false, "status", "--repo", repo); !strings.Contains(statusLine(t, out, "codegraph"), "indexed") || strings.Contains(out, "committed") {
		t.Errorf("status after aggy's build: %s", out)
	}

	// An index aggy has no record of, and no git to ask.
	other := switchRepo(t, "")
	writeAt(t, filepath.Join(other, ".codegraph", "codegraph.db"), "db", 0o600)
	writeAt(t, filepath.Join(other, ".codegraph", indexMarker), "x\n", 0o600)
	t.Setenv("PATH", t.TempDir())
	if _, out, _ := toolRun("", false, "status", "--repo", other); !strings.Contains(statusLine(t, out, "codegraph"), "indexed (git could not check)") {
		t.Errorf("status without git: %s", out)
	}
}

// TestCodegraphIndexOnFirstOn (T6711): aggy tool on codegraph --repo
// runs codegraph init in the main checkout until it has finished once
// (aggy's marker), with an environment built from scratch that keeps the
// telemetry opt-out; the folder goes in the repo's git exclude file, so
// git ignores it although codegraph's own .gitignore leaves itself
// tracked; a repo it is never turned on for, or that turns it off, is
// never indexed; a missing or failing codegraph, a symlinked index
// folder, and an agent with no terminal are refused, and nothing is
// recorded; at a terminal the person is asked first.
func TestCodegraphIndexOnFirstOn(t *testing.T) {
	isolate(t)
	fakeCodegraph(t, "0")
	t.Setenv("DO_NOT_TRACK", "0")
	t.Setenv("CODEGRAPH_TELEMETRY", "1")
	t.Setenv("AGGY_SECRET_TEST", "leak")
	repo := switchRepo(t, "")
	other := switchRepo(t, "")
	inits := filepath.Join(repo, ".codegraph", "inits")

	code, out, errOut := toolRun("", false, "on", "codegraph", "--repo", repo)
	if code != 0 || !strings.Contains(out, "Ctrl-C cancels") || lines(t, inits) != 1 || !exists(t, filepath.Join(repo, ".codegraph", indexMarker)) {
		t.Fatalf("first on: %d %q %q, %d runs", code, out, errOut, lines(t, inits))
	}
	env, _ := os.ReadFile(filepath.Join(repo, ".codegraph", "env"))
	// T6822: aggy's opt-outs replace the person's own values.
	if !strings.Contains(string(env), "DO_NOT_TRACK=1\n") || !strings.Contains(string(env), "CODEGRAPH_TELEMETRY=0\n") ||
		strings.Contains(string(env), "DO_NOT_TRACK=0") || strings.Contains(string(env), "AGGY_SECRET_TEST") {
		t.Errorf("init env: %s", env)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, ".codegraph", ".gitignore")); string(b) != codegraphGitignore {
		t.Errorf("codegraph's .gitignore was replaced: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude")); !strings.Contains(string(b), "\n.codegraph/\n") {
		t.Errorf("exclude: %q", b)
	}
	if s := gitIn(t, repo, "status", "--porcelain"); s != "" {
		t.Errorf("the index shows in git status: %q", s)
	}
	// Once indexed, a later on (another vendor) runs nothing.
	if code, _, errOut := toolRun("", false, "on", "codegraph", "--repo", repo, "--vendor", "codex"); code != 0 || lines(t, inits) != 1 {
		t.Fatalf("second on: %d %q, %d runs", code, errOut, lines(t, inits))
	}
	// A database without aggy's marker (a build cut short) is rebuilt
	// with codegraph index: init would only say it is initialized.
	marker, indexes := filepath.Join(repo, ".codegraph", indexMarker), filepath.Join(repo, ".codegraph", "indexes")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := toolRun("", false, "on", "codegraph", "--repo", repo); code != 0 || lines(t, inits) != 1 || lines(t, indexes) != 1 || !exists(t, marker) {
		t.Fatalf("without the marker: %d %q, %d inits, %d indexes", code, errOut, lines(t, inits), lines(t, indexes))
	}
	// A database without the schema: index refuses it, and init rebuilds it.
	_ = os.Remove(marker)
	writeAt(t, filepath.Join(repo, ".codegraph", "codegraph.db"), "schemaless", 0o600)
	code, out, errOut = toolRun("", false, "on", "codegraph", "--repo", repo)
	if b, _ := os.ReadFile(filepath.Join(repo, ".codegraph", "codegraph.db")); code != 0 || !strings.Contains(out, "trying") || lines(t, inits) != 2 || string(b) != "db" || !exists(t, marker) {
		t.Fatalf("schemaless: %d %q %q, %d inits, db %q", code, out, errOut, lines(t, inits), b)
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

	// At a terminal: asked; no leaves nothing.
	asked := switchRepo(t, "")
	code, out, _ = toolRun("n\n", true, "on", "codegraph", "--repo", asked)
	if code != 0 || !strings.Contains(out, "Index ") || exists(t, filepath.Join(asked, ".codegraph")) {
		t.Fatalf("declined: %d %q", code, out)
	}
	codegraphOff(t, asked)
	// An agent with no terminal is refused.
	t.Setenv("AGGY_VENDOR", "codex")
	if code, _, errOut := toolRun("", false, "on", "codegraph", "--repo", asked); code != 1 || !strings.Contains(errOut, "you run it yourself") || exists(t, filepath.Join(asked, ".codegraph")) {
		t.Fatalf("agent: %d %q", code, errOut)
	}
	t.Setenv("AGGY_VENDOR", "")
	codegraphOff(t, asked)
	if code, _, errOut := toolRun("y\n", true, "on", "codegraph", "--repo", asked); code != 0 || !exists(t, filepath.Join(asked, ".codegraph", indexMarker)) {
		t.Fatalf("yes: %d %q", code, errOut)
	}

	// A symlinked index folder: refused, nothing run through it.
	linked := switchRepo(t, "")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(linked, ".codegraph")); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := toolRun("", false, "on", "codegraph", "--repo", linked); code != 1 || !strings.Contains(errOut, "tool.index_failed") || exists(t, filepath.Join(outside, "inits")) {
		t.Fatalf("symlinked folder: %d %q", code, errOut)
	}
	codegraphOff(t, linked)

	// Missing: refused, nothing recorded, the pinned install named.
	third := switchRepo(t, "")
	t.Setenv("PATH", t.TempDir())
	code, _, errOut = toolRun("", false, "on", "codegraph", "--repo", third)
	if code != 1 || !strings.Contains(errOut, "tool.index_failed") || !strings.Contains(errOut, "npm install -g @colbymchenry/codegraph@1.6.2") {
		t.Fatalf("missing: %d %q", code, errOut)
	}
	codegraphOff(t, third)
	// Failing: refused, and nothing recorded.
	fakeCodegraph(t, "1")
	if code, _, errOut := toolRun("", false, "on", "codegraph", "--repo", third); code != 1 || !strings.Contains(errOut, "tool.index_failed") || !strings.Contains(errOut, "exit 1") {
		t.Fatalf("failing init: %d %q", code, errOut)
	}
	codegraphOff(t, third)
}

// indexFixture is a main checkout with a finished codegraph index and
// one of aggy's edit worktrees of it (<state>/worktrees/<id>), and
// codegraph's built-in state as on.
func indexFixture(t *testing.T) (toolRepoAt, switchboard.State) {
	t.Helper()
	isolate(t)
	main := switchRepo(t, "")
	for n, s := range map[string]string{"codegraph.db": "db", "codegraph.db-wal": "wal", "daemon.pid": "1", ".gitignore": codegraphGitignore, indexMarker: "x"} {
		writeAt(t, filepath.Join(main, ".codegraph", n), s, 0o600)
	}
	writeAt(t, filepath.Join(main, ".git", "info", "exclude"), ".codegraph/\n", 0o644)
	wts := filepath.Join(os.Getenv(home.Env), worktreesDir)
	if err := os.MkdirAll(wts, 0o700); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(wts, "0123456789abcdef")
	runGit(t, main, "worktree", "add", "-q", wt)
	repo, err := toolRepo(wt, true)
	if err != nil || repo.root == repo.main {
		t.Fatalf("toolRepo: %+v %v", repo, err)
	}
	return repo, codegraphState(t)
}

func codegraphState(t *testing.T) switchboard.State {
	t.Helper()
	r, _, err := tooldef.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	cg, _ := r.Get("codegraph")
	return switchboard.State{Tool: cg, Vendor: adapter.Claude, On: true, Parts: cg.PartsFor(adapter.Claude)}
}

// TestCopyIndexes (T6711): a session in one of aggy's worktrees with
// codegraph on gets a copy of the main checkout's finished index (its
// database and WAL, never the server's pid file), ignored by git; with
// codegraph off, or not wired, in a worktree of the person's own, or
// from an unfinished index, it gets none; a symlink in the index, an
// index over the cap, or a source that changes while copied refuses the
// copy and leaves no database behind; a folder already there without a
// database (a committed .gitignore, a copy cut short) is copied into.
func TestCopyIndexes(t *testing.T) {
	ctx := context.Background()
	idx := func(repo toolRepoAt) string { return filepath.Join(repo.root, ".codegraph") }
	copyOn := func(repo toolRepoAt, st switchboard.State) []string {
		return copyIndexes(ctx, []switchboard.State{st}, []string{"codegraph"}, repo)
	}
	noDB := func(t *testing.T, repo toolRepoAt, w []string, want string) {
		t.Helper()
		if len(w) != 1 || !strings.Contains(w[0], want) || !strings.Contains(w[0], "tool.index_copy_refused") || exists(t, filepath.Join(idx(repo), "codegraph.db")) {
			t.Errorf("warns %q; db left: %v", w, exists(t, filepath.Join(idx(repo), "codegraph.db")))
		}
	}

	t.Run("on", func(t *testing.T) {
		repo, st := indexFixture(t)
		if w := copyOn(repo, st); len(w) != 0 {
			t.Fatal(w)
		}
		if b, _ := os.ReadFile(filepath.Join(idx(repo), "codegraph.db")); string(b) != "db" {
			t.Errorf("db = %q", b)
		}
		if !exists(t, filepath.Join(idx(repo), "codegraph.db-wal")) || exists(t, filepath.Join(idx(repo), "daemon.pid")) || !exists(t, filepath.Join(idx(repo), indexMarker)) {
			t.Error("want the WAL and the marker, and no pid file")
		}
		if s := gitIn(t, repo.root, "status", "--porcelain", "--ignored=no"); s != "" {
			t.Errorf("the copy shows in git status: %q", s)
		}
		// A second launch leaves the worktree's own index alone.
		writeAt(t, filepath.Join(idx(repo), "codegraph.db"), "branch", 0o600)
		_ = copyOn(repo, st)
		if b, _ := os.ReadFile(filepath.Join(idx(repo), "codegraph.db")); string(b) != "branch" {
			t.Errorf("replaced: %q", b)
		}
	})
	t.Run("folder without a database", func(t *testing.T) {
		repo, st := indexFixture(t)
		writeAt(t, filepath.Join(idx(repo), ".gitignore"), "mine\n", 0o600)
		writeAt(t, filepath.Join(idx(repo), "codegraph.db-wal"), "stale", 0o600)
		writeAt(t, filepath.Join(idx(repo), copyTemp("codegraph.db")), "stale", 0o600)
		if w := copyOn(repo, st); len(w) != 0 {
			t.Fatal(w)
		}
		b, _ := os.ReadFile(filepath.Join(idx(repo), ".gitignore"))
		wal, _ := os.ReadFile(filepath.Join(idx(repo), "codegraph.db-wal"))
		if string(b) != "mine\n" || string(wal) != "wal" || !exists(t, filepath.Join(idx(repo), "codegraph.db")) || exists(t, filepath.Join(idx(repo), copyTemp("codegraph.db"))) {
			t.Errorf(".gitignore %q, wal %q", b, wal)
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
	t.Run("unfinished index", func(t *testing.T) {
		repo, st := indexFixture(t)
		_ = os.Remove(filepath.Join(repo.main, ".codegraph", indexMarker))
		if w := copyOn(repo, st); len(w) != 0 || exists(t, idx(repo)) {
			t.Errorf("copied an unfinished index: %q", w)
		}
	})
	t.Run("person's own worktree", func(t *testing.T) {
		repo, st := indexFixture(t)
		wt := filepath.Join(t.TempDir(), "mine")
		runGit(t, repo.main, "worktree", "add", "-q", wt)
		own, err := toolRepo(wt, true)
		if err != nil {
			t.Fatal(err)
		}
		if w := copyOn(own, st); len(w) != 0 || exists(t, filepath.Join(wt, ".codegraph")) {
			t.Errorf("copied into a worktree aggy gc does not remove: %q", w)
		}
	})
	t.Run("main checkout", func(t *testing.T) {
		repo, st := indexFixture(t)
		repo.root = repo.main
		if w := copyOn(repo, st); len(w) != 0 {
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
		noDB(t, repo, copyOn(repo, st), "symlink")
	})
	t.Run("symlinked db", func(t *testing.T) {
		repo, st := indexFixture(t)
		db := filepath.Join(repo.main, ".codegraph", "codegraph.db")
		_ = os.Remove(db)
		if err := os.Symlink(filepath.Join(t.TempDir(), "x"), db); err != nil {
			t.Fatal(err)
		}
		noDB(t, repo, copyOn(repo, st), "symlink")
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
		noDB(t, repo, copyOn(repo, st), "symlink")
	})
	t.Run("over the cap", func(t *testing.T) {
		repo, st := indexFixture(t)
		old := maxIndexCopy
		maxIndexCopy = 4 // db (2) and wal (3) bytes
		t.Cleanup(func() { maxIndexCopy = old })
		noDB(t, repo, copyOn(repo, st), "over")
	})
	t.Run("changed while copied", func(t *testing.T) {
		repo, st := indexFixture(t)
		testHookIndexCopied = func() {
			writeAt(t, filepath.Join(repo.main, ".codegraph", "codegraph.db-wal"), "checkpointed", 0o600)
		}
		t.Cleanup(func() { testHookIndexCopied = nil })
		noDB(t, repo, copyOn(repo, st), "changed")
		if exists(t, filepath.Join(idx(repo), "codegraph.db-wal")) || exists(t, filepath.Join(idx(repo), copyTemp("codegraph.db"))) {
			t.Error("a partial copy was left")
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
	mustWrite(t, filepath.Join(f.repo, ".codegraph", indexMarker), "x", 0o600)
	mustWrite(t, filepath.Join(f.repo, ".codegraph", ".gitignore"), codegraphGitignore, 0o600)
	mustWrite(t, filepath.Join(f.repo, ".git", "info", "exclude"), ".codegraph/\n", 0o644)
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

// TestCodegraphIndexTelemetryAllowed (T6822): with the user config's
// [tools] allow_telemetry, a prebuild gets no opt-out from aggy, and the
// person's own values pass through as the definition's env allows.
func TestCodegraphIndexTelemetryAllowed(t *testing.T) {
	isolate(t)
	fakeCodegraph(t, "0")
	writeAt(t, filepath.Join(os.Getenv(home.Env), "config.toml"), "[tools]\nallow_telemetry = true\n", 0o600)
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv("CODEGRAPH_TELEMETRY", "1")
	repo := switchRepo(t, "")
	if code, _, errOut := toolRun("", false, "on", "codegraph", "--repo", repo); code != 0 {
		t.Fatalf("on: %d %q", code, errOut)
	}
	env, _ := os.ReadFile(filepath.Join(repo, ".codegraph", "env"))
	if strings.Contains(string(env), "DO_NOT_TRACK=1") || !strings.Contains(string(env), "CODEGRAPH_TELEMETRY=1\n") {
		t.Errorf("init env: %s", env)
	}
}
