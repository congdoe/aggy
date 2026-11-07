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
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
)

const reviewID = "22222222-2222-4333-8444-555555555555"

// TestCannotTellKeepsRow (T6899): a sessions folder that is not there says
// nothing, so nothing goes back and the row is not removed.
func TestCannotTellKeepsRow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	now := time.Now()
	c := store.Child{Vendor: "claude", VendorHome: filepath.Join(home, "unmounted"), Cwd: t.TempDir()}
	if got := childStarted(supervisor.RunFacts{}, c, reviewID, true, now, nil, &sessionFiles{}); got != startUnknown {
		t.Errorf("an unmounted home = %v", got)
	}
	// A vendor aggy has no folder for at all.
	if got := childStarted(supervisor.RunFacts{}, store.Child{Vendor: "nobody"}, reviewID, true, now, nil, &sessionFiles{}); got != startUnknown {
		t.Errorf("an unknown vendor = %v", got)
	}
	// Codex with no id, and no folder to watch.
	if got := childStarted(supervisor.RunFacts{}, store.Child{Vendor: "codex"}, "", true, now, nil, &sessionFiles{}); got != startUnknown {
		t.Errorf("no watch = %v", got)
	}
	x := newSessionIndex("p", adapter.Codex)
	if got := childStarted(supervisor.RunFacts{}, store.Child{Vendor: "codex"}, "", true, now, x.sawRollout, &sessionFiles{}); got != startUnknown {
		t.Errorf("an index with no watch = %v", got)
	}
	x.watchDir = filepath.Join(home, "unmounted", "sessions")
	if got := childStarted(supervisor.RunFacts{}, store.Child{Vendor: "codex"}, "", true, now, x.sawRollout, &sessionFiles{}); got != startUnknown {
		t.Errorf("a watch of a folder not there = %v", got)
	}
}

// TestSkewedFileIsStarted (T6899): a new child's id is new, so its file counts
// however aggy's clock reads. A resumed child's old file does not.
func TestSkewedFileIsStarted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GROK_HOME", "")
	cwd := t.TempDir()
	d := filepath.Join(home, ".grok", "sessions", grok.EncodeCwd(cwd), reviewID)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-5 * time.Second)
	if err := os.Chtimes(d, past, past); err != nil {
		t.Fatal(err)
	}
	c := store.Child{Vendor: "grok", Cwd: cwd}
	if got := childStarted(supervisor.RunFacts{}, c, reviewID, true, time.Now(), nil, &sessionFiles{}); got != startYes {
		t.Errorf("new child = %v", got)
	}
	if got := childStarted(supervisor.RunFacts{}, c, reviewID, false, time.Now(), nil, &sessionFiles{}); got != startNever {
		t.Errorf("resumed child = %v", got)
	}
}

// TestSettleOtherRepo (T6901): a session of another directory never becomes this
// child's, whichever vendor; one of this directory does.
func TestSettleOtherRepo(t *testing.T) {
	const from, to = "11111111-2222-4333-8444-555555555555", "99999999-2222-4333-8444-555555555555"
	const cfrom, cto = "019a0000-0000-7000-8000-000000000001", "019a0000-0000-7000-8000-000000000002"
	ctx := context.Background()
	rollout := func(t *testing.T, home, cwd string) {
		t.Helper()
		u := time.Now().UTC()
		day := filepath.Join(home, ".codex", "sessions", u.Format("2006"), u.Format("01"), u.Format("02"))
		if err := os.MkdirAll(day, 0o700); err != nil {
			t.Fatal(err)
		}
		meta := `{"type":"session_meta","payload":{"id":"` + cto + `","source":"cli","cwd":"` + cwd + `"}}`
		if err := os.WriteFile(filepath.Join(day, "rollout-2026-01-01T00-00-00-"+cto+".jsonl"), []byte(meta+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	touch := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name, vendor, from, to string
		place                  func(t *testing.T, home, dir string)
		moves                  bool
	}{
		{"claude, another project's folder", "claude", from, to, func(t *testing.T, home, _ string) {
			touch(t, filepath.Join(home, ".claude", "projects", "-some-other-repo", to+".jsonl"))
		}, false},
		{"claude, this project's folder", "claude", from, to, func(t *testing.T, home, dir string) {
			touch(t, filepath.Join(home, ".claude", "projects", claudeProjectDir(dir), to+".jsonl"))
		}, true},
		{"grok, another folder", "grok", from, to, func(t *testing.T, home, _ string) {
			if err := os.MkdirAll(filepath.Join(home, ".grok", "sessions", "other", to), 0o700); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"grok, this folder", "grok", from, to, func(t *testing.T, home, dir string) {
			if err := os.MkdirAll(filepath.Join(home, ".grok", "sessions", grok.EncodeCwd(dir), to), 0o700); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"codex, another cwd", "codex", cfrom, cto, func(t *testing.T, home, _ string) { rollout(t, home, "/some/other/repo") }, false},
		{"codex, no cwd", "codex", cfrom, cto, func(t *testing.T, home, _ string) { rollout(t, home, "") }, false},
		{"codex, this cwd", "codex", cfrom, cto, func(t *testing.T, home, dir string) { rollout(t, home, dir) }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := sessionStore(t)
			home := os.Getenv("HOME")
			t.Setenv("GROK_HOME", "")
			t.Setenv("CODEX_HOME", "")
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			dir := mustCanon(t, t.TempDir())
			now := time.Now()
			if err := s.InsertParent(ctx, store.Parent{ID: "aa11", Dir: dir, CreatedAt: now, LastActive: now}); err != nil {
				t.Fatal(err)
			}
			c := store.Child{ID: "c1", ParentID: "aa11", Vendor: tt.vendor, VendorSessionID: tt.from, Cwd: dir, Status: "open", CreatedAt: now, LastActive: now}
			if err := s.InsertChild(ctx, c); err != nil {
				t.Fatal(err)
			}
			tt.place(t, home, dir)
			ids := &childIDs{s: s, child: c}
			ids.follow(tt.to, "clear")
			ids.settle(ctx, nil, &sessionFiles{})
			cs, err := s.Children(ctx, "aa11")
			if err != nil {
				t.Fatal(err)
			}
			want := tt.from
			if tt.moves {
				want = tt.to
			}
			if cs[0].VendorSessionID != want {
				t.Errorf("row = %q, want %q", cs[0].VendorSessionID, want)
			}
		})
	}
}

// TestNoIDDoesNotHide (T6899): a Codex child with no id cannot be reopened, so it
// hides neither the session before it nor its parent's earlier one.
func TestNoIDDoesNotHide(t *testing.T) {
	ctx, s, dir, claudeID := unstartedWorld(t)
	later := time.Now().Add(time.Hour)
	if err := s.InsertParent(ctx, store.Parent{ID: "bb22", Dir: dir, DirKey: mustCanon(t, dir), CreatedAt: later, LastActive: later}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChild(ctx, store.Child{ID: "c-later", ParentID: "bb22", Vendor: "codex", Cwd: dir, Status: "open", CreatedAt: later, LastActive: later}); err != nil {
		t.Fatal(err)
	}
	got, err := plan(ctx, s, config.Config{}, dir, target{}, time.Now(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got.parent.ID != "aa11" || got.resume != claudeID {
		t.Errorf("reopened parent %s new=%v vendor=%s", got.parent.ID, got.newChild, got.child.Vendor)
	}
	// In the one parent too.
	if err := s.InsertChild(ctx, store.Child{ID: "c-cdx", ParentID: "aa11", Vendor: "codex", Cwd: dir, Status: "open", CreatedAt: later, LastActive: later}); err != nil {
		t.Fatal(err)
	}
	got, err = plan(ctx, s, config.Config{}, dir, target{ParentID: "aa11"}, time.Now(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got.resume != claudeID {
		t.Errorf("reopened %s new=%v", got.child.Vendor, got.newChild)
	}
	// Named by vendor, with nothing else of that vendor, it still gets the note.
	var note strings.Builder
	got, err = plan(ctx, s, config.Config{}, dir, target{ParentID: "aa11", Vendor: adapter.Codex}, time.Now(), &note)
	if err != nil {
		t.Fatal(err)
	}
	if !got.newChild || !strings.Contains(note.String(), "no recorded session id") {
		t.Errorf("new=%v, note %q", got.newChild, note.String())
	}
}

// TestGoBackLine (T6899): going back to a child with no id opens a new session,
// and the line says so.
func TestGoBackLine(t *testing.T) {
	line := goBackLine(adapter.Grok, switchTo{vendor: adapter.Codex})
	if strings.Contains(line, "goes back") || !strings.Contains(line, "opens a new codex session") {
		t.Errorf("no id: %q", line)
	}
	line = goBackLine(adapter.Grok, switchTo{vendor: adapter.Codex, fromID: reviewID})
	if !strings.Contains(line, "goes back to codex") {
		t.Errorf("with an id: %q", line)
	}
}

// TestPreviewWritesNothing (T6899): a preview plan writes nothing, for any parent.
func TestPreviewWritesNothing(t *testing.T) {
	ctx, s, dir, _ := unstartedWorld(t)
	before, err := s.Children(ctx, "aa11")
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := s.InsertParent(ctx, store.Parent{ID: "bb22", Dir: dir, DirKey: mustCanon(t, dir), CreatedAt: later, LastActive: later}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChild(ctx, store.Child{ID: "c-later", ParentID: "bb22", Vendor: "grok", VendorSessionID: "99999999-2222-4333-8444-555555555555", Cwd: dir, Status: "open", CreatedAt: later, LastActive: later}); err != nil {
		t.Fatal(err)
	}
	if _, err := plan(ctx, s, config.Config{}, dir, target{Preview: true}, time.Now(), io.Discard); err != nil {
		t.Fatal(err)
	}
	after, err := s.Children(ctx, "aa11")
	if err != nil {
		t.Fatal(err)
	}
	for i := range before {
		if before[i].FileSeen != after[i].FileSeen || !before[i].LastActive.Equal(after[i].LastActive) {
			t.Errorf("child %s changed: %+v -> %+v", before[i].ID, before[i], after[i])
		}
	}
}

// TestHookStates (T6848): hooks of another aggy binary, or part of the set, are
// not "not installed", and only no Stop hook means aggy sees no turn end.
func TestHookStates(t *testing.T) {
	const bin, other = "/opt/homebrew/bin/aggy", "/Users/x/dev/aggy/bin/aggy"
	settings := func(t *testing.T, b string, events ...string) string {
		t.Helper()
		s := `{"hooks":{`
		for i, e := range events {
			if i > 0 {
				s += ","
			}
			cmd, err := hook.Command(b, hook.Entry{Vendor: adapter.Claude, Event: e})
			if err != nil {
				t.Fatal(err)
			}
			s += `"` + e + `":[{"hooks":[{"type":"command","command":"` + cmd + `"}]}]`
		}
		return s + `}}`
	}
	all := []string{"Stop", "StopFailure", "SessionStart", "Notification"}
	for _, tt := range []struct {
		name, content, state string
		missing              bool
		warn                 string
	}{
		{"none", "", hookMissing, true, "are not installed"},
		{"all, this binary", settings(t, bin, all...), hookInstalled, false, ""},
		{"all, another binary", settings(t, other, all...), hookStale, false, "another aggy binary"},
		{"part of the set", settings(t, bin, "Stop", "Notification"), hookPartial, false, "StopFailure, SessionStart"},
		{"no Stop", settings(t, bin, "Notification"), hookPartial, true, "are not installed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			oldBin, oldPath, oldCheck := hookBin, pathAggy, checkTrusted
			hookBin = func() (string, error) { return bin, nil }
			pathAggy = func() (string, error) { return bin, nil }
			checkTrusted = func(string) error { return nil }
			t.Cleanup(func() { hookBin, pathAggy, checkTrusted = oldBin, oldPath, oldCheck })
			if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
				t.Fatal(err)
			}
			if tt.content != "" {
				if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(tt.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if st, _ := hookStatus(adapter.Claude, bin); st != tt.state {
				t.Errorf("state = %q, want %q", st, tt.state)
			}
			if got := hooksMissing(adapter.Claude); got != tt.missing {
				t.Errorf("hooksMissing = %v", got)
			}
			w := hookWarning(adapter.Claude)
			if tt.warn == "" && w != "" || !strings.Contains(w, tt.warn) {
				t.Errorf("warning = %q", w)
			}
		})
	}
}
