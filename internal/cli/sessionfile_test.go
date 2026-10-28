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
			// The layout is unverified: a miss is never "gone".
			wantErr: errUnknownSession,
		},
		{
			name: "no vendor home", vendor: adapter.Grok, id: sidA,
			wantErr: errUnknownSession,
		},
		{
			name: "claude in the cwd's folder", vendor: adapter.Claude, id: sidA,
			setup: func(t *testing.T, home, _ string) {
				touch(t, filepath.Join(home, ".claude", "projects", "-work-repo", sidA+".jsonl"), mod)
			},
			want: mod,
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
			wantErr: errUnknownSession,
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
			wantErr: errUnknownSession,
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
			name: "codex rollout first saved days after the start", vendor: adapter.Codex, id: sidA,
			setup: func(t *testing.T, home, _ string) {
				d := started.Add(9 * 24 * time.Hour).UTC()
				touch(t, filepath.Join(home, ".codex", "sessions", d.Format("2006"), d.Format("01"), d.Format("02"),
					"rollout-x-"+sidA+".jsonl"), mod)
			},
			want: mod,
		},
		{
			name: "codex rollout after now is not searched", vendor: adapter.Codex, id: sidA,
			setup: func(t *testing.T, home, _ string) {
				d := started.Add(40 * 24 * time.Hour).UTC()
				touch(t, filepath.Join(home, ".codex", "sessions", d.Format("2006"), d.Format("01"), d.Format("02"),
					"rollout-x-"+sidA+".jsonl"), mod)
			},
			wantErr: errNoSessionFile,
		},
		{
			name: "grok encoded-cwd folder that is a symlink", vendor: adapter.Grok, id: sidA,
			setup: func(t *testing.T, home, outside string) {
				touch(t, filepath.Join(outside, sidA, "a"), mod)
				root := filepath.Join(home, ".grok", "sessions")
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, url.QueryEscape(cwd))); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: errNoSessionFile,
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
			f := &sessionFiles{getenv: func(k string) string { return env[k] },
				now: func() time.Time { return started.Add(10 * 24 * time.Hour) }}
			got, err := f.LastChanged(tt.vendor, tt.id, "", cwd, started)
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
	if got, err := f.LastChanged(adapter.Claude, sidA, "", "", time.Now()); err != nil || !got.Equal(mod) {
		t.Errorf("LastChanged = %v, %v", got, err)
	}
	env["CLAUDE_CONFIG_DIR"] = "relative"
	f = &sessionFiles{getenv: func(k string) string { return env[k] }}
	if _, err := f.LastChanged(adapter.Claude, sidA, "", "", time.Now()); err == nil || errors.Is(err, errNoSessionFile) {
		t.Errorf("a relative CLAUDE_CONFIG_DIR: err = %v", err)
	}
}

// TestRefreshChildren (T6663, D23-1): last active follows the session
// file; closed, gone, unknown, and running are worked out each time and
// never stored; a live lease is never marked; a child's recorded vendor
// home is where its file is looked for.
func TestRefreshChildren(t *testing.T) {
	s := sessionStore(t)
	ctx := context.Background()
	dir, removed := t.TempDir(), filepath.Join(t.TempDir(), "worktree")
	otherHome := t.TempDir()
	const p = "p0000000-0000-4000-8000-000000000000"
	const (
		sidC = "cccccccc-0000-4000-8000-000000000000"
		sidD = "dddddddd-0000-4000-8000-000000000000"
		sidE = "eeeeeeee-0000-4000-8000-000000000000"
		sidF = "ffffffff-0000-4000-8000-000000000000"
	)
	seedParent(t, s, p, dir,
		store.Child{ID: "active", Vendor: "claude", VendorSessionID: sidA},
		store.Child{ID: "wt", Vendor: "claude", VendorSessionID: sidB, Cwd: removed},
		store.Child{ID: "missing", Vendor: "grok", VendorSessionID: sidC},
		store.Child{ID: "held", Vendor: "grok", VendorSessionID: sidD, Cwd: removed},
		store.Child{ID: "unsure", Vendor: "claude", VendorSessionID: sidE},
		store.Child{ID: "elsewhere", Vendor: "claude", VendorSessionID: sidF, VendorHome: otherHome},
		store.Child{ID: "codex", Vendor: "codex"},
		store.Child{ID: "borrow", Vendor: "grok", Kind: "research", Cwd: removed})
	mod := time.Now().Add(time.Minute).Truncate(time.Millisecond)
	claudeSession(t, sidA, mod)
	claudeSession(t, sidB, mod)
	touch(t, filepath.Join(otherHome, "projects", "-w", sidF+".jsonl"), mod.Add(-time.Second))
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".grok", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireLease(ctx, "grok", sidD)
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
	want := map[string]string{"active": "open", "wt": "closed", "missing": "gone", "held": "running",
		"unsure": "unknown", "elsewhere": "open", "codex": "open", "borrow": "open"}
	for id, st := range want {
		if got[id].Status != st {
			t.Errorf("%s status = %q, want %q", id, got[id].Status, st)
		}
	}
	if !got["active"].LastActive.Equal(mod) || cs[0].ID != "active" {
		t.Errorf("active last active = %v, want %v (first: %s)", got["active"].LastActive, mod, cs[0].ID)
	}
	if !got["elsewhere"].LastActive.Equal(mod.Add(-time.Second)) {
		t.Errorf("elsewhere last active = %v: the recorded vendor home was not used", got["elsewhere"].LastActive)
	}
	par, _ := s.GetParent(ctx, p)
	if !par.LastActive.Equal(mod) {
		t.Errorf("parent last active = %v, want %v", par.LastActive, mod)
	}
	// Nothing but times was stored: a removed directory that comes back
	// (a disk mounted again) makes the child open again.
	raw, _ := s.Children(ctx, p)
	for _, c := range raw {
		if c.Status != "open" {
			t.Errorf("%s stored status = %q", c.ID, c.Status)
		}
	}
	if err := os.MkdirAll(removed, 0o700); err != nil {
		t.Fatal(err)
	}
	cs, _ = refreshChildren(ctx, s, p, &sessionFiles{})
	for _, c := range cs {
		if c.ID == "wt" && c.Status != "open" {
			t.Errorf("wt after its directory came back = %q", c.Status)
		}
	}
	// An older file never lowers the time.
	claudeSession(t, sidA, mod.Add(-time.Hour))
	cs, _ = refreshChildren(ctx, s, p, &sessionFiles{})
	for _, c := range cs {
		if c.ID == "active" && !c.LastActive.Equal(mod) {
			t.Errorf("last active fell to %v", c.LastActive)
		}
	}
}

func TestClaudeProjectDir(t *testing.T) {
	for in, want := range map[string]string{"/work/repo": "-work-repo", "/a b/c.d_e": "-a-b-c-d-e"} {
		if got := claudeProjectDir(in); got != want {
			t.Errorf("claudeProjectDir(%q) = %q, want %q", in, got, want)
		}
	}
}
