package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/store"
)

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
	edited := gitIn(t, f.repo, "rev-parse", "HEAD")
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

	// N53-1: a later change of the person's own, with the same lines,
	// reviewed from after the edit: the edit predates the range.
	mustWrite(t, filepath.Join(f.repo, "e.go"), "package a\n\n"+strings.ReplaceAll(codexLines, "Codex", "Mine"), 0o600)
	mustWrite(t, filepath.Join(f.repo, "f.go"), "package a\n\n"+codexLines, 0o600)
	gitIn(t, f.repo, "add", "e.go", "f.go")
	gitIn(t, f.repo, "commit", "-q", "-m", "mine")
	check("an edit made before the reviewed range", f.repo, material(f.repo, edited), nil)
	noMarkers(t, markers)
}

// TestEditIn (T6747): an edit is in a review on three distinct lines the
// review adds once and the base tree lacks (one for an edit of fewer
// lines), in any file, or on a file of the edit with no long line.
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
	tests := []struct {
		name         string
		edit, review string
		inBase       []string
		want         bool
	}{
		{"three lines", five, file("a.go", l1, l2, l3), nil, true},
		{"two of five", five, file("a.go", l1, l2), nil, false},
		{"moved to another file", five, file("z.go", l1, l2, l3), nil, true},
		{"common lines in the base tree", five, file("a.go", l1, l2, l3), []string{l2}, false},
		{"lines the review adds twice", five, file("a.go", l1, l2, l3) + file("b.go", l3), nil, false},
		{"one of a two-line edit", file("a.go", l1, l2), file("b.go", l2), nil, true},
		{"short lines only", file("a.go", "x := 1", "}"), file("a.go", "y := 2"), nil, true},
		{"short lines, other file", file("a.go", "x := 1"), file("b.go", "x := 1"), nil, false},
		{"binary file touched", five + bin("img.png"), bin("img.png"), nil, true},
		{"binary file untouched", five + bin("img.png"), bin("other.png"), nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := map[string]bool{}
			for _, l := range tt.inBase {
				in[l] = true
			}
			if got := editIn(readDiff(tt.edit), readDiff(tt.review), in); got != tt.want {
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
