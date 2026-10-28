package cli

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

func touch(t *testing.T, p string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestSessionFileLastChanged(t *testing.T) {
	started := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	mod := started.Add(3 * time.Hour).Truncate(time.Second)
	cwd := "/work/repo"
	tests := []struct {
		name    string
		vendor  adapter.Vendor
		id      string
		setup   func(t *testing.T, home, outside string)
		want    time.Time
		wantErr error // nil with a zero want means any error
	}{
		{
			name: "claude in any project folder", vendor: adapter.Claude, id: sidA,
			setup: func(t *testing.T, home, _ string) {
				touch(t, filepath.Join(home, ".claude", "projects", "-elsewhere", sidA+".jsonl"), mod)
			},
			want: mod,
		},
		{
			name: "claude missing", vendor: adapter.Claude, id: sidA,
			setup: func(t *testing.T, home, _ string) {
				touch(t, filepath.Join(home, ".claude", "projects", "-work", sidB+".jsonl"), mod)
			},
			wantErr: errNoSessionFile,
		},
		{
			name: "no vendor home", vendor: adapter.Claude, id: sidA,
			wantErr: errNoSessionFile,
		},
		{
			name: "claude through a symlinked project folder is not followed", vendor: adapter.Claude, id: sidA,
			setup: func(t *testing.T, home, outside string) {
				touch(t, filepath.Join(outside, sidA+".jsonl"), mod)
				root := filepath.Join(home, ".claude", "projects")
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, "-link")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: errNoSessionFile,
		},
		{
			name: "claude session file that is a symlink", vendor: adapter.Claude, id: sidA,
			setup: func(t *testing.T, home, outside string) {
				touch(t, filepath.Join(outside, "f"), mod)
				d := filepath.Join(home, ".claude", "projects", "-work")
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "f"), filepath.Join(d, sidA+".jsonl")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: errNoSessionFile,
		},
		{
			name: "codex rollout in its date folder", vendor: adapter.Codex, id: sidA,
			setup: func(t *testing.T, home, _ string) {
				d := started.Local()
				touch(t, filepath.Join(home, ".codex", "sessions", d.Format("2006"), d.Format("01"), d.Format("02"),
					"rollout-2026-10-27T12-00-00-"+sidA+".jsonl"), mod)
			},
			want: mod,
		},
		{
			name: "codex rollout of another thread", vendor: adapter.Codex, id: sidA,
			setup: func(t *testing.T, home, _ string) {
				d := started.UTC()
				touch(t, filepath.Join(home, ".codex", "sessions", d.Format("2006"), d.Format("01"), d.Format("02"),
					"rollout-2026-10-27T12-00-00-"+sidB+".jsonl"), mod)
			},
			wantErr: errNoSessionFile,
		},
		{
			name: "codex date folder that is a symlink", vendor: adapter.Codex, id: sidA,
			setup: func(t *testing.T, home, outside string) {
				touch(t, filepath.Join(outside, "rollout-x-"+sidA+".jsonl"), mod)
				d := started.UTC()
				month := filepath.Join(home, ".codex", "sessions", d.Format("2006"), d.Format("01"))
				if err := os.MkdirAll(month, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(month, d.Format("02"))); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: errNoSessionFile,
		},
		{
			name: "grok session folder, newest entry", vendor: adapter.Grok, id: sidA,
			setup: func(t *testing.T, home, _ string) {
				d := filepath.Join(home, ".grok", "sessions", url.QueryEscape(cwd), sidA)
				touch(t, filepath.Join(d, "a"), mod.Add(-time.Hour))
				touch(t, filepath.Join(d, "b"), mod)
				if err := os.Chtimes(d, mod.Add(-2*time.Hour), mod.Add(-2*time.Hour)); err != nil {
					t.Fatal(err)
				}
			},
			want: mod,
		},
		{
			name: "grok session under another cwd", vendor: adapter.Grok, id: sidA,
			setup: func(t *testing.T, home, _ string) {
				d := filepath.Join(home, ".grok", "sessions", "%2Fother", sidA)
				touch(t, filepath.Join(d, "a"), mod)
				if err := os.Chtimes(d, mod, mod); err != nil {
					t.Fatal(err)
				}
			},
			want: mod,
		},
		{
			name: "an id that is not a UUID never becomes a path", vendor: adapter.Claude, id: "../../x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, outside := t.TempDir(), t.TempDir()
			if tt.setup != nil {
				tt.setup(t, home, outside)
			}
			env := map[string]string{"HOME": home}
			f := &sessionFiles{getenv: func(k string) string { return env[k] }}
			got, err := f.LastChanged(tt.vendor, tt.id, cwd, started)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.want.IsZero():
				if err == nil {
					t.Error("want an error")
				}
			case err != nil || !got.Equal(tt.want):
				t.Errorf("LastChanged = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestSessionFileRelocated(t *testing.T) {
	dir := t.TempDir()
	mod := time.Now().Add(-time.Hour).Truncate(time.Second)
	touch(t, filepath.Join(dir, "projects", "-w", sidA+".jsonl"), mod)
	env := map[string]string{"HOME": t.TempDir(), "CLAUDE_CONFIG_DIR": dir}
	f := &sessionFiles{getenv: func(k string) string { return env[k] }}
	if got, err := f.LastChanged(adapter.Claude, sidA, "", time.Now()); err != nil || !got.Equal(mod) {
		t.Errorf("LastChanged = %v, %v", got, err)
	}
	env["CLAUDE_CONFIG_DIR"] = "relative"
	f = &sessionFiles{getenv: func(k string) string { return env[k] }}
	if _, err := f.LastChanged(adapter.Claude, sidA, "", time.Now()); err == nil || errors.Is(err, errNoSessionFile) {
		t.Errorf("a relative CLAUDE_CONFIG_DIR: err = %v", err)
	}
}

// TestRefreshChildren (T6663): last active follows the session file, a
// removed directory closes a child, a missing session file makes it gone
// unless a live process holds its lease.
func TestRefreshChildren(t *testing.T) {
	s := sessionStore(t)
	ctx := context.Background()
	dir, removed := t.TempDir(), filepath.Join(t.TempDir(), "worktree")
	const p = "p0000000-0000-4000-8000-000000000000"
	const sidC = "cccccccc-0000-4000-8000-000000000000"
	seedParent(t, s, p, dir,
		store.Child{ID: "active", Vendor: "claude", VendorSessionID: sidA},
		store.Child{ID: "wt", Vendor: "claude", VendorSessionID: sidB, Cwd: removed},
		store.Child{ID: "missing", Vendor: "grok", VendorSessionID: sidC},
		store.Child{ID: "held", Vendor: "claude", VendorSessionID: "dddddddd-0000-4000-8000-000000000000"},
		store.Child{ID: "codex", Vendor: "codex"},
		store.Child{ID: "borrow", Vendor: "grok", Kind: "research", Cwd: removed})
	mod := time.Now().Add(time.Minute).Truncate(time.Millisecond)
	claudeSession(t, sidA, mod)
	claudeSession(t, sidB, mod)
	lease, err := s.AcquireLease(ctx, "claude", "dddddddd-0000-4000-8000-000000000000")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release(ctx) }()

	cs, err := refreshChildren(ctx, s, p, &sessionFiles{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.Child{}
	for _, c := range cs {
		got[c.ID] = c
	}
	want := map[string]string{"active": "open", "wt": "closed", "missing": "gone", "held": "open", "codex": "open", "borrow": "open"}
	for id, st := range want {
		if got[id].Status != st {
			t.Errorf("%s status = %q, want %q", id, got[id].Status, st)
		}
	}
	if !got["active"].LastActive.Equal(mod) || cs[0].ID != "active" {
		t.Errorf("active last active = %v, want %v (first: %s)", got["active"].LastActive, mod, cs[0].ID)
	}
	par, _ := s.GetParent(ctx, p)
	if !par.LastActive.Equal(mod) {
		t.Errorf("parent last active = %v, want %v", par.LastActive, mod)
	}
	// An older file never lowers the time.
	claudeSession(t, sidA, mod.Add(-time.Hour))
	if cs, _ = refreshChildren(ctx, s, p, &sessionFiles{}); !cs[0].LastActive.Equal(mod) {
		t.Errorf("last active fell to %v", cs[0].LastActive)
	}
}
