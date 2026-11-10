//go:build unix

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/store"
)

// seedBorrowOf records a finished edit borrow's child of vendor in cwd, after a
// first session that makes the parent.
func seedBorrowOf(t *testing.T, w *world, vendor, cwd string) {
	t.Helper()
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if vendor == "claude" {
		seedBorrowChild(t, w, "edit", cwd)
		return
	}
	s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "run", "claude")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy run exit = %d", code)
	}
	parent := fakecli.Records(t, w.record)[0].Env["AGGY_PARENT"]
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(w.home, ".aggy", "aggy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now()
	if err := st.InsertChild(ctx, store.Child{ID: borrowChildID, ParentID: parent, Vendor: vendor, Kind: "edit",
		VendorSessionID: borrowSID, Cwd: cwd, Status: "closed", CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
}

// TestReopenedBorrowKeepsDenies (T6967 review 5): a borrow child reopened on
// screen keeps the policy as denies. Claude gets every rule in
// permissions.deny and no ask; Codex, whose global rules file asks, is not
// reopened at all, and the refusal says why.
func TestReopenedBorrowKeepsDenies(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		w := newWorld(t)
		seedBorrowOf(t, w, "claude", filepath.Join(w.home, "worktree"))
		s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "resume", borrowChildID[:8])
		s.waitFor([]byte("READY>"))
		s.send([]byte{0x04})
		if code := s.exit(); code != 0 {
			t.Fatalf("aggy resume exit = %d", code)
		}
		recs := fakecli.Records(t, w.record)
		vals := recs[len(recs)-1].Flags["--settings"]
		if len(vals) != 1 {
			t.Fatalf("--settings = %q", vals)
		}
		var layer struct {
			Permissions struct{ Ask, Deny []string } `json:"permissions"`
		}
		if err := json.Unmarshal([]byte(vals[0]), &layer); err != nil {
			t.Fatal(err)
		}
		p := layer.Permissions
		if len(p.Ask) != 0 || !slices.Contains(p.Deny, "Bash(rm -rf*)") || !slices.Contains(p.Deny, "Read(//**/.env.*)") {
			t.Errorf("reopened borrow: ask %v, deny %v", p.Ask, p.Deny)
		}
	})
	t.Run("codex", func(t *testing.T) {
		w := newWorld(t)
		seedBorrowOf(t, w, "codex", filepath.Join(w.home, "worktree"))
		runs := len(fakecli.Records(t, w.record))
		s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "resume", borrowChildID[:8])
		s.waitFor([]byte("policy.unreachable"))
		if code := s.exit(); code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
		if !strings.Contains(s.screen(), "Codex borrow child") {
			t.Errorf("the refusal does not say why: %s", s.screen())
		}
		for _, r := range fakecli.Records(t, w.record)[runs:] {
			if r.Mode == "interactive" {
				t.Fatalf("codex started on screen: %+v", r)
			}
		}
	})
}
