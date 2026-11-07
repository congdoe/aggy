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

// TestResumeBorrowChild (F13): aggy resume takes a finished research borrow's
// child id from aggy session: its CLI resumes the session on screen in that
// borrow's temp dir, made again, empty, and removed after.
func TestResumeBorrowChild(t *testing.T) {
	w := newWorld(t)
	sc := map[string]any{"raw_echo": true, "stdout": "READY>"}
	s := w.start(t, sc, "run", "claude")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy run exit = %d", code)
	}
	parent := fakecli.Records(t, w.record)[0].Env["AGGY_PARENT"]

	const (
		childID = "abc1234500000000"
		sid     = "11111111-2222-4333-8444-555555555555"
	)
	tmp := filepath.Join(w.home, "tmp")
	cwd := filepath.Join(tmp, "aggy-borrow-1", "work")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(w.home, ".claude", "projects", "p", sid+".jsonl")
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
	if err := st.InsertChild(ctx, store.Child{ID: childID, ParentID: parent, Vendor: "claude", Kind: "research",
		VendorSessionID: sid, Cwd: cwd, Status: "closed", CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	s = w.start(t, sc, "resume", childID[:8])
	s.waitFor([]byte("lockdown"))
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy resume exit = %d", code)
	}
	recs := fakecli.Records(t, w.record)
	got := recs[len(recs)-1]
	if !slices.Equal(got.Flags["--resume"], []string{sid}) || got.Cwd != cwd {
		t.Errorf("args %q in %s, want --resume %s in %s", got.Args, got.Cwd, sid, cwd)
	}
	if _, err := os.Lstat(filepath.Dir(cwd)); !os.IsNotExist(err) {
		t.Errorf("the temp dir after the session: %v, want it removed", err)
	}
}
