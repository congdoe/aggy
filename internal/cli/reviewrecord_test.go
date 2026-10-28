package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/store"
)

// editRecords writes edit borrow records straight into the index of the
// fixture's repo: a quick stand-in for running each edit.
type editRecords struct {
	t   *testing.T
	f   *borrowFixture
	s   *store.Store
	n   int
	now time.Time
}

func newEditRecords(t *testing.T, f *borrowFixture) *editRecords {
	t.Helper()
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().Add(-24 * time.Hour)
	if err := s.InsertParent(ctx, store.Parent{ID: "rec-parent", Dir: f.repo, CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
	return &editRecords{t: t, f: f, s: s, now: now}
}

// add records an edit by vendor from base to tree, one second newer than
// the last, and marks its worktree removed when removed is set.
func (r *editRecords) add(vendor, base, tree string, removed bool) {
	r.t.Helper()
	ctx := context.Background()
	r.n++
	id := fmt.Sprintf("%016x", r.n)
	at := r.now.Add(time.Duration(r.n) * time.Second)
	path := filepath.Join(r.f.state, worktreesDir, id)
	c := store.Child{ID: "rec-" + id, ParentID: "rec-parent", Vendor: vendor, Kind: "edit", Cwd: path, CreatedAt: at, LastActive: at,
		Worktree: store.Worktree{Path: path, Branch: branchPrefix + id, Base: base, Repo: r.f.repo, State: store.WorktreePending}}
	l, err := r.s.StartBorrow(ctx, c, store.Borrow{ID: id, Kind: "edit", Vendor: vendor, StartedAt: at}, store.BorrowCaps{})
	if err == nil {
		err = l.Finish(ctx, store.BorrowEnd{Status: "done", EndedAt: at})
	}
	if err == nil {
		err = r.s.SetWorktreeTree(ctx, id, tree)
	}
	if err == nil && removed {
		err = r.s.ClaimWorktreeRemoval(ctx, id, store.WorktreePending, at)
	}
	if err != nil {
		r.t.Fatal(err)
	}
}

// gitStdin runs the test's own git in dir with in as stdin.
func gitStdin(t *testing.T, dir, in string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Stdin = dir, strings.NewReader(in)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// TestReviewRecordMany (N53-9): every recorded edit is compared, in a
// fixed number of git calls; an old one is never counted blind. Missing
// objects count while the worktree is there, never once aggy gc removed
// it.
func TestReviewRecordMany(t *testing.T) {
	f, _ := editFixture(t)
	ctx := context.Background()
	gitIn(t, f.repo, "checkout", "--", "a.go")
	base := gitIn(t, f.repo, "rev-parse", "HEAD")
	recs := newEditRecords(t, f)
	check := func(name string, want []adapter.Vendor, note string) {
		t.Helper()
		m, err := f.env.reviewMaterial(ctx, f.repo, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		got, notes, err := f.env.recordAuthors(ctx, recs.s, f.repo, m)
		if err != nil || !slices.Equal(got, want) || (note == "") != (len(notes) == 0) || (note != "" && !strings.Contains(strings.Join(notes, "\n"), note)) {
			t.Errorf("%s: authors %v, notes %q, err %v; want %v, note %q", name, got, notes, err, want, note)
		}
	}

	// The edit in review is the oldest of all: grok's lines in a.go.
	committed := "package a\n\nfunc Committed() {}\n"
	mustWrite(t, filepath.Join(f.repo, "a.go"), committed+codexLines, 0o600)
	gitIn(t, f.repo, "add", "a.go")
	recs.add("grok", base, gitIn(t, f.repo, "write-tree"), false)
	gitIn(t, f.repo, "reset", "-q")
	gitIn(t, f.repo, "checkout", "--", "a.go")
	// 60 newer codex edits, each adding its own file.
	ls := gitIn(t, f.repo, "ls-tree", "HEAD")
	dir := t.TempDir()
	var paths []string
	for i := range 60 {
		p := filepath.Join(dir, fmt.Sprintf("u%d.go", i))
		mustWrite(t, p, fmt.Sprintf("package a\n\nfunc Unrelated%d() string { return \"value %d\" }\n", i, i), 0o600)
		paths = append(paths, p)
	}
	blobs := strings.Fields(gitIn(t, f.repo, append([]string{"hash-object", "-w"}, paths...)...))
	for i, b := range blobs {
		recs.add("codex", base, gitStdin(t, f.repo, ls+"\n100644 blob "+b+"\tu"+fmt.Sprint(i)+".go\n", "mktree"), false)
	}

	// The person's own change to z.go: none of the 61 is in it.
	mustWrite(t, filepath.Join(f.repo, "z.go"), "package a\n\nfunc Mine() string { return \"the person's own\" }\n", 0o600)
	gitIn(t, f.repo, "add", "z.go")
	check("an unrelated change among 61 edits", nil, "")
	mustWrite(t, filepath.Join(f.repo, "a.go"), committed+codexLines, 0o600)
	check("the oldest edit, in review", []adapter.Vendor{adapter.Grok}, "")
	gitIn(t, f.repo, "checkout", "--", "a.go")

	// Objects git no longer has: gone with a removed worktree, counted
	// while the worktree is there.
	gone := strings.Repeat("ab", 20)
	recs.add("claude", base, gone, true)
	check("missing objects, worktree removed", nil, "")
	recs.add("claude", base, gone, false)
	check("missing objects, worktree there", []adapter.Vendor{adapter.Claude}, "no longer has the objects")
}

// TestReviewRecordMove (N53-9 MEDIUM): an edit that moves code already in
// the base into another file is in the review: it has not landed, since
// the base does not hold its files as it left them.
func TestReviewRecordMove(t *testing.T) {
	f, _ := editFixture(t)
	ctx := context.Background()
	gitIn(t, f.repo, "checkout", "--", "a.go")
	body := "func B() int {\n\ttotal := computeTheTotal(items)\n\tfiltered := filterTheItems(items, total)\n\treturn reduceTheFiltered(filtered)\n}\n"
	mustWrite(t, filepath.Join(f.repo, "b.go"), "package a\n\n"+body, 0o600)
	gitIn(t, f.repo, "add", "b.go")
	gitIn(t, f.repo, "commit", "-q", "-m", "b")
	base := gitIn(t, f.repo, "rev-parse", "HEAD")
	recs := newEditRecords(t, f)
	// Codex moved B into c.go; the person accepted it.
	mustWrite(t, filepath.Join(f.repo, "b.go"), "package a\n", 0o600)
	mustWrite(t, filepath.Join(f.repo, "c.go"), "package a\n\n"+body, 0o600)
	gitIn(t, f.repo, "add", "-A", "b.go", "c.go")
	recs.add("codex", base, gitIn(t, f.repo, "write-tree"), false)
	m, err := f.env.reviewMaterial(ctx, f.repo, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, notes, err := f.env.recordAuthors(ctx, recs.s, f.repo, m); err != nil || !slices.Equal(got, []adapter.Vendor{adapter.Codex}) {
		t.Errorf("moved code: authors %v, notes %q, err %v; want codex", got, notes, err)
	}
	// Once committed, a review from there finds it landed.
	gitIn(t, f.repo, "commit", "-q", "-m", "moved")
	if m, err = f.env.reviewMaterial(ctx, f.repo, "HEAD", nil); err != nil {
		t.Fatal(err)
	}
	if got, _, err := f.env.recordAuthors(ctx, recs.s, f.repo, m); err != nil || got != nil {
		t.Errorf("landed move: authors %v, err %v; want none", got, err)
	}
}

// codexLines are an edit's own lines, long and found nowhere else.
const codexLines = "func EditedByCodex() int { return 42 }\n" +
	"func AlsoByCodex(s string) string { return s + \"!\" }\n" +
	"var codexTable = map[string]int{\"one\": 1, \"two\": 2}\n"

// TestReviewRecord (T6747): a review routes on aggy's own record of which
// vendor made an edit, matched against the reviewed diff, and a forged
// trailer can never bring that vendor back.
func TestReviewRecord(t *testing.T) {
	f, markers := editFixture(t, adapter.Grok, adapter.Claude)
	ctx := context.Background()
	gitIn(t, f.repo, "checkout", "--", "a.go")
	material := func(repo, base string) *borrow.Material {
		t.Helper()
		m, err := f.env.reviewMaterial(ctx, repo, base, nil)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	authors := func(repo string, m *borrow.Material) ([]adapter.Vendor, []string) {
		t.Helper()
		s, err := store.OpenDefault(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		got, notes, err := f.env.recordAuthors(ctx, s, repo, m)
		if err != nil {
			t.Fatal(err)
		}
		return got, notes
	}
	check := func(name, repo string, m *borrow.Material, want []adapter.Vendor) []string {
		t.Helper()
		got, notes := authors(repo, m)
		if !slices.Equal(got, want) {
			t.Errorf("%s: authors %v, want %v (notes %q)", name, got, want, notes)
		}
		return notes
	}
	codex := []adapter.Vendor{adapter.Codex}

	check("no record", f.repo, material(f.repo, ""), nil)
	committed := "package a\n\nfunc Committed() {}\n"
	id, _ := editBorrow(t, f, map[string]string{"a.go": committed + codexLines})
	check("a pending edit is not in the work tree", f.repo, material(f.repo, ""), nil)
	if code, out, errOut := accept(f, "", false, id, "--yes"); code != 0 {
		t.Fatalf("accept: code %d: %s %s", code, out, errOut)
	}
	// The person commits the edit from a Claude session (or forges it):
	// the trailer names claude, not codex.
	gitIn(t, f.repo, "commit", "-q", "-am", "edit\n\nAggy-Vendor: claude")
	m := material(f.repo, "")
	check("accepted and committed", f.repo, m, codex)

	// N53-3: a cut diff falls back to the files the edit touched, and says so.
	cut := *m
	cut.Diff, cut.DiffTruncated = "", true
	if notes := check("cut diff", f.repo, &cut, codex); len(notes) != 1 || !strings.Contains(notes[0], "size cap") || !strings.Contains(notes[0], "codex (borrow "+id+")") {
		t.Errorf("cut diff notes %q", notes)
	}

	// N53-2: a rename, or the code pasted into another file, still counts.
	gitIn(t, f.repo, "mv", "a.go", "b.go")
	check("renamed", f.repo, material(f.repo, ""), codex)
	gitIn(t, f.repo, "mv", "b.go", "a.go")
	mustWrite(t, filepath.Join(f.repo, "a.go"), committed, 0o600)
	mustWrite(t, filepath.Join(f.repo, "d.go"), "package a\n\n"+codexLines, 0o600)
	gitIn(t, f.repo, "add", "d.go")
	check("pasted elsewhere", f.repo, material(f.repo, ""), codex)
	gitIn(t, f.repo, "rm", "-q", "--cached", "d.go")
	if err := os.Remove(filepath.Join(f.repo, "d.go")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.repo, "checkout", "--", "a.go")

	other, _ := filepath.EvalSymlinks(t.TempDir())
	gitRepo(t, other)
	mustWrite(t, filepath.Join(other, "a.go"), committed+codexLines, 0o600)
	check("another repo with the same lines", other, material(other, ""), nil)

	// Record (codex) and trailer (claude) disagree: both are skipped, and
	// grok reviews.
	f.scenario["grok"] = `{"json":{"result":"LGTM"}}`
	before := len(fakecli.Records(t, f.record))
	code, out, errOut := f.run(t, "review", "review the change")
	if code != 0 {
		t.Fatalf("review: code %d: %s %s", code, out, errOut)
	}
	var ran []string
	for _, r := range fakecli.Records(t, f.record)[before:] {
		if r.Mode == "headless" {
			ran = append(ran, r.Vendor)
		}
	}
	if !slices.Equal(ran, []string{"grok"}) {
		t.Errorf("review ran on %v, want grok", ran)
	}

	// allowed_vendors leaves only the recorded author: refused.
	mustWrite(t, filepath.Join(f.repo, ".aggy", "config.toml"), "allowed_vendors = [\"codex\"]\n", 0o600)
	before = len(fakecli.Records(t, f.record))
	code, out, errOut = f.run(t, "review", "review the change")
	if code != 1 || !strings.Contains(out+errOut, "codex: aggy's record shows it made an edit in this change") {
		t.Errorf("only the author allowed: code %d: %s %s", code, out, errOut)
	}
	if n := len(fakecli.Records(t, f.record)); n != before {
		t.Errorf("a child ran: %d records, want %d", n, before)
	}

	// The person rewrote the edit's lines: no longer codex's change.
	mustWrite(t, filepath.Join(f.repo, "a.go"), committed+"\nfunc Rewritten() string { return \"mine\" }\n", 0o600)
	check("rewritten", f.repo, material(f.repo, ""), nil)
	gitIn(t, f.repo, "checkout", "--", "a.go")

	// N53-6: main moves and the branch is rebased onto it, then squashed:
	// the edit's base commit is rewritten, its lines are not.
	gitIn(t, f.repo, "checkout", "-q", "main")
	mustWrite(t, filepath.Join(f.repo, "m.go"), "package a\n\nfunc OnMain() {}\n", 0o600)
	gitIn(t, f.repo, "add", "m.go")
	gitIn(t, f.repo, "commit", "-q", "-m", "main moves")
	gitIn(t, f.repo, "checkout", "-q", "feature")
	gitIn(t, f.repo, "rebase", "-q", "main")
	check("rebased", f.repo, material(f.repo, ""), codex)
	gitIn(t, f.repo, "reset", "-q", "--soft", "main")
	gitIn(t, f.repo, "commit", "-q", "-m", "squashed")
	squashed := gitIn(t, f.repo, "rev-parse", "HEAD")
	check("squashed", f.repo, material(f.repo, ""), codex)

	// N53-1: a later change of the person's own, with the same lines,
	// reviewed from a base that holds the edit: it landed there.
	mustWrite(t, filepath.Join(f.repo, "e.go"), "package a\n\n"+strings.ReplaceAll(codexLines, "Codex", "Mine"), 0o600)
	mustWrite(t, filepath.Join(f.repo, "f.go"), "package a\n\n"+codexLines, 0o600)
	gitIn(t, f.repo, "add", "e.go", "f.go")
	gitIn(t, f.repo, "commit", "-q", "-m", "mine")
	check("an edit that landed in the base", f.repo, material(f.repo, squashed), nil)
	noMarkers(t, markers)
}

// TestEditIn (T6747): an edit is in a review on three distinct lines the
// review adds once and the base tree lacks (all of them, for an edit with
// fewer such lines), in any file, or on a file of the edit with no long
// line; never when every line it added is in the base tree.
func TestEditIn(t *testing.T) {
	file := func(path string, lines ...string) string {
		s := "diff --git a/" + path + " b/" + path + "\n--- a/" + path + "\n+++ b/" + path + "\n@@ -0,0 +1 @@\n"
		for _, l := range lines {
			s += "+" + l + "\n"
		}
		return s
	}
	bin := func(path string) string {
		return "diff --git a/" + path + " b/" + path + "\nBinary files a/" + path + " and b/" + path + " differ\n"
	}
	l1, l2, l3, l4, l5 := "alpha := compute(one)", "beta := compute(two)", "gamma := compute(three)", "delta := compute(four)", "epsilon := compute(five)"
	five := file("a.go", l1, l2, l3, l4, l5)
	all := []string{l1, l2, l3, l4, l5}
	tests := []struct {
		name           string
		edit, review   string
		inBase, landed []string
		want           bool
	}{
		{"three lines", five, file("a.go", l1, l2, l3), nil, nil, true},
		{"two of five", five, file("a.go", l1, l2), nil, nil, false},
		{"moved to another file", five, file("z.go", l1, l2, l3), nil, nil, true},
		{"common lines in the base tree", five, file("a.go", l1, l2, l3), []string{l2}, nil, false},
		{"lines the review adds twice", five, file("a.go", l1, l2, l3) + file("b.go", l3), nil, nil, false},
		{"one of a two-line edit", file("a.go", l1, l2), file("b.go", l2), nil, nil, false},
		{"both of a two-line edit", file("a.go", l1, l2), file("b.go", l2, l1), nil, nil, true},
		{"short lines only", file("a.go", "x := 1", "}"), file("a.go", "y := 2"), nil, nil, true},
		{"short lines, other file", file("a.go", "x := 1"), file("b.go", "x := 1"), nil, nil, false},
		{"short lines, file landed", file("a.go", "x := 1"), file("a.go", "y := 2"), nil, []string{"a.go"}, false},
		{"binary file touched", five + bin("img.png"), bin("img.png"), nil, nil, true},
		{"binary file untouched", five + bin("img.png"), bin("other.png"), nil, nil, false},
		// N53-7: common lines padding an edit lower what it needs.
		{"two distinctive lines padded with three common ones", five, file("a.go", l1, l2), []string{l3, l4, l5}, nil, true},
		{"one distinctive line", five, file("b.go", l5), []string{l1, l2, l3, l4}, nil, true},
		// Landed: every file it touched is in the base as it left it.
		{"landed", five + bin("img.png"), file("a.go", l1, l2, l3) + bin("img.png"), all, []string{"a.go", "img.png"}, false},
		// A move or copy of code already in the base, files not landed:
		// its lines decide.
		{"moved code the review adds", five, file("c.go", l1, l2, l3), all, nil, true},
		{"landed lines, file changed since, review elsewhere", five, file("a.go", "zeta := other(six)"), all, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := func(ls []string) map[string]bool {
				m := map[string]bool{}
				for _, l := range ls {
					m[l] = true
				}
				return m
			}
			if got := editIn(readDiff(tt.edit), readDiff(tt.review), set(tt.inBase), set(tt.landed)); got != tt.want {
				t.Errorf("editIn = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDiffPath(t *testing.T) {
	for h, want := range map[string]string{
		"diff --git a/x.go b/x.go":                   "x.go",
		"diff --git a/a b/c b/a b/c":                 "a b/c",
		`diff --git "a/t\tab" "b/t\tab"`:             "t\tab",
		`diff --git "a/caf\303\251" "b/caf\303\251"`: "café",
		"diff --git a/x b/y":                         "",
		"diff --git x":                               "",
	} {
		if got := diffPath(h); got != want {
			t.Errorf("diffPath(%q) = %q, want %q", h, got, want)
		}
	}
}

// TestInBase (N53-8): a line is in the base tree only as a whole line,
// compared normalized, never as part of a longer one.
func TestInBase(t *testing.T) {
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	gitIn(t, repo, "init", "-q")
	mustWrite(t, filepath.Join(repo, "x.go"), "x := alpha(one) + beta(two)\n\t\tgamma := delta(three)  \n", 0o600)
	gitIn(t, repo, "add", "x.go")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	lines := []string{"alpha(one) + beta(two)", "gamma := delta(three)", "absent := line(four)"}
	text := map[string]string{}
	for _, l := range lines {
		text[l] = l
	}
	got := gitEnv().inBase(context.Background(), repo, "HEAD", lines, text)
	if len(got) != 1 || !got["gamma := delta(three)"] {
		t.Errorf("inBase = %v, want only the whole line", got)
	}
}

// TestGitSafeEnv (N53-5): every git call aggy runs takes gitSafe and
// GIT_NO_LAZY_FETCH=1, so a partial clone never fetches from its remote.
func TestGitSafeEnv(t *testing.T) {
	var got adapter.Command
	e := borrowEnv{
		gitPath: func() (string, error) { return "/usr/bin/git", nil },
		gitRun: func(_ context.Context, c adapter.Command) (adapter.Output, error) {
			got = c
			return adapter.Output{}, nil
		},
	}
	if _, err := e.gitOutput(context.Background(), "/r", "status"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Env, "GIT_NO_LAZY_FETCH=1") || !slices.Equal(got.Args[:len(gitSafe)], gitSafe) {
		t.Errorf("git ran with env %q, args %q", got.Env, got.Args)
	}
}
