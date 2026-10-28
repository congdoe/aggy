package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// sessionStore is a store in a temp AGGY_HOME, with HOME a temp dir.
func sessionStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", filepath.Join(t.TempDir(), "aggy"))
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		t.Setenv(k, "")
	}
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

const (
	sidA = "11111111-2222-4333-8444-555555555555"
	sidB = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

// seedParent adds a parent in dir with the given interactive children,
// oldest first.
func seedParent(t *testing.T, s *store.Store, id, dir string, children ...store.Child) {
	t.Helper()
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour)
	if err := s.InsertParent(ctx, store.Parent{ID: id, Dir: dir, CreatedAt: t0, LastActive: t0}); err != nil {
		t.Fatal(err)
	}
	for i, c := range children {
		c.ParentID = id
		if c.Cwd == "" {
			c.Cwd = dir
		}
		c.CreatedAt = t0.Add(time.Duration(i) * time.Minute)
		c.LastActive = c.CreatedAt
		if err := s.InsertChild(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
}

// claudeSession writes the session file Claude keeps for id under the
// temp HOME, modified at mod.
func claudeSession(t *testing.T, id string, mod time.Time) string {
	t.Helper()
	p := filepath.Join(os.Getenv("HOME"), ".claude", "projects", "-work", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlan(t *testing.T) {
	cfg := config.Default()
	cfg.DefaultVendor = adapter.Claude
	tests := []struct {
		name  string
		seed  func(t *testing.T, s *store.Store, dir string)
		tgt   target
		check func(t *testing.T, p launchPlan, dir string)
		want  string // error text; "" means none
		note  string // stderr text
	}{
		{
			name: "new with a vendor",
			tgt:  target{New: true, Vendor: adapter.Grok},
			check: func(t *testing.T, p launchPlan, dir string) {
				if !p.newParent || !p.newChild || p.child.Vendor != "grok" || p.resume != "" ||
					!adapter.IsUUID(p.child.VendorSessionID) || p.parent.Dir != dir || p.child.Cwd != dir {
					t.Errorf("plan = %+v", p)
				}
			},
		},
		{
			name: "bare with no parent here starts one on the default vendor",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "99999999-0000-4000-8000-000000000000", t.TempDir(),
					store.Child{ID: "x", Vendor: "grok", VendorSessionID: sidB})
			},
			check: func(t *testing.T, p launchPlan, dir string) {
				if !p.newParent || p.child.Vendor != "claude" || p.parent.Dir != dir {
					t.Errorf("plan = %+v", p)
				}
			},
		},
		{
			name: "bare resumes the child last on screen",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "p0000000-0000-4000-8000-000000000000", dir,
					store.Child{ID: "old", Vendor: "grok", VendorSessionID: sidB},
					store.Child{ID: "borrow", Vendor: "codex", Kind: "research"},
					store.Child{ID: "last", Vendor: "claude", VendorSessionID: sidA})
				claudeSession(t, sidA, time.Now().Add(-time.Hour))
				// A borrow is newer, but only interactive children resume.
				_ = s.SetChildActivity(context.Background(), "borrow", time.Now())
			},
			check: func(t *testing.T, p launchPlan, dir string) {
				if p.newParent || p.newChild || p.child.ID != "last" || p.resume != sidA {
					t.Errorf("plan = %+v", p)
				}
				req := p.interactiveRequest()
				if req.Resume != sidA || req.SessionID != "" || req.BriefFile != "" || req.Cwd != dir {
					t.Errorf("request = %+v", req)
				}
			},
		},
		{
			name: "a gone child starts a new one of its vendor",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "p0000000-0000-4000-8000-000000000000", dir,
					store.Child{ID: "g", Vendor: "grok", VendorSessionID: sidB})
				// Grok's layout is recorded: its sessions folder is there
				// and holds nothing for sidB.
				if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".grok", "sessions"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, p launchPlan, dir string) {
				if p.newParent || !p.newChild || p.child.Vendor != "grok" || p.resume != "" {
					t.Errorf("plan = %+v", p)
				}
			},
			note: "the vendor has no saved session for it",
		},
		{
			name: "a claude session file aggy saw once and cannot find now is unknown, and still resumed",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "p0000000-0000-4000-8000-000000000000", dir,
					store.Child{ID: "c", Vendor: "claude", VendorSessionID: sidA})
				if err := s.SetChildFileSeen(context.Background(), "c"); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".claude", "projects", "-x"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, p launchPlan, dir string) {
				if p.newChild || p.resume != sidA {
					t.Errorf("plan = %+v", p)
				}
			},
		},
		{
			// D23-7: started with --session-id and quit before the first
			// message, so Claude never wrote the file; it must not be
			// resumed for good.
			name: "a claude session never saved starts a new one",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "p0000000-0000-4000-8000-000000000000", dir,
					store.Child{ID: "c", Vendor: "claude", VendorSessionID: sidA})
				if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".claude", "projects", "-x"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, p launchPlan, dir string) {
				if !p.newChild || p.child.Vendor != "claude" || p.resume != "" {
					t.Errorf("plan = %+v", p)
				}
			},
			note: "the vendor has no saved session for it",
		},
		{
			name: "a child whose directory was removed starts a new one",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "p0000000-0000-4000-8000-000000000000", dir,
					store.Child{ID: "c", Vendor: "claude", VendorSessionID: sidA, Cwd: filepath.Join(dir, "wt")})
				claudeSession(t, sidA, time.Now())
			},
			check: func(t *testing.T, p launchPlan, dir string) {
				if !p.newChild || p.child.Vendor != "claude" || p.child.Cwd != dir {
					t.Errorf("plan = %+v", p)
				}
			},
			note: "its directory was removed",
		},
		{
			name: "a re-cloned directory starts a new parent",
			seed: func(t *testing.T, s *store.Store, dir string) {
				key, _ := safefs.CanonicalPath(dir)
				if err := s.InsertParent(context.Background(), store.Parent{ID: "abc00000-0000-4000-8000-000000000000",
					Dir: dir, DirKey: key, DirID: "1:1", CreatedAt: time.Now(), LastActive: time.Now()}); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, p launchPlan, dir string) {
				if !p.newParent || p.parent.DirID != dirID(dir) || p.parent.DirID == "" {
					t.Errorf("plan = %+v", p)
				}
			},
			note: "aggy resume abc00000 reopens the old one",
		},
		{
			name: "a codex child without an id starts a new codex child",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "p0000000-0000-4000-8000-000000000000", dir,
					store.Child{ID: "c", Vendor: "codex"})
			},
			check: func(t *testing.T, p launchPlan, dir string) {
				if !p.newChild || p.child.Vendor != "codex" || p.child.VendorSessionID != "" {
					t.Errorf("plan = %+v", p)
				}
				if req := p.interactiveRequest(); req.SessionID != "" || req.Resume != "" {
					t.Errorf("request = %+v", req)
				}
			},
			note: "no recorded session id",
		},
		{
			name: "an id from the index that is not a UUID is refused",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "p0000000-0000-4000-8000-000000000000", dir,
					store.Child{ID: "c", Vendor: "claude", VendorSessionID: "--dangerously-skip-permissions"})
			},
			want: "invalid id",
		},
		{
			name: "resume by prefix",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "abcdef00-0000-4000-8000-000000000000", t.TempDir(),
					store.Child{ID: "c", Vendor: "claude", VendorSessionID: sidA})
				claudeSession(t, sidA, time.Now().Add(-time.Hour))
			},
			tgt: target{ParentID: "abcdef"},
			check: func(t *testing.T, p launchPlan, dir string) {
				if p.resume != sidA || p.child.Cwd == dir {
					t.Errorf("plan = %+v", p)
				}
			},
		},
		{
			name: "unknown prefix",
			tgt:  target{ParentID: "abc"},
			want: "no session abc",
		},
		{
			name: "ambiguous prefix",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "abc00000-0000-4000-8000-000000000000", dir)
				seedParent(t, s, "abc11111-0000-4000-8000-000000000000", dir)
			},
			tgt:  target{ParentID: "abc"},
			want: "2 sessions start with abc",
		},
		{
			name: "a parent whose directory is gone",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "abc00000-0000-4000-8000-000000000000", filepath.Join(dir, "removed"))
			},
			tgt:  target{ParentID: "abc0"},
			want: "no longer exists",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := sessionStore(t)
			dir := t.TempDir()
			if tt.seed != nil {
				tt.seed(t, s, dir)
			}
			var errb bytes.Buffer
			p, err := plan(context.Background(), s, cfg, dir, tt.tgt, time.Now(), &errb)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("err = %v, want %q", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(errb.String(), tt.note) {
				t.Errorf("stderr = %q, want %q", errb.String(), tt.note)
			}
			tt.check(t, p, dir)
		})
	}
}

// TestCommitLease: a launch takes the vendor session's lease, and one
// that finds it held by a live process is refused with a clear message.
func TestCommitLease(t *testing.T) {
	s := sessionStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	seedParent(t, s, "p0000000-0000-4000-8000-000000000000", dir,
		store.Child{ID: "c", Vendor: "claude", VendorSessionID: sidA})
	claudeSession(t, sidA, time.Now().Add(-time.Hour))
	cfg := config.Default()
	p, err := plan(ctx, s, cfg, dir, target{}, time.Now(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := p.commit(ctx, s, time.Now())
	if err != nil || lease == nil {
		t.Fatalf("commit = %v, %v", lease, err)
	}
	if _, err := p.commit(ctx, s, time.Now()); err == nil || !strings.Contains(err.Error(), "already open in another aggy") {
		t.Errorf("second commit err = %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if pid, alive, _ := s.LeaseHolder(ctx, "claude", sidA); alive {
		t.Errorf("lease still held by %d", pid)
	}

	// A new parent records its rows, and the parent's last vendor.
	p, err = plan(ctx, s, cfg, dir, target{New: true, Vendor: adapter.Codex}, time.Now(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	lease, err = p.commit(ctx, s, time.Now())
	if err != nil || lease != nil {
		t.Fatalf("codex commit = %v, %v; a codex child has no id to lease", lease, err)
	}
	got, err := s.LatestParentInDir(ctx, canon(t, dir), dir)
	if err != nil || got.ID != p.parent.ID || got.LastVendor != "codex" {
		t.Errorf("latest parent = %+v, %v", got, err)
	}
}

// TestPlanCanonicalDir (D23-3): another spelling of the directory (here
// through a symlink) finds the same parent.
func TestPlanCanonicalDir(t *testing.T) {
	s := sessionStore(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DefaultVendor = adapter.Grok
	ctx := context.Background()
	p, err := plan(ctx, s, cfg, real, target{New: true}, time.Now(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := p.commit(ctx, s, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release(ctx) })
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".grok", "sessions", "x", p.child.VendorSessionID), 0o700); err != nil {
		t.Fatal(err)
	}
	again, err := plan(ctx, s, cfg, link, target{}, time.Now(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if again.newParent || again.parent.ID != p.parent.ID || again.resume != p.child.VendorSessionID {
		t.Errorf("from the symlink: plan = %+v, want parent %s resumed", again, p.parent.ID)
	}
	if p.child.VendorHome != filepath.Join(os.Getenv("HOME"), ".grok") {
		t.Errorf("vendor home = %q", p.child.VendorHome)
	}
}

func canon(t *testing.T, dir string) string {
	t.Helper()
	k, err := safefs.CanonicalPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestChildIDs (F23-2, F23-3): each vendor's id reaches the child row.
// Claude and Grok: the id the launch assigned, recorded at once, with
// the launch's lease. Codex: none at launch; the id the session index
// confirms is recorded first-wins, and its lease is taken then and held
// until the CLI exits.
func TestChildIDs(t *testing.T) {
	ctx := context.Background()
	for _, v := range []adapter.Vendor{adapter.Claude, adapter.Grok, adapter.Codex} {
		t.Run(string(v), func(t *testing.T) {
			s := sessionStore(t)
			p, err := plan(ctx, s, config.Default(), t.TempDir(), target{New: true, Vendor: v}, time.Now(), &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			lease, err := p.commit(ctx, s, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			ids := &childIDs{s: s, child: p.child}
			var got []string
			onID := func(id string) { got = append(got, id); ids.set(id) }
			x := newSessionIndex(p.parent.ID, v)
			x.record = func(store.Event) error { return nil }
			x.verify = func(string, string) rolloutVerdict { return rolloutTUI }
			x.onID = onID
			want := p.child.VendorSessionID
			if v == adapter.Codex {
				if want != "" || lease != nil {
					t.Fatalf("codex child has id %q at launch", want)
				}
				want = idA
				x.hook(hookReq(v, "SessionStart", `{"session_id":"`+idA+`","source":"startup"}`))
				x.saw(rolls(idA))
				// A /clear or nested session's later id never moves it.
				ids.set(idB)
			} else {
				x.set(want, "", "assigned")
				x.hook(hookReq(v, "SessionStart", `{"session_id":"`+idB+`","sessionId":"`+idB+`","source":"clear"}`))
			}
			if len(got) != 1 || got[0] != want {
				t.Errorf("onID got %v, want [%s]", got, want)
			}
			cs, _ := s.Children(ctx, p.parent.ID)
			if len(cs) != 1 || cs[0].VendorSessionID != want {
				t.Fatalf("child row = %+v, want session %s", cs, want)
			}
			if _, alive, _ := s.LeaseHolder(ctx, string(v), want); !alive {
				t.Errorf("%s session %s is not leased while on screen", v, want)
			}
			ids.release(ctx)
			if lease != nil {
				_ = lease.Release(ctx)
			}
			if _, alive, _ := s.LeaseHolder(ctx, string(v), want); alive {
				t.Error("lease outlived the CLI")
			}
		})
	}
}
