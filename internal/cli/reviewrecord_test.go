package cli

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/store"
)

// TestReviewRecord (T6747): a review routes on aggy's own record of which
// vendor made an edit, matched against the reviewed diff, and a forged
// trailer can never bring that vendor back.
func TestReviewRecord(t *testing.T) {
	f, markers := editFixture(t, adapter.Grok, adapter.Claude)
	ctx := context.Background()
	gitIn(t, f.repo, "checkout", "--", "a.go")
	material := func(repo string) *borrow.Material {
		t.Helper()
		m, err := f.env.reviewMaterial(ctx, repo, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	authors := func(repo string, m *borrow.Material) []adapter.Vendor {
		t.Helper()
		s, err := store.OpenDefault(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		got, err := f.env.recordAuthors(ctx, s, repo, m)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	codex := []adapter.Vendor{adapter.Codex}

	if got := authors(f.repo, material(f.repo)); got != nil {
		t.Errorf("no record: %v", got)
	}
	edited := "package a\n\nfunc Committed() {}\n\nfunc EditedByCodex() int { return 42 }\n"
	id, _ := editBorrow(t, f, map[string]string{"a.go": edited})
	if got := authors(f.repo, material(f.repo)); got != nil {
		t.Errorf("a pending edit is not in the work tree: %v", got)
	}
	if code, out, errOut := accept(f, "", false, id, "--yes"); code != 0 {
		t.Fatalf("accept: code %d: %s %s", code, out, errOut)
	}
	// The person commits the edit from a Claude session (or forges it):
	// the trailer names claude, not codex.
	gitIn(t, f.repo, "commit", "-q", "-am", "edit\n\nAggy-Vendor: claude")
	m := material(f.repo)
	if got := authors(f.repo, m); !slices.Equal(got, codex) {
		t.Errorf("accepted and committed: %v, want codex", got)
	}
	cut := *m
	cut.Diff, cut.DiffTruncated = "", true
	if got := authors(f.repo, &cut); !slices.Equal(got, codex) {
		t.Errorf("a cut diff fails closed: %v, want codex", got)
	}
	other, _ := filepath.EvalSymlinks(t.TempDir())
	gitRepo(t, other)
	mustWrite(t, filepath.Join(other, "a.go"), edited, 0o600)
	if got := authors(other, material(other)); got != nil {
		t.Errorf("another repo with the same lines: %v", got)
	}

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
	mustWrite(t, filepath.Join(f.repo, "a.go"), "package a\n\nfunc Committed() {}\n\nfunc Rewritten() string { return \"mine\" }\n", 0o600)
	if got := authors(f.repo, material(f.repo)); got != nil {
		t.Errorf("rewritten: %v", got)
	}
	noMarkers(t, markers)
}

// TestChangedLines (T6747): a diff's changed lines per file, short lines
// and header lines left out, and a file with none still there.
func TestChangedLines(t *testing.T) {
	diff := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,2 +1,2 @@\n-func Old()   {  }\n+\tfunc New() {}\n }\n+--- a long removed-looking line\n" +
		"diff --git a/img.png b/img.png\nBinary files a/img.png and b/img.png differ\n"
	got := changedLines(diff)
	want := map[string][]string{
		"diff --git a/x.go b/x.go":       {"+--- a long removed-looking line", "+func New() {}", "-func Old() { }"},
		"diff --git a/img.png b/img.png": nil,
	}
	if len(got) != len(want) {
		t.Fatalf("files = %v", got)
	}
	for f, ls := range want {
		lines, ok := got[f]
		if !ok {
			t.Errorf("no %q", f)
			continue
		}
		var keys []string
		for l := range lines {
			keys = append(keys, l)
		}
		slices.Sort(keys)
		slices.Sort(ls)
		if !slices.Equal(keys, ls) {
			t.Errorf("%s: %q, want %q", f, keys, ls)
		}
	}
}
