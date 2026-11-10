package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/transcript"
)

// The PR #127 review's attacks on T6965, as regression tests. Each failed on
// 8a47321.

// testIndex opens the default index for a test.
func testIndex(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestR127ClaudeCollision: /w/my_app and /w/my-app share one Claude project
// folder, as do every folder under CLAUDE_CODE_PROJECT_DIR_NAME; the other
// folder's session is not listed, resolved, or continued here, as its file
// records the other folder.
func TestR127ClaudeCollision(t *testing.T) {
	for _, tt := range []struct{ name, here, other string }{
		{"underscore and dash", "my_app", "my-app"},
		{"dot and dash", "foo.bar", "foo-bar"},
		{"nested and dash", filepath.Join("a", "b-c"), filepath.Join("a", "b", "c")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home, base := nativeWorld(t)
			here, other := filepath.Join(base, tt.here), filepath.Join(base, tt.other)
			for _, d := range []string{here, other} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if transcript.ClaudeProjectDir(here) != transcript.ClaudeProjectDir(other) {
				t.Fatal("the test's folders do not collide")
			}
			claudeNativeAt(t, home, other, natB, "OTHER PROJECT secret plan", time.Now())
			t.Chdir(here)
			assertNotHere(t, here)
		})
	}
	t.Run("one project folder named for all", func(t *testing.T) {
		home, base := nativeWorld(t)
		cfgDir := filepath.Join(home, "cfg")
		t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
		t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "shared")
		here, other := filepath.Join(base, "one"), filepath.Join(base, "two")
		for _, d := range []string{here, other} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		writeNativeAt(t, filepath.Join(cfgDir, "projects", "shared", natB+".jsonl"), claudeLineIn(other, "OTHER"), time.Now())
		writeNativeAt(t, filepath.Join(cfgDir, "projects", "shared", natA+".jsonl"), claudeLineIn(here, "mine"), time.Now().Add(-time.Hour))
		t.Chdir(here)
		ns, _ := nativeSessions(&sessionFiles{}, adapter.Claude, here)
		if len(ns) != 1 || ns[0].ID != natA {
			t.Errorf("listed %+v, want only this folder's", ns)
		}
	})
	t.Run("no folder recorded", func(t *testing.T) {
		home, cwd := nativeWorld(t)
		p := claudeNativeAt(t, home, cwd, natB, "x", time.Now())
		writeNativeAt(t, p, claudeUserLine("no cwd"), time.Now())
		t.Chdir(cwd)
		assertNotHere(t, cwd)
	})
}

// assertNotHere checks that natB is not listed in, resolved to, or continued
// in here.
func assertNotHere(t *testing.T, here string) {
	t.Helper()
	if ns, _ := nativeSessions(&sessionFiles{}, adapter.Claude, here); len(ns) != 0 {
		t.Errorf("listed here: %+v", ns)
	}
	tg := target{}
	if code, err := resolveResume(&tg, adapter.Claude, natB); code != diag.CodeNotFound || tg.Native != nil {
		t.Errorf("resolveResume = %v %v native %v, want not found", code, err, tg.Native)
	}
	var errb strings.Builder
	p, err := plan(context.Background(), testIndex(t), config.Default(), here, target{Continue: true}, time.Now(), &errb)
	if !errors.Is(err, errNoSession) || p.adopted {
		t.Errorf("-c: %v adopted %v resume %q, want no session", err, p.adopted, p.resume)
	}
}

// TestR127FutureMtime: a file modified in the future never wins -c and is
// listed last, and a launch by name is not taken by it.
func TestR127FutureMtime(t *testing.T) {
	home, cwd := nativeWorld(t)
	s := testIndex(t)
	var errb strings.Builder
	claudeNativeAt(t, home, cwd, natA, "real recent work", time.Now().Add(-time.Minute))
	claudeNativeAt(t, home, cwd, natB, "planted", time.Now().Add(365*24*time.Hour))
	p, err := plan(context.Background(), s, config.Default(), cwd, target{Continue: true, Vendor: adapter.Claude}, time.Now(), &errb)
	if err != nil || p.resume != natA {
		t.Errorf("-c: %v resume %q, want %s", err, p.resume, natA)
	}
	ns, _ := nativeSessions(&sessionFiles{}, adapter.Claude, cwd)
	if len(ns) != 2 || ns[0].ID != natA || ns[1].ID != natB {
		t.Errorf("listed %+v, want the future one last", ns)
	}
	cs, _, err := sessionChoices(context.Background(), s, cwd, []adapter.Vendor{adapter.Claude}, &sessionFiles{})
	if err != nil || len(cs) != 2 || cs[0].Native.ID != natA {
		t.Errorf("choices %+v, %v; want the future one last", cs, err)
	}
	// Only a planted file: -c finds nothing to continue.
	_ = os.Remove(filepath.Join(home, ".claude", "projects", transcript.ClaudeProjectDir(cwd), natA+".jsonl"))
	if _, err := plan(context.Background(), s, config.Default(), cwd, target{Continue: true, Vendor: adapter.Claude}, time.Now(), &errb); !errors.Is(err, errNoSession) {
		t.Errorf("-c with only a future file: %v, want no session", err)
	}
}

// TestR127AdoptNamedWrongSession: a launch by name whose CLI reported no
// session is not given another session that changed in the folder meanwhile,
// and leaves no rows behind.
func TestR127AdoptNamedWrongSession(t *testing.T) {
	home, cwd := nativeWorld(t)
	ctx := context.Background()
	s := testIndex(t)
	var errb strings.Builder
	p, err := plan(ctx, s, config.Default(), cwd, target{ResumeName: "typo-name", Vendor: adapter.Claude}, time.Now(), &errb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.commit(ctx, s, time.Now()); err != nil {
		t.Fatal(err)
	}
	claudeNativeAt(t, home, cwd, natD, "someone else's session", time.Now())
	adoptNamed(ctx, s, p.child, "", &sessionFiles{}, io.Discard)
	if c, err := s.ChildBySession(ctx, "claude", natD); err == nil {
		t.Errorf("another session was adopted: %+v", c)
	}
	if ps, _ := s.Parents(ctx); len(ps) != 0 {
		t.Errorf("parents left: %+v", ps)
	}
}

// TestR127ResumeValues: what -r takes as a name, an id, or neither.
func TestR127ResumeValues(t *testing.T) {
	home, cwd := nativeWorld(t)
	t.Chdir(cwd)
	claudeNativeAt(t, home, cwd, natA, "here", time.Now())
	tests := []struct {
		v      adapter.Vendor
		value  string
		code   diag.Code
		name   string
		native string
	}{
		{adapter.Claude, "foo.jsonl", diag.CodeUsage, "", ""},
		{adapter.Claude, " -x", diag.CodeUsage, "", ""},
		{adapter.Claude, "x ", diag.CodeUsage, "", ""},
		{adapter.Claude, "help", diag.CodeUsage, "", ""},
		{adapter.Claude, "mcp", diag.CodeUsage, "", ""},
		{adapter.Claude, "MCP", diag.CodeUsage, "", ""},
		{adapter.Grok, "help", diag.CodeUsage, "", ""},
		{adapter.Claude, "3f2a", diag.CodeNotFound, "", ""},
		{adapter.Claude, "deadbeef", diag.CodeNotFound, "", ""},
		{"", "3f2a", diag.CodeNotFound, "", ""},
		{adapter.Claude, strings.ToUpper(natB), diag.CodeNotFound, "", ""},
		{adapter.Claude, strings.ToUpper(natA), diag.Code{}, "", natA},
		{adapter.Claude, "x y", diag.CodeUsage, "", ""},
		{adapter.Claude, "x y", diag.CodeUsage, "", ""},
		{adapter.Claude, "a\tb", diag.CodeUsage, "", ""},
		{adapter.Claude, `a b "c" ;$(id)`, diag.Code{}, `a b "c" ;$(id)`, ""},
		{adapter.Claude, "setup", diag.Code{}, "setup", ""},
		{adapter.Codex, "auth", diag.CodeNotFound, "", ""},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s %q", tt.v, tt.value), func(t *testing.T) {
			tg := target{}
			code, err := resolveResume(&tg, tt.v, tt.value)
			if code != tt.code || (err != nil) != (tt.code != diag.Code{}) {
				t.Fatalf("code %v err %v, want %v", code, err, tt.code)
			}
			got := ""
			if tg.Native != nil {
				got = tg.Native.ID
			}
			if tg.ResumeName != tt.name || got != tt.native {
				t.Errorf("name %q native %q, want %q %q", tg.ResumeName, got, tt.name, tt.native)
			}
			if tt.v == adapter.Codex && strings.Contains(err.Error(), "name only") {
				t.Errorf("codex error %q", err)
			}
		})
	}
}

// TestR127DoubleAdopt: two aggy processes adopt one native session at once;
// the second records nothing and says another aggy took it, and a plan made
// again reopens the first one's child.
func TestR127DoubleAdopt(t *testing.T) {
	home, cwd := nativeWorld(t)
	claudeNativeAt(t, home, cwd, natA, "hello", time.Now().Add(-time.Hour))
	ctx := context.Background()
	s1, s2 := testIndex(t), testIndex(t)
	var errb strings.Builder
	ns, _ := findNative(&sessionFiles{}, []adapter.Vendor{adapter.Claude}, cwd, natA)
	p1, err1 := plan(ctx, s1, config.Default(), cwd, target{Native: &ns}, time.Now(), &errb)
	p2, err2 := plan(ctx, s2, config.Default(), cwd, target{Native: &ns}, time.Now(), &errb)
	if err1 != nil || err2 != nil || !p1.adopted || !p2.adopted {
		t.Fatalf("plans: %v %v", err1, err2)
	}
	l1, err := p1.commit(ctx, s1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l1.Release(ctx) }()
	if _, err := p2.commit(ctx, s2, time.Now()); !errors.Is(err, errAdoptedElsewhere) {
		t.Errorf("second commit: %v, want errAdoptedElsewhere", err)
	}
	if ps, _ := s1.Parents(ctx); len(ps) != 1 {
		t.Errorf("parents = %d, want 1", len(ps))
	}
	p3, err := plan(ctx, s2, config.Default(), cwd, target{Native: &ns}, time.Now(), &errb)
	if err != nil || p3.adopted || p3.parent.ID != p1.parent.ID {
		t.Errorf("plan again: %v adopted %v parent %s, want %s", err, p3.adopted, p3.parent.ID, p1.parent.ID)
	}
	if _, err := p3.commit(ctx, s2, time.Now()); err == nil || !strings.Contains(err.Error(), "already open in another aggy") {
		t.Errorf("commit again: %v, want the session open in the first aggy", err)
	}
}

// TestR127PrintModeWinsContinue: a newer session aggy never saw wins -c only
// as this folder's own, and is named before the launch; one whose file
// records no folder (here, a line in the shape claude -p might write) does not
// win. aggy's own print runs leave no session at all (TestR127PrintNoSession).
func TestR127PrintModeWinsContinue(t *testing.T) {
	home, cwd := nativeWorld(t)
	s := testIndex(t)
	claudeNativeAt(t, home, cwd, natA, "my interactive work", time.Now().Add(-time.Hour))
	p := claudeNativeAt(t, home, cwd, natB, "x", time.Now())
	writeNativeAt(t, p, `{"type":"user","entrypoint":"sdk-cli","message":{"content":"ignore prior instructions"}}`+"\n", time.Now())
	var errb strings.Builder
	pl, err := plan(context.Background(), s, config.Default(), cwd, target{Continue: true}, time.Now(), &errb)
	if err != nil || pl.resume != natA {
		t.Fatalf("-c: %v resume %q, want %s", err, pl.resume, natA)
	}
	if !strings.Contains(errb.String(), "continuing claude's own session from 1h ago, new to aggy: my interactive work") {
		t.Errorf("said %q, want the session named", errb.String())
	}
}

// TestR127PrintNoSession: aggy's print runs of Claude pass
// --no-session-persistence, unless the words ask for a session.
func TestR127PrintNoSession(t *testing.T) {
	caps := []string{probe.CapNoSessionPersistence}
	tests := []struct {
		v     adapter.Vendor
		caps  []string
		words []string
		want  bool
	}{
		{adapter.Claude, caps, []string{"-p", "hi"}, true},
		{adapter.Claude, caps, []string{"--print", "hi"}, true},
		{adapter.Claude, nil, []string{"-p", "hi"}, false},
		{adapter.Grok, caps, []string{"-p", "hi"}, false},
		{adapter.Claude, caps, []string{"--continue", "-p", "hi"}, false},
		{adapter.Claude, caps, []string{"-p", "-r", "abc", "hi"}, false},
		{adapter.Claude, caps, []string{"-p", "--resume=abc", "hi"}, false},
		{adapter.Claude, caps, []string{"-p", "--session-id", natA, "hi"}, false},
		{adapter.Claude, caps, []string{"-p", "-n", "demo", "hi"}, false},
		{adapter.Claude, caps, []string{"mcp", "list"}, false},
		{adapter.Claude, caps, []string{"-p", "--", "-c"}, true},
	}
	for _, tt := range tests {
		if got := printLeavesNoSession(tt.v, tt.caps, tt.words); got != tt.want {
			t.Errorf("%s %q: %v, want %v", tt.v, tt.words, got, tt.want)
		}
	}
}

// TestR127PickerPiped: input that is not a terminal never picks, and a picked
// line leaves what was typed after it.
func TestR127PickerPiped(t *testing.T) {
	home, cwd := nativeWorld(t)
	t.Chdir(cwd)
	claudeNativeAt(t, home, cwd, natA, "older", time.Now().Add(-2*time.Hour))
	in := strings.NewReader("1\nrest of type-ahead\n")
	pickIn = in
	defer func() { pickIn = os.Stdin }()
	defer func(f func() bool) { stdinTerminal = f }(stdinTerminal)
	stdinTerminal = func() bool { return false }
	var out strings.Builder
	var tg target
	picked, code, err := pickSession(&tg, "", &out)
	if picked || code != diag.CodeNoTerminal || err == nil {
		t.Errorf("piped pick: %v %v %v, want refused", picked, code, err)
	}
	stdinTerminal = func() bool { return true }
	if picked, _, err := pickSession(&tg, "", &out); !picked || err != nil {
		t.Fatalf("pick: %v %v", picked, err)
	}
	if rest, _ := io.ReadAll(in); string(rest) != "rest of type-ahead\n" {
		t.Errorf("left %q, want the type-ahead", rest)
	}
}

// TestR127ListingCost: -c reads the head of as few files as it needs, and a
// list at most what it shows (TestNativeBudgetTimeout has the time limit).
func TestR127ListingCost(t *testing.T) {
	home, cwd := nativeWorld(t)
	big := strings.Repeat(`{"type":"assistant","message":{"content":"`+strings.Repeat("y", 4000)+`"}}`+"\n", 50)
	now := time.Now()
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i)
		writeNativeAt(t, filepath.Join(home, ".claude", "projects", transcript.ClaudeProjectDir(cwd), id+".jsonl"),
			claudeLineIn(cwd, "p")+big, now.Add(-time.Duration(i)*time.Minute))
	}
	instr := strings.Repeat("i", 30000)
	for i := 0; i < 600; i++ {
		id := fmt.Sprintf("%08x-2222-4222-8222-222222222222", i)
		at := now.Add(-time.Duration(i) * time.Minute)
		meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "source": "cli", "cwd": cwd, "base_instructions": instr}})
		day := at.UTC().Format("2006/01/02")
		name := "rollout-" + at.UTC().Format("2006-01-02T15-04-05") + "-" + id + ".jsonl"
		writeNativeAt(t, filepath.Join(home, ".codex", "sessions", day, name), string(meta)+"\n", at)
	}
	s := testIndex(t)
	// Head reads are the point, not time: a budget no load runs past.
	defer func(d time.Duration) { nativeBudget = d }(nativeBudget)
	nativeBudget = time.Hour
	for _, v := range []adapter.Vendor{adapter.Claude, adapter.Codex} {
		before := nativeHeadReads.Load()
		ns, _ := nativeSessions(&sessionFiles{}, v, cwd)
		// One read a session for Claude; the session_meta and the title for Codex.
		if n := nativeHeadReads.Load() - before; len(ns) != maxNativeListed || n > 2*maxNativeListed {
			t.Errorf("%s: listed %d with %d head reads", v, len(ns), n)
		}
		before = nativeHeadReads.Load()
		var errb strings.Builder
		p, err := plan(context.Background(), s, config.Default(), cwd, target{Continue: true, Vendor: v}, time.Now(), &errb)
		if n := nativeHeadReads.Load() - before; err != nil || !p.adopted || n > 2 {
			t.Errorf("%s -c: %v adopted %v with %d head reads", v, err, p.adopted, n)
		}
	}
}

// TestNativeBudgetTimeout: a look at the vendors' folders that runs past
// nativeBudget gives up, says so, and -c goes on with aggy's own index; a
// blocking fake look stands in for a slow disk, and the budget is tiny.
func TestNativeBudgetTimeout(t *testing.T) {
	home, cwd := nativeWorld(t)
	claudeNativeAt(t, home, cwd, natA, "here", time.Now())
	s := testIndex(t)
	defer func(d time.Duration, h func()) { nativeBudget, nativeScanHook = d, h }(nativeBudget, nativeScanHook)
	nativeBudget = time.Millisecond
	release := make(chan struct{})
	defer close(release)
	nativeScanHook = func() { <-release }
	var errb strings.Builder
	_, err := plan(context.Background(), s, config.Default(), cwd, target{Continue: true, Vendor: adapter.Claude}, time.Now(), &errb)
	if !errors.Is(err, errNoSession) || !strings.Contains(errb.String(), nativeSlowNote) {
		t.Errorf("slow -c: %v, said %q", err, errb.String())
	}
	cs, slow, err := sessionChoices(context.Background(), s, cwd, []adapter.Vendor{adapter.Claude}, &sessionFiles{})
	if err != nil || !slow || len(cs) != 0 {
		t.Errorf("slow list: %+v slow %v, %v", cs, slow, err)
	}
}

// TestR127ContinueInstalled: bare -c passes over a newer session of a vendor
// whose CLI is not on PATH, aggy's or the vendor's own.
func TestR127ContinueInstalled(t *testing.T) {
	home, cwd := nativeWorld(t)
	s := testIndex(t)
	claudeNativeAt(t, home, cwd, natA, "claude's", time.Now().Add(-time.Hour))
	codexNativeAt(t, home, cwd, natB, "cli", "codex's", time.Now())
	defer func(f func(adapter.Vendor) bool) { cliInstalled = f }(cliInstalled)
	cliInstalled = func(v adapter.Vendor) bool { return v == adapter.Claude }
	var errb strings.Builder
	p, err := plan(context.Background(), s, config.Default(), cwd, target{Continue: true, Installed: true}, time.Now(), &errb)
	if err != nil || p.resume != natA {
		t.Errorf("-c: %v resume %q, want claude's %s", err, p.resume, natA)
	}
	p, err = plan(context.Background(), s, config.Default(), cwd, target{Continue: true}, time.Now(), &errb)
	if err != nil || p.resume != natB {
		t.Errorf("-c not limited: %v resume %q, want codex's %s", err, p.resume, natB)
	}
}

// TestCodexNativeNewestFirst: Codex's rollouts are walked newest first, across
// date folders, and the walk stops once it has what it was asked for.
func TestCodexNativeNewestFirst(t *testing.T) {
	home, cwd := nativeWorld(t)
	now := time.Now()
	ids := []string{natA, natB, natC, natD} // oldest first, a day apart
	for i, id := range ids {
		codexNativeAt(t, home, cwd, id, "cli", "p", now.Add(time.Duration(i-len(ids))*24*time.Hour))
	}
	var seen []string
	got := codexNative(filepath.Join(home, ".codex"), 2, func(n *nativeSession) bool {
		seen = append(seen, n.ID)
		return n.ID != natD
	})
	if len(got) != 2 || got[0].ID != natC || got[1].ID != natB || strings.Join(seen, ",") != natD+","+natC+","+natB {
		t.Errorf("got %+v after accepting %v; want C and B, newest first, A never looked at", got, seen)
	}
}
