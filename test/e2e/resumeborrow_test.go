//go:build unix

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/store"
)

const (
	borrowChildID = "abc1234500000000"
	borrowSID     = "11111111-2222-4333-8444-555555555555"
)

// seedBorrowChild runs a first session, then records a finished borrow's child
// of kind in cwd, with a session file the vendor kept.
func seedBorrowChild(t *testing.T, w *world, kind, cwd string) {
	t.Helper()
	s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "run", "claude")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy run exit = %d", code)
	}
	parent := fakecli.Records(t, w.record)[0].Env["AGGY_PARENT"]
	file := filepath.Join(w.home, ".claude", "projects", "p", borrowSID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(w.home, ".aggy", "aggy.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.InsertChild(ctx, store.Child{ID: borrowChildID, ParentID: parent, Vendor: "claude", Kind: kind,
		VendorSessionID: borrowSID, Cwd: cwd, Status: "closed", CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestResumeBorrowChild (F13): aggy resume takes a finished image borrow's
// child id from aggy session: its CLI resumes the session on screen in that
// borrow's temp dir, made new, and removed after, as the launch note says.
func TestResumeBorrowChild(t *testing.T) {
	w := newWorld(t)
	tmp := filepath.Join(w.home, "tmp")
	cwd := filepath.Join(tmp, "aggy-borrow-1", "work")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	seedBorrowChild(t, w, "image", cwd)

	s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "resume", borrowChildID[:8])
	s.waitFor([]byte("lockdown no longer applies"))
	s.waitFor([]byte("removed when the session ends"))
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy resume exit = %d", code)
	}
	recs := fakecli.Records(t, w.record)
	got := recs[len(recs)-1]
	if !slices.Equal(got.Flags["--resume"], []string{borrowSID}) || got.Cwd != cwd {
		t.Errorf("args %q in %s, want --resume %s in %s", got.Args, got.Cwd, borrowSID, cwd)
	}
	if _, err := os.Lstat(filepath.Dir(cwd)); !os.IsNotExist(err) {
		t.Errorf("the temp dir after the session: %v, want it removed", err)
	}
}

// TestResumeRefusesPlantedTempDir (T6904): an image child's temp dir that is
// already there may hold anything, so the CLI never starts in it.
func TestResumeRefusesPlantedTempDir(t *testing.T) {
	w := newWorld(t)
	cwd := filepath.Join(w.home, "tmp", "aggy-borrow-1", "work")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(cwd, "CLAUDE.md")
	if err := os.WriteFile(planted, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedBorrowChild(t, w, "image", cwd)
	runs := len(fakecli.Records(t, w.record))

	s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "resume", borrowChildID[:8])
	s.waitFor([]byte("already there"))
	if code := s.exit(); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if n := len(fakecli.Records(t, w.record)); n != runs {
		t.Errorf("the CLI started in a planted dir: %d runs, want %d", n, runs)
	}
	if _, err := os.Stat(planted); err != nil {
		t.Errorf("the planted file: %v", err)
	}
}

// TestResumeResearchRefused (T6904): a research child never gets file-reading
// or MCP tools, so it does not go on screen, and the refusal has its code and
// says where the report is.
func TestResumeResearchRefused(t *testing.T) {
	for _, kind := range []string{"research", "news"} {
		t.Run(kind, func(t *testing.T) {
			w := newWorld(t)
			seedBorrowChild(t, w, kind, w.cwd)
			runs := len(fakecli.Records(t, w.record))
			s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "resume", borrowChildID[:8])
			s.waitFor([]byte("aggy inbox"))
			s.waitFor([]byte("borrow.no_screen"))
			if code := s.exit(); code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if n := len(fakecli.Records(t, w.record)); n != runs {
				t.Errorf("the CLI started: %d runs, want %d", n, runs)
			}
		})
	}
}

// TestResumeEditNoWorktreeMemory (T6904): an edit child could write its
// worktree's memory file, and no one has reviewed it, so the session does not
// take it as instructions, and the launch says what accept will do.
func TestResumeEditNoWorktreeMemory(t *testing.T) {
	w := newWorld(t)
	wt := filepath.Join(w.home, "worktree")
	if err := os.MkdirAll(filepath.Join(wt, ".aggy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".aggy", "memory.md"), []byte("planted instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedBorrowChild(t, w, "edit", wt)
	s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "resume", borrowChildID[:8])
	s.waitFor([]byte("aggy accept refuses"))
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy resume exit = %d", code)
	}
	recs := fakecli.Records(t, w.record)
	got := recs[len(recs)-1]
	if len(got.Flags["--append-system-prompt-file"]) != 0 {
		t.Errorf("args %q carry the worktree's memory", got.Args)
	}
}
