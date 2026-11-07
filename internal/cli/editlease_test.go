package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/store"
)

// TestEditWorktreeRespectsLease (T6904): while an edit child's session is on
// screen, aggy reject, accept, and check leave its worktree alone, as aggy gc
// does; once it closes, reject works.
func TestEditWorktreeRespectsLease(t *testing.T) {
	f, _ := editFixture(t)
	id, wt := editBorrow(t, f, map[string]string{"a.go": "package a\n// edited\n"})
	ctx := context.Background()
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	b, err := s.GetBorrow(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var session string
	ps, err := s.Parents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		kids, err := s.Children(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range kids {
			if k.ID == b.ChildID {
				session = k.VendorSessionID
			}
		}
	}
	lease, err := s.AcquireLease(ctx, "codex", session)
	if err != nil {
		t.Fatal(err)
	}

	var out, errb strings.Builder
	if code := rejectWith([]string{id}, f.env, &out, &errb); code != 1 || !strings.Contains(errb.String(), "open in another aggy") {
		t.Errorf("reject while open: code %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(wt, "a.go")); err != nil {
		t.Errorf("the worktree was touched: %v", err)
	}
	if code, _, errOut := accept(f, "", false, id, "--yes"); code != 1 || !strings.Contains(errOut, "open in another aggy") {
		t.Errorf("accept while open: code %d: %s", code, errOut)
	}

	if err := lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if code := rejectWith([]string{id}, f.env, &out, &errb); code != 0 {
		t.Errorf("reject after close: code %d: %s", code, errb.String())
	}
}
