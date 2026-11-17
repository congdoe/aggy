package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

// TestTurnSeenHere (T6982, #147 review, H1): a Claude switch before the first
// turn end goes in one step only where an earlier on-screen Claude session in
// the same folder ended a turn. A fresh folder, or one whose earlier session
// ended none, gives no such evidence, so the status line asks.
func TestTurnSeenHere(t *testing.T) {
	for _, c := range []struct {
		name    string
		earlier bool // an earlier on-screen session in the folder
		turned  bool // and it ended a turn
		want    bool
	}{
		{"a fresh folder", false, false, false},
		{"an earlier session with no turn end", true, false, false},
		{"an earlier session with a turn end", true, true, true},
		{"the home folder, an earlier turn end", true, true, false}, // Claude keeps no trust there
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			ctx := context.Background()
			s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state", store.FileName))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			dir := t.TempDir()
			if strings.HasPrefix(c.name, "the home folder") {
				dir = home
			}
			now := time.Now()
			par := store.Parent{ID: "p1", Dir: dir, CreatedAt: now, LastActive: now}
			if err := s.InsertParent(ctx, par); err != nil {
				t.Fatal(err)
			}
			if c.earlier {
				ch := store.Child{ID: "c1", ParentID: "p1", Vendor: string(adapter.Claude), Cwd: dir, CreatedAt: now, LastActive: now}
				if err := s.InsertChild(ctx, ch); err != nil {
					t.Fatal(err)
				}
				if c.turned {
					if err := s.SetChildTurnSeen(ctx, "c1"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if got := turnSeenHere(ctx, s, adapter.Claude, par); got != c.want {
				t.Errorf("turnSeenHere = %v, want %v", got, c.want)
			}
		})
	}
}
