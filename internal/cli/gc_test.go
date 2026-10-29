package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

func gc(f *borrowFixture, args ...string) (int, string, string) {
	return gcIn(f, "", false, args...)
}

func gcIn(f *borrowFixture, in string, tty bool, args ...string) (int, string, string) {
	var out, errb strings.Builder
	code := gcWith(args, f.env, strings.NewReader(in), tty, &out, &errb)
	return code, out.String(), errb.String()
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Lstat(p)
	return err == nil
}

// TestGCWorktrees (T6694, T6695): gc removes an accepted worktree and
// its branch once past accepted_worktree_days, and a pending one past
// pending_worktree_days; a worktree whose path became a symlink is
// refused, and the tree it points at is untouched.
func TestGCWorktrees(t *testing.T) {
	f, markers := editFixture(t)
	gitIn(t, f.repo, "checkout", "--", "a.go")
	accepted, wtA := editBorrow(t, f, map[string]string{"a.go": "package a\n// accepted\n"})
	if code, out, errOut := accept(f, "", false, accepted, "--yes"); code != 0 {
		t.Fatalf("accept: %d %s %s", code, out, errOut)
	}
	// T6711: a worktree's copy of the CodeGraph index goes with it.
	mustWrite(t, filepath.Join(wtA, ".codegraph", "codegraph.db"), "db", 0o600)
	mustWrite(t, filepath.Join(wtA, ".codegraph", ".gitignore"), "*\n", 0o600)
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

	// Freshly accepted: kept for aggy pr.
	if code, out, _ := gc(f); code != 0 || !strings.Contains(out, "Nothing to remove.") {
		t.Fatalf("fresh: code %d: %s", code, out)
	}
	mustWrite(t, filepath.Join(f.state, "config.toml"), "[gc]\npending_worktree_days = 90\n", 0o600)
	f.env.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }

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
	if exists(t, wtA) || exists(t, filepath.Join(wtA, ".codegraph")) || gitIn(t, f.repo, "branch", "--list", "aggy/edit-"+accepted) != "" {
		t.Error("accepted worktree, its index copy, or its branch left")
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
	f.env.now = func() time.Time { return time.Now().Add(91 * 24 * time.Hour) }
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
	f.env.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
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

// TestGCDryRunAndAsk (T6695): --dry-run lists what would go, with bytes,
// and removes nothing, not even the index's mtime; on a terminal gc asks
// and removes only on y; the retention days come from the user config.
func TestGCDryRunAndAsk(t *testing.T) {
	f, _ := editFixture(t)
	gitIn(t, f.repo, "checkout", "--", "a.go")
	id, wt := editBorrow(t, f, map[string]string{"a.go": "package a\n// accepted\n"})
	if code, _, errOut := accept(f, "", false, id, "--yes"); code != 0 {
		t.Fatal(errOut)
	}
	report := filepath.Join(f.state, "inbox", "aaaaaaaaaaaaaaaa.md")
	mustWrite(t, report, "two days old", 0o600)
	at := time.Now().Add(31*24*time.Hour - 48*time.Hour) // 2 days old on the test's clock
	if err := os.Chtimes(report, at, at); err != nil {
		t.Fatal(err)
	}
	f.env.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	db := filepath.Join(f.state, "aggy.db")
	before, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}

	code, out, errOut := gc(f, "--dry-run")
	if code != 0 || !strings.Contains(out, wt) || !strings.Contains(out, " B") || !strings.Contains(out, "Dry run: nothing removed.") {
		t.Fatalf("dry run: code %d: %s %s", code, out, errOut)
	}
	if strings.Contains(out, report) {
		t.Errorf("a 2-day report listed under the default 30 days: %s", out)
	}
	if after, _ := os.Stat(db); !exists(t, wt) || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("the dry run removed or wrote something")
	}

	// The user's own config shortens the inbox retention.
	mustWrite(t, filepath.Join(f.state, "config.toml"), "[gc]\ninbox_days = 1\n", 0o600)
	if code, out, _ := gcIn(f, "n\n", true); code != 1 || !strings.Contains(out, report) || !strings.Contains(out, "Remove these? [y/N] Nothing removed.") || !exists(t, wt) || !exists(t, report) {
		t.Fatalf("answered n: code %d: %s", code, out)
	}
	if code, out, errOut := gcIn(f, "y\n", true); code != 0 || exists(t, wt) || exists(t, report) {
		t.Fatalf("answered y: code %d: %s %s", code, out, errOut)
	}
	// What is left is the edit's child row, now that its worktree is gone.
	if code, out, _ := gc(f, "--dry-run"); code != 0 || strings.Contains(out, wt) || !strings.Contains(out, "codex edit borrow") {
		t.Errorf("after: %d %s", code, out)
	}
}

// onRead runs fn on the first read, then reads as r: a person who
// answers the prompt after something else happened meanwhile.
type onRead struct {
	fn func()
	r  io.Reader
}

func (o *onRead) Read(p []byte) (int, error) {
	if o.fn != nil {
		o.fn()
		o.fn = nil
	}
	return o.r.Read(p)
}

// TestGCRecheckLease (T6694, GC-1): a lease taken while the prompt waits
// keeps the worktree.
func TestGCRecheckLease(t *testing.T) {
	f, _ := editFixture(t)
	gitIn(t, f.repo, "checkout", "--", "a.go")
	id, wt := editBorrow(t, f, map[string]string{"a.go": "package a\n// accepted\n"})
	if code, _, errOut := accept(f, "", false, id, "--yes"); code != 0 {
		t.Fatal(errOut)
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	b, err := s.GetBorrow(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var session string
	ps, _ := s.Parents(ctx)
	for _, p := range ps {
		kids, _ := s.Children(ctx, p.ID)
		for _, k := range kids {
			if k.ID == b.ChildID {
				session = k.VendorSessionID
			}
		}
	}
	if session == "" {
		if err := s.SetChildSessionID(ctx, b.ChildID, "s-edit"); err != nil {
			t.Fatal(err)
		}
		session = "s-edit"
	}
	var lease *store.Lease
	in := &onRead{r: strings.NewReader("y\n"), fn: func() {
		if lease, err = s.AcquireLease(ctx, "codex", session); err != nil {
			t.Error(err)
		}
	}}
	var out, errb strings.Builder
	f.env.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	code := gcWith(nil, f.env, in, true, &out, &errb)
	if lease != nil {
		defer func() { _ = lease.Release(ctx) }()
	}
	if code != 1 || !strings.Contains(errb.String(), "lease was taken since") || !exists(t, wt) {
		t.Fatalf("code %d: %s %s", code, out.String(), errb.String())
	}
	if gitIn(t, f.repo, "branch", "--list", "aggy/edit-"+id) == "" {
		t.Error("branch deleted under a held lease")
	}
}

// TestGCLeftover (T6694, GC-3): a removal claimed but cut short (a
// crash) leaves a worktree that the next gc removes.
func TestGCLeftover(t *testing.T) {
	f, _ := editFixture(t)
	gitIn(t, f.repo, "checkout", "--", "a.go")
	id, wt := editBorrow(t, f, map[string]string{"a.go": "package a\n// accepted\n"})
	if code, _, errOut := accept(f, "", false, id, "--yes"); code != 0 {
		t.Fatal(errOut)
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorktreeRemoval(ctx, id, store.WorktreeAccepted, time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	code, out, errOut := gc(f, "--yes")
	if code != 0 || !strings.Contains(out, "cut short") || exists(t, wt) {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	if gitIn(t, f.repo, "branch", "--list", "aggy/edit-"+id) != "" {
		t.Error("branch left")
	}
	if _, out, _ := gc(f); !strings.Contains(out, "Nothing to remove.") {
		t.Errorf("second run: %s", out)
	}
}

// TestGCAcceptedKept (T6695, GC-2): an accepted worktree changed after
// the accept, or whose branch was committed on, is listed and kept. A
// commit of exactly the accepted tree (T6750, GC-7: the edit child
// committing its own work) does not keep it.
func TestGCAcceptedKept(t *testing.T) {
	f, _ := editFixture(t)
	gitIn(t, f.repo, "checkout", "--", "a.go")
	var ids, wts []string
	for _, file := range []string{"a.go", "b.go", "c.go", "d.go"} {
		id, wt := editBorrow(t, f, map[string]string{file: "package a\n// edited\n"})
		if code, _, errOut := accept(f, "", false, id, "--yes"); code != 0 {
			t.Fatal(errOut)
		}
		gitIn(t, f.repo, "checkout", "--", ".")
		gitIn(t, f.repo, "clean", "-fdq")
		ids, wts = append(ids, id), append(wts, wt)
	}
	mustWrite(t, filepath.Join(wts[0], "a.go"), "package a\n// changed later\n", 0o600)
	mustWrite(t, filepath.Join(wts[1], "new.go"), "package a\n", 0o600)
	mustWrite(t, filepath.Join(wts[2], "c.go"), "package a\n// committed later\n", 0o600)
	gitIn(t, wts[2], "commit", "-q", "-a", "-m", "keep me")
	// The work tree back to the accepted state: only the commit differs.
	mustWrite(t, filepath.Join(wts[2], "c.go"), "package a\n// edited\n", 0o600)
	gitIn(t, wts[3], "commit", "-q", "-m", "the child's own commit")
	f.env.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	code, out, errOut := gc(f, "--yes")
	if code != 0 || !strings.Contains(out, "changes made after the accept") || !strings.Contains(out, "new files made after the accept") ||
		!strings.Contains(out, "has commits past the recorded edit") || !strings.Contains(out, "deletes 1 commit(s)") {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	for i, wt := range wts[:3] {
		if !exists(t, wt) || gitIn(t, f.repo, "branch", "--list", "aggy/edit-"+ids[i]) == "" {
			t.Errorf("worktree %d or its branch removed", i)
		}
	}
	if exists(t, wts[3]) || gitIn(t, f.repo, "branch", "--list", "aggy/edit-"+ids[3]) != "" {
		t.Error("a worktree whose only commit is the accepted tree was kept")
	}
}

// TestGCPendingKept (T6778): a pending edit past its age whose branch
// has commits past the recorded edit loses its worktree, not its branch;
// later runs sum the kept branch up in one line, with no "kept:" line.
func TestGCPendingKept(t *testing.T) {
	f, _ := editFixture(t)
	gitIn(t, f.repo, "checkout", "--", "a.go")
	id, wt := editBorrow(t, f, map[string]string{"a.go": "package a\n// pending\n"})
	mustWrite(t, filepath.Join(wt, "mine.go"), "package a\n", 0o600)
	gitIn(t, wt, "add", "mine.go")
	gitIn(t, wt, "commit", "-q", "-m", "my own work")
	branch := "aggy/edit-" + id
	f.env.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	code, out, errOut := gc(f, "--yes")
	if code != 0 || !strings.Contains(out, "its branch "+branch+" is kept") || exists(t, wt) {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	if !strings.Contains(gitIn(t, f.repo, "log", "--format=%s", branch), "my own work") {
		t.Error("the branch or its commit was lost")
	}
	for i := 0; i < 2; i++ {
		_, out, _ = gc(f)
		if strings.Contains(out, wt) || strings.Contains(out, "kept:") ||
			!strings.Contains(out, "1 branch(es) kept with commits past the recorded edit, until you delete them: "+branch) {
			t.Errorf("run %d: %s", i+2, out)
		}
	}
}

// TestGCLeftoverGone (T6750): a removal cut short after the worktree's
// directory went leaves its git admin entry and branch.
//   - plain (GC-6): the next gc removes both, and a run after that has
//     nothing to remove.
//   - committed (M61-1): the person checked the branch out in the main
//     checkout and committed on it; gc drops the admin entry but keeps
//     the branch and its commit, and says so.
//   - foreign admin (M61-3): the admin entry's gitdir points at another
//     worktree and the branch is gone; gc lists nothing and leaves it.
func TestGCLeftoverGone(t *testing.T) {
	for _, tc := range []string{"plain", "committed", "foreign admin"} {
		t.Run(tc, func(t *testing.T) {
			f, _ := editFixture(t)
			gitIn(t, f.repo, "checkout", "--", "a.go")
			id, wt := editBorrow(t, f, map[string]string{"a.go": "package a\n// accepted\n"})
			if code, _, errOut := accept(f, "", false, id, "--yes"); code != 0 {
				t.Fatal(errOut)
			}
			ctx := context.Background()
			s, err := store.OpenDefault(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.ClaimWorktreeRemoval(ctx, id, store.WorktreeAccepted, time.Now()); err != nil {
				t.Fatal(err)
			}
			_ = s.Close()
			// The crash: the directory is gone, git's admin entry and the
			// branch are not.
			if err := os.RemoveAll(wt); err != nil {
				t.Fatal(err)
			}
			branch := "aggy/edit-" + id
			admin := filepath.Join(f.repo, ".git", "worktrees", id)
			if !exists(t, admin) {
				t.Fatalf("no admin entry at %s", admin)
			}
			switch tc {
			case "committed":
				gitIn(t, f.repo, "checkout", "--", ".")
				gitIn(t, f.repo, "checkout", "-q", "--ignore-other-worktrees", branch)
				mustWrite(t, filepath.Join(f.repo, "mine.go"), "package a\n", 0o600)
				gitIn(t, f.repo, "add", "mine.go")
				gitIn(t, f.repo, "commit", "-q", "-m", "my own work")
				gitIn(t, f.repo, "checkout", "-q", "-")
			case "foreign admin":
				mustWrite(t, filepath.Join(admin, "gitdir"), filepath.Join(t.TempDir(), "other", ".git")+"\n", 0o600)
				gitIn(t, f.repo, "update-ref", "-d", "refs/heads/"+branch)
			}
			code, out, errOut := gc(f, "--yes")
			if code != 0 {
				t.Fatalf("code %d: %s %s", code, out, errOut)
			}
			hasBranch := gitIn(t, f.repo, "branch", "--list", branch) != ""
			switch tc {
			case "plain":
				if !strings.Contains(out, "cut short") || exists(t, admin) || hasBranch {
					t.Errorf("admin entry or branch left: %s", out)
				}
			case "committed":
				if !strings.Contains(out, "its branch "+branch+" is kept") || !strings.Contains(out, "kept: its branch "+branch+" has commits") {
					t.Errorf("not listed as kept: %s", out)
				}
				if exists(t, admin) || !hasBranch || !strings.Contains(gitIn(t, f.repo, "log", "--format=%s", branch), "my own work") {
					t.Errorf("admin entry left, or the branch or its commit lost: %s", out)
				}
			case "foreign admin":
				if !strings.Contains(out, "Nothing to remove.") || !exists(t, admin) {
					t.Errorf("foreign admin entry listed or removed: %s", out)
				}
			}
			if _, out, _ := gc(f); !strings.Contains(out, "Nothing to remove.") {
				t.Errorf("second run: %s", out)
			}
		})
	}
}
