package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
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
			name: "resume with no parent here starts one on the default vendor",
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
			name: "resume reopens the child last on screen",
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
				if !slices.Equal(req.Env, []string{"AGGY_VENDOR=claude", "AGGY_TRACE=" + dlog.Trace()}) {
					t.Errorf("env = %q, want AGGY_VENDOR and AGGY_TRACE only (no model set)", req.Env)
				}
			},
		},
		{
			// T6664: a switch puts the parent's latest child of that
			// vendor on screen, not the latest child.
			name: "a switch resumes the latest child of its vendor",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "c0000000-0000-4000-8000-000000000000", dir,
					store.Child{ID: "c", Vendor: "claude", VendorSessionID: sidA},
					store.Child{ID: "last", Vendor: "grok", VendorSessionID: sidB})
				claudeSession(t, sidA, time.Now().Add(-time.Hour))
			},
			tgt: target{ParentID: "c0000000", Vendor: adapter.Claude},
			check: func(t *testing.T, p launchPlan, dir string) {
				if p.newParent || p.newChild || p.child.ID != "c" || p.resume != sidA {
					t.Errorf("plan = %+v", p)
				}
			},
		},
		{
			// T6665: --new from the picker starts another child even
			// though one of that vendor exists.
			name: "a switch with new starts another child",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "c0000000-0000-4000-8000-000000000000", dir,
					store.Child{ID: "c", Vendor: "claude", VendorSessionID: sidA})
				claudeSession(t, sidA, time.Now())
			},
			tgt: target{ParentID: "c0000000", Vendor: adapter.Claude, NewChild: true, Model: "sonnet-x"},
			check: func(t *testing.T, p launchPlan, dir string) {
				// O56-7: the one-launch model is on the child before its
				// row is committed.
				if !p.newChild || p.child.Vendor != "claude" || p.child.ID == "c" || p.child.Model != "sonnet-x" {
					t.Errorf("plan = %+v", p)
				}
			},
		},
		{
			name: "a switch to a picked child resumes it, not the latest",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "c0000000-0000-4000-8000-000000000000", dir,
					store.Child{ID: "old", Vendor: "claude", VendorSessionID: sidA},
					store.Child{ID: "new", Vendor: "claude", VendorSessionID: sidB})
				claudeSession(t, sidA, time.Now().Add(-time.Hour))
				claudeSession(t, sidB, time.Now())
			},
			tgt: target{ParentID: "c0000000", Vendor: adapter.Claude, Child: "old"},
			check: func(t *testing.T, p launchPlan, dir string) {
				if p.newChild || p.child.ID != "old" || p.resume != sidA {
					t.Errorf("plan = %+v", p)
				}
			},
		},
		{
			name: "a switch to a vendor with no child here starts one",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "c0000000-0000-4000-8000-000000000000", dir,
					store.Child{ID: "c", Vendor: "claude", VendorSessionID: sidA})
				claudeSession(t, sidA, time.Now())
			},
			tgt: target{ParentID: "c0000000", Vendor: adapter.Codex},
			check: func(t *testing.T, p launchPlan, dir string) {
				if p.newParent || !p.newChild || p.child.Vendor != "codex" || p.parent.ID != "c0000000-0000-4000-8000-000000000000" {
					t.Errorf("plan = %+v", p)
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
			name: "a prefix of a parent and a child is ambiguous",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "abc00000-0000-4000-8000-000000000000", dir)
				seedParent(t, s, "p0000000-0000-4000-8000-000000000000", dir,
					store.Child{ID: "abc99999-0000-4000-8000-000000000000", Vendor: "claude", VendorSessionID: sidA})
			},
			tgt:  target{ParentID: "abc"},
			want: "2 sessions start with abc",
		},
		{
			name: "a picked child missing after refresh is an error",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "p0000000-0000-4000-8000-000000000000", dir,
					store.Child{ID: "abc99999-0000-4000-8000-000000000000", Vendor: "claude", VendorSessionID: sidA})
				orig := planRefresh
				t.Cleanup(func() { planRefresh = orig })
				planRefresh = func(context.Context, *store.Store, string, *sessionFiles) ([]store.Child, error) { return nil, nil }
			},
			tgt:  target{ParentID: "abc9"},
			want: "session abc99999 is no longer recorded",
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

// TestChildEnv (T6692, T6753): a child gets AGGY_VENDOR, AGGY_MODEL only
// when a model is set, and this run's AGGY_TRACE.
func TestChildEnv(t *testing.T) {
	trace := "AGGY_TRACE=" + dlog.Trace()
	if got := childEnv("codex", "gpt-5"); !slices.Equal(got, []string{"AGGY_VENDOR=codex", "AGGY_MODEL=gpt-5", trace}) {
		t.Errorf("env = %q", got)
	}
	if got := childEnv("grok", ""); !slices.Equal(got, []string{"AGGY_VENDOR=grok", trace}) {
		t.Errorf("env = %q", got)
	}
}

// TestPlanBadModel (O56-7): plan checks the one-launch model before
// anything else, as it may have come over the socket.
func TestPlanBadModel(t *testing.T) {
	if _, err := plan(context.Background(), nil, config.Config{}, t.TempDir(), target{Model: "a b"}, time.Now(), io.Discard); err == nil {
		t.Error("a bad model was planned")
	}
}

// TestPlanSkipsUnstartedChild (T6899): a child that never started does not count
// as the parent's latest, nor its parent as the directory's, when anything
// else there has started. Named by id it still goes on screen; alone it does
// too, with the usual note.
func TestPlanSkipsUnstartedChild(t *testing.T) {
	ctx, s, dir, claudeID := unstartedWorld(t)
	got, err := plan(ctx, s, config.Config{}, dir, target{}, time.Now(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got.child.Vendor != "claude" || got.resume != claudeID {
		t.Errorf("latest = %s %s, want the claude child %s", got.child.Vendor, got.resume, claudeID)
	}
	// A later parent that holds only a child that never started hides nothing.
	later := time.Now().Add(time.Hour)
	if err := s.InsertParent(ctx, store.Parent{ID: "bb22", Dir: dir, DirKey: mustCanon(t, dir), CreatedAt: later, LastActive: later}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChild(ctx, store.Child{ID: "c-later", ParentID: "bb22", Vendor: "grok", VendorSessionID: "99999999-2222-4333-8444-555555555555", Cwd: dir, Status: "open", CreatedAt: later, LastActive: later}); err != nil {
		t.Fatal(err)
	}
	if got, err = plan(ctx, s, config.Config{}, dir, target{}, time.Now(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if got.parent.ID != "aa11" || got.resume != claudeID {
		t.Errorf("reopened parent %s session %s, want aa11 %s", got.parent.ID, got.resume, claudeID)
	}
	// A switch to the vendor whose only child never started starts a new one.
	if got, err = plan(ctx, s, config.Config{}, dir, target{ParentID: "aa11", Vendor: adapter.Grok}, time.Now(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if !got.newChild || got.child.Vendor != "grok" {
		t.Errorf("switch to grok: new %v vendor %s", got.newChild, got.child.Vendor)
	}
}

// unstartedWorld is a parent aa11 in a temp HOME with a Claude child that has a
// session file and a later Grok child that has none and never had.
func unstartedWorld(t *testing.T) (context.Context, *store.Store, string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("GROK_HOME", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("AGGY_HOME", filepath.Join(home, ".aggy"))
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	const claudeID, grokID = "11111111-2222-4333-8444-555555555555", "22222222-2222-4333-8444-555555555555"
	now := time.Now()
	if err := s.InsertParent(ctx, store.Parent{ID: "aa11", Dir: dir, DirKey: mustCanon(t, dir), LastVendor: "grok", CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
	for i, c := range []store.Child{{ID: "c-claude", Vendor: "claude", VendorSessionID: claudeID}, {ID: "c-grok", Vendor: "grok", VendorSessionID: grokID}} {
		at := now.Add(time.Duration(i) * time.Minute)
		c.ParentID, c.Cwd, c.Status, c.CreatedAt, c.LastActive = "aa11", dir, "open", at, at
		if err := s.InsertChild(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	proj := filepath.Join(home, ".claude", "projects", claudeProjectDir(dir))
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, claudeID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".grok", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	return ctx, s, dir, claudeID
}

func mustCanon(t *testing.T, dir string) string {
	t.Helper()
	key, err := safefs.CanonicalPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// TestChildStarted (T6899) is the evidence that a CLI got past its dialogs.
func TestChildStarted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GROK_HOME", "")
	const id = "22222222-2222-4333-8444-555555555555"
	cwd := t.TempDir()
	dir := filepath.Join(home, ".grok", "sessions", grok.EncodeCwd(cwd), id)
	child := store.Child{Vendor: "grok", Cwd: cwd}
	now := time.Now()
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	check := func(what string, got, want startEvidence) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}
	facts := supervisor.RunFacts{}
	check("no file, a folder to look in", childStarted(facts, child, id, true, now, nil, &sessionFiles{}), startNever)
	check("a counted turn", childStarted(supervisor.RunFacts{TurnRan: true}, child, id, true, now, nil, &sessionFiles{}), startYes)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	check("a file since the launch", childStarted(facts, child, id, false, now, nil, &sessionFiles{}), startYes)
	check("a resumed session's old file", childStarted(facts, child, id, false, now.Add(time.Hour), nil, &sessionFiles{}), startNever)
	check("a new child's file, whatever the clock", childStarted(facts, child, id, true, now.Add(time.Hour), nil, &sessionFiles{}), startYes)
	codex := store.Child{Vendor: "codex", Cwd: cwd}
	check("no rollout, watched", childStarted(facts, codex, "", true, now, func() (bool, bool) { return false, true }, &sessionFiles{}), startNever)
	check("a new rollout", childStarted(facts, codex, "", true, now, func() (bool, bool) { return true, true }, &sessionFiles{}), startYes)
	check("no watch", childStarted(facts, codex, "", true, now, func() (bool, bool) { return false, false }, &sessionFiles{}), startUnknown)
}

// TestSawRolloutLooksAgain (T6899): a rollout written just before the CLI exits,
// between two polls, still counts; one there before the launch does not.
func TestSawRolloutLooksAgain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	now := time.Now()
	day := filepath.Join(dir, now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	const old, fresh = "019a0000-0000-7000-8000-000000000001", "019a0000-0000-7000-8000-000000000002"
	write := func(id string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(day, "rollout-2026-01-01T00-00-00-"+id+".jsonl"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(old)
	x := newSessionIndex("p", adapter.Codex)
	x.watchDir, x.watchStart, x.watchBefore = dir, now, codexRollouts(dir, now, now)
	if len(x.watchBefore) != 1 {
		t.Fatalf("before = %v", x.watchBefore)
	}
	if seen, known := x.sawRollout(); seen || !known {
		t.Errorf("a rollout from before the launch: seen %v, known %v", seen, known)
	}
	write(fresh)
	if seen, _ := x.sawRollout(); !seen {
		t.Error("a new rollout did not count")
	}
}

// TestChildIDsSettle (T6901): the moved id reaches the row only with a session
// file behind it, keeps the row's lease, and for Codex only as the TUI's rollout.
func TestChildIDsSettle(t *testing.T) {
	const oldID, newID = "11111111-2222-4333-8444-555555555555", "99999999-2222-4333-8444-555555555555"
	const codexOld, codexNew = "019a0000-0000-7000-8000-000000000001", "019a0000-0000-7000-8000-000000000002"
	setup := func(t *testing.T, vendor, id string) (*store.Store, store.Child, string) {
		t.Helper()
		s := sessionStore(t)
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := s.InsertParent(context.Background(), store.Parent{ID: "aa11", Dir: dir, CreatedAt: now, LastActive: now}); err != nil {
			t.Fatal(err)
		}
		c := store.Child{ID: "c1", ParentID: "aa11", Vendor: vendor, VendorSessionID: id, Cwd: dir, Status: "open", CreatedAt: now, LastActive: now}
		if err := s.InsertChild(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		return s, c, os.Getenv("HOME")
	}
	idOf := func(t *testing.T, s *store.Store) string {
		t.Helper()
		cs, err := s.Children(context.Background(), "aa11")
		if err != nil || len(cs) != 1 {
			t.Fatalf("children: %+v, %v", cs, err)
		}
		return cs[0].VendorSessionID
	}
	ctx := context.Background()

	t.Run("no file leaves the row", func(t *testing.T) {
		s, c, home := setup(t, "claude", oldID)
		if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o700); err != nil {
			t.Fatal(err)
		}
		ids := &childIDs{s: s, child: c}
		ids.follow(newID, "clear")
		if ids.current() != newID {
			t.Errorf("current = %q", ids.current())
		}
		ids.settle(ctx, nil, &sessionFiles{})
		if got := idOf(t, s); got != oldID {
			t.Errorf("row = %q, want the old id", got)
		}
	})
	t.Run("a file moves the row and its lease", func(t *testing.T) {
		s, c, home := setup(t, "claude", oldID)
		proj := filepath.Join(home, ".claude", "projects", claudeProjectDir(c.Cwd))
		if err := os.MkdirAll(proj, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proj, newID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		l, err := s.AcquireLease(ctx, "claude", oldID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Release(ctx) }()
		var events []store.Event
		ids := &childIDs{s: s, child: c}
		ids.follow(newID, "clear")
		ids.settle(ctx, func(e store.Event) error { events = append(events, e); return nil }, &sessionFiles{})
		if got := idOf(t, s); got != newID {
			t.Errorf("row = %q, want the new id", got)
		}
		if _, alive, _ := s.LeaseHolder(ctx, "claude", newID); !alive {
			t.Error("the lease did not move")
		}
		if len(events) != 1 {
			t.Errorf("events = %+v", events)
		}
	})
	t.Run("never moves without a follow", func(t *testing.T) {
		s, c, _ := setup(t, "claude", oldID)
		(&childIDs{s: s, child: c}).settle(ctx, nil, &sessionFiles{})
		if got := idOf(t, s); got != oldID {
			t.Errorf("row = %q", got)
		}
	})
	for _, tt := range []struct {
		name, source, want string
	}{
		{"the TUI's rollout moves it", "cli", codexNew},
		{"a nested codex exec's never does", "exec", codexOld},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, c, home := setup(t, "codex", codexOld)
			meta := `{"type":"session_meta","payload":{"id":"` + codexNew + `","source":"` + tt.source + `","cwd":"` + c.Cwd + `"}}`
			now := time.Now().UTC()
			day := filepath.Join(home, ".codex", "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
			if err := os.MkdirAll(day, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(day, "rollout-2026-01-01T00-00-00-"+codexNew+".jsonl"), []byte(meta+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			ids := &childIDs{s: s, child: c}
			ids.follow(codexNew, "clear")
			ids.settle(ctx, nil, &sessionFiles{})
			if got := idOf(t, s); got != tt.want {
				t.Errorf("row = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFromSwitch: only a switch's launch has a CLI to go back to.
func TestFromSwitch(t *testing.T) {
	for _, tt := range []struct {
		t    target
		want bool
	}{
		{target{}, false},
		{target{New: true, Vendor: adapter.Grok}, false},
		{target{ParentID: "p"}, false},
		{target{ParentID: "p", Vendor: adapter.Grok}, true},
		{target{ParentID: "p", Vendor: adapter.Grok, SameChild: true}, false},
	} {
		if got := tt.t.fromSwitch(); got != tt.want {
			t.Errorf("%+v: %v", tt.t, got)
		}
	}
}
