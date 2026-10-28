package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

func gc(f *borrowFixture, args ...string) (int, string, string) {
	var out, errb strings.Builder
	code := gcWith(args, f.env, &out, &errb)
	return code, out.String(), errb.String()
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Lstat(p)
	return err == nil
}

// TestGCWorktrees (T6694): gc removes an accepted worktree and its
// branch, and a pending one only once it is old; a worktree whose path
// became a symlink is refused, and the tree it points at is untouched.
func TestGCWorktrees(t *testing.T) {
	f, markers := editFixture(t)
	gitIn(t, f.repo, "checkout", "--", "a.go")
	accepted, wtA := editBorrow(t, f, map[string]string{"a.go": "package a\n// accepted\n"})
	if code, out, errOut := accept(f, "", false, accepted, "--yes"); code != 0 {
		t.Fatalf("accept: %d %s %s", code, out, errOut)
	}
	pending, wtP := editBorrow(t, f, map[string]string{"b.go": "package a\n"})
	linked, wtL := editBorrow(t, f, map[string]string{"c.go": "package a\n"})
	if code, _, errOut := accept(f, "", false, linked, "--yes"); code != 0 {
		t.Fatal(errOut)
	}
	// The linked worktree's directory is moved away and a symlink to it
	// put in its place.
	moved := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Rename(wtL, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, wtL); err != nil {
		t.Fatal(err)
	}

	// Without --yes nothing goes.
	code, out, _ := gc(f)
	if code != 1 || !strings.Contains(out, wtA) || strings.Contains(out, wtP) || !strings.Contains(out, "Nothing removed") {
		t.Fatalf("no --yes: code %d: %s", code, out)
	}
	if !exists(t, wtA) {
		t.Fatal("removed without --yes")
	}
	code, out, errOut := gc(f, "--yes")
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	if exists(t, wtA) || gitIn(t, f.repo, "branch", "--list", "aggy/edit-"+accepted) != "" {
		t.Error("accepted worktree or branch left")
	}
	if !exists(t, wtP) {
		t.Error("a new pending worktree was removed")
	}
	if !strings.Contains(out, "symlink") || !exists(t, filepath.Join(moved, "c.go")) {
		t.Errorf("symlinked worktree not refused, or its target touched: %s", out)
	}
	if gitIn(t, f.repo, "branch", "--list", "aggy/edit-"+linked) == "" {
		t.Error("the refused worktree's branch was deleted")
	}

	// A second run has nothing left of the accepted one.
	if _, out, _ := gc(f); strings.Contains(out, wtA) {
		t.Errorf("removed twice: %s", out)
	}
	// Past the pending age, the pending one goes, as a reject would.
	f.env.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	if code, out, errOut := gc(f, "--yes"); code != 0 || exists(t, wtP) {
		t.Fatalf("old pending: code %d: %s %s", code, out, errOut)
	}
	if code, _, errOut := accept(f, "", false, pending, "--yes"); code != 1 || !strings.Contains(errOut, "already rejected") {
		t.Errorf("accept after gc: %d %s", code, errOut)
	}
	noMarkers(t, markers)
}

// TestGCNotAggysBranch (T6694): a worktree record whose branch is not
// aggy/edit-<id> is left alone, and so is that branch.
func TestGCNotAggysBranch(t *testing.T) {
	f, _ := editFixture(t)
	gitIn(t, f.repo, "branch", "theirs")
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef"
	path := filepath.Join(resolvePath(filepath.Join(f.state, worktreesDir)), id)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := s.InsertParent(ctx, store.Parent{ID: "p1", Dir: f.repo, CreatedAt: old, LastActive: old}); err != nil {
		t.Fatal(err)
	}
	c := store.Child{ID: "c1", ParentID: "p1", Vendor: "codex", Kind: "edit", Cwd: path, CreatedAt: old, LastActive: old,
		Worktree: store.Worktree{Path: path, Branch: "theirs", Repo: f.repo, State: store.WorktreePending}}
	l, err := s.StartBorrow(ctx, c, store.Borrow{ID: id, Kind: "edit", Vendor: "codex", StartedAt: old}, store.BorrowCaps{})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Finish(ctx, store.BorrowEnd{Status: "done", EndedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorktreeState(ctx, id, store.WorktreePending, store.WorktreeAccepted); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	code, out, errOut := gc(f, "--yes")
	if code != 0 || !strings.Contains(out, "not a worktree aggy made") {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	if !exists(t, path) || gitIn(t, f.repo, "branch", "--list", "theirs") == "" {
		t.Error("removed what aggy did not make")
	}
}

// TestGCChildrenInboxCache (T6694): closed borrow children past their
// age leave the index unless their lease is held; open (interactive)
// children stay; old inbox reports and expired cache entries go, a
// symlink among them is refused.
func TestGCChildrenInboxCache(t *testing.T) {
	f := newBorrowFixture(t, adapter.Grok)
	t.Setenv("PATH", t.TempDir())
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	old := time.Now().Add(-60 * 24 * time.Hour)
	if err := s.InsertParent(ctx, store.Parent{ID: "p1", Dir: f.repo, CreatedAt: old, LastActive: old}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []store.Child{
		{ID: "held", Status: "closed", Kind: "research", VendorSessionID: "s-held"},
		{ID: "free", Status: "closed", Kind: "research", VendorSessionID: "s-free"},
		{ID: "session", Status: "closed", VendorSessionID: "s-int"},
		{ID: "open", Kind: "research"},
	} {
		c.ParentID, c.Vendor, c.Cwd, c.CreatedAt, c.LastActive = "p1", "grok", f.repo, old, old
		if err := s.InsertChild(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := s.AcquireLease(ctx, "grok", "s-held")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release(ctx) }()

	write := func(rel, data string, age time.Duration) string {
		p := filepath.Join(f.state, rel)
		mustWrite(t, p, data, 0o600)
		at := time.Now().Add(-age)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
		return p
	}
	day := 24 * time.Hour
	oldReport := write("inbox/aaaaaaaaaaaaaaaa.md", "old", 40*day)
	newReport := write("inbox/bbbbbbbbbbbbbbbb.md", "new", day)
	key := strings.Repeat("c", 64)
	expired := write("research/cache/"+key+".json", `{"kind":"news","as_of":"`+time.Now().Add(-2*day).Format(time.RFC3339)+`"}`, 0)
	fresh := write("research/cache/"+strings.Repeat("d", 64)+".json", `{"kind":"research","as_of":"`+time.Now().Add(-2*day).Format(time.RFC3339)+`"}`, 0)
	target := write("keep.md", "not aggy's to remove", 90*day)
	link := filepath.Join(f.state, "inbox", "eeeeeeeeeeeeeeee.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := gc(f, "--yes")
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	kids, err := s.Children(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, k := range kids {
		left = append(left, k.ID)
	}
	if got := strings.Join(left, ","); !strings.Contains(got, "held") || strings.Contains(got, "free") ||
		!strings.Contains(got, "session") || !strings.Contains(got, "open") {
		t.Errorf("children left: %s\n%s", got, out)
	}
	if !strings.Contains(out, "child held: its lease is held") {
		t.Errorf("held lease not named: %s", out)
	}
	if exists(t, oldReport) || !exists(t, newReport) || exists(t, expired) || !exists(t, fresh) {
		t.Errorf("inbox/cache: old %v new %v expired %v fresh %v", exists(t, oldReport), exists(t, newReport), exists(t, expired), exists(t, fresh))
	}
	if !exists(t, link) || !exists(t, target) || !strings.Contains(out, "not a regular file") {
		t.Errorf("symlinked report not refused: %s", out)
	}
}

// TestGCDelegated (T6694): a borrow child cannot run gc.
func TestGCDelegated(t *testing.T) {
	f := newBorrowFixture(t)
	t.Setenv("PATH", t.TempDir())
	f.env.getenv = func(k string) string {
		if k == "AGGY_DELEGATED" {
			return "1"
		}
		return ""
	}
	if code, _, errOut := gc(f, "--yes"); code != 1 || errOut == "" {
		t.Errorf("code %d: %s", code, errOut)
	}
}
