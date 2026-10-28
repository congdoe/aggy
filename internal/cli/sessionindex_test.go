package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
)

const (
	idA = "aaaaaaaa-1111-4111-8111-111111111111"
	idB = "bbbbbbbb-2222-4222-8222-222222222222"
)

// recorded returns a sessionIndex whose events go to a slice.
func recorded(v adapter.Vendor) (*sessionIndex, func() []string) {
	x := newSessionIndex("p", v)
	var mu sync.Mutex
	var ids []string
	x.record = func(e store.Event) error {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, e.Detail["session"].(string)+"/"+e.Detail["via"].(string))
		return nil
	}
	return x, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), ids...) }
}

func hookReq(v adapter.Vendor, event, payload string) supervisor.Request {
	return supervisor.Request{Type: supervisor.ReqHookEvent, Vendor: string(v), Event: event, Payload: json.RawMessage(payload)}
}

// TestSessionIndexAssigned: Claude and Grok record their pre-assigned
// id and nothing a hook says, even a SessionStart from a nested CLI or a
// forged one.
func TestSessionIndexAssigned(t *testing.T) {
	for _, v := range []adapter.Vendor{adapter.Claude, adapter.Grok} {
		x, got := recorded(v)
		x.set(idA, "", "assigned")
		x.hook(hookReq(v, "SessionStart", `{"session_id":"`+idB+`","sessionId":"`+idB+`","source":"startup"}`))
		x.hook(hookReq(v, "SessionStart", `{"session_id":"`+idB+`","sessionId":"`+idB+`","source":"clear"}`))
		if g := got(); len(g) != 1 || g[0] != idA+"/assigned" {
			t.Errorf("%s: recorded %v", v, g)
		}
	}
}

// TestSessionIndexCodexClaim: a Codex id is recorded only when the first
// claimed id shows up as a new rollout; a rollout alone, or a later
// claim, never is.
func TestSessionIndexCodexClaim(t *testing.T) {
	// A lone new rollout from another Codex: not recorded.
	x, got := recorded(adapter.Codex)
	x.saw([]string{idB})
	if g := got(); len(g) != 0 {
		t.Errorf("rollout alone recorded %v", g)
	}
	// The on-screen claim, then its rollout: recorded.
	x.hook(hookReq(adapter.Codex, "SessionStart", `{"session_id":"`+idA+`","source":"startup"}`))
	if g := got(); len(g) != 0 {
		t.Errorf("claim alone recorded %v", g)
	}
	x.saw([]string{idA})
	// A nested codex exec's later SessionStart and rollout: ignored.
	x.hook(hookReq(adapter.Codex, "SessionStart", `{"session_id":"`+idB+`","source":"startup"}`))
	if g := got(); len(g) != 1 || g[0] != idA+"/hook and sessions folder" {
		t.Errorf("recorded %v", g)
	}
	// Rollout first, then the claim: recorded on the claim.
	y, gotY := recorded(adapter.Codex)
	y.saw([]string{idA})
	y.hook(hookReq(adapter.Codex, "Stop", `{"session_id":"`+idA+`"}`))
	if g := gotY(); len(g) != 1 {
		t.Errorf("recorded %v", g)
	}
	// A later claim never replaces the first, even once its rollout shows.
	z, gotZ := recorded(adapter.Codex)
	z.hook(hookReq(adapter.Codex, "SessionStart", `{"session_id":"`+idA+`"}`))
	z.hook(hookReq(adapter.Codex, "SessionStart", `{"session_id":"`+idB+`"}`))
	z.saw([]string{idB})
	if g := gotZ(); len(g) != 0 {
		t.Errorf("later claim recorded %v", g)
	}
	// Another vendor's event and a bad id are ignored.
	w, gotW := recorded(adapter.Codex)
	w.hook(hookReq(adapter.Claude, "SessionStart", `{"session_id":"`+idA+`"}`))
	w.hook(hookReq(adapter.Codex, "SessionStart", `{"session_id":"x"}`))
	w.saw([]string{idA, "x"})
	if g := gotW(); len(g) != 0 {
		t.Errorf("recorded %v", g)
	}
}

func rollout(t *testing.T, dir string, at time.Time, id string) {
	t.Helper()
	d := codex.RolloutDirs(dir, at, at)[1] // the local-time folder of at
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "rollout-2026-10-28T10-00-00-"+id+".jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWatchCodex(t *testing.T) {
	old := codexPoll
	codexPoll = 10 * time.Millisecond
	t.Cleanup(func() { codexPoll = old })
	dir := t.TempDir()
	start := time.Now()
	rollout(t, dir, start, "cccccccc-3333-4333-8333-333333333333") // an older session
	before := codexRollouts(dir, start, start)
	x, got := recorded(adapter.Codex)
	done := make(chan struct{})
	go func() { x.watchCodex(dir, start, before, make(chan struct{})); close(done) }()
	rollout(t, dir, start, idB) // another Codex, alongside
	time.Sleep(50 * time.Millisecond)
	if g := got(); len(g) != 0 {
		t.Fatalf("the watch recorded %v on its own", g)
	}
	x.hook(hookReq(adapter.Codex, "SessionStart", `{"session_id":"`+idA+`"}`))
	rollout(t, dir, start, idA)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the watch did not end once the id was recorded")
	}
	if g := got(); len(g) != 1 || g[0] != idA+"/hook and sessions folder" {
		t.Errorf("recorded %v", g)
	}
}

func TestSessionIndexWritesTheIndex(t *testing.T) {
	isolate(t)
	_, stop := startIndex("11111111-2222-4333-8444-555555555555", adapter.Claude, idA, nil)
	stop()
	code, out, _ := run("log")
	if code != 0 || !strings.Contains(out, "session="+idA) || !strings.Contains(out, "via=assigned") {
		t.Errorf("aggy log: %d\n%s", code, out)
	}
}
