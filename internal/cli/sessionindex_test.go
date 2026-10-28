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

func TestSessionIndexHooks(t *testing.T) {
	x, got := recorded(adapter.Claude)
	x.hook(hookReq(adapter.Claude, "Stop", `{"session_id":"not-a-uuid"}`))
	x.hook(hookReq(adapter.Grok, "SessionStart", `{"sessionId":"`+idB+`"}`)) // another vendor
	x.hook(hookReq(adapter.Claude, "Stop", `{"session_id":"`+idA+`"}`))      // first id, from any event
	x.hook(hookReq(adapter.Claude, "Stop", `{"session_id":"`+idB+`"}`))      // not a SessionStart: kept
	x.hook(hookReq(adapter.Claude, "SessionStart", `{"session_id":"`+idA+`","source":"resume"}`))
	x.hook(hookReq(adapter.Claude, "SessionStart", `{"session_id":"`+idB+`","source":"clear"}`))
	want := []string{idA + "/hook", idB + "/hook"}
	if g := got(); len(g) != 2 || g[0] != want[0] || g[1] != want[1] {
		t.Errorf("recorded %v", g)
	}
	// Grok's camelCase, and Codex's notify thread-id.
	g, gotG := recorded(adapter.Grok)
	g.hook(hookReq(adapter.Grok, "SessionStart", `{"sessionId":"`+idA+`"}`))
	c, gotC := recorded(adapter.Codex)
	c.hook(hookReq(adapter.Codex, "agent-turn-complete", `{"type":"agent-turn-complete","thread-id":"`+idB+`"}`))
	if len(gotG()) != 1 || len(gotC()) != 1 || gotC()[0] != idB+"/hook" {
		t.Errorf("grok %v codex %v", gotG(), gotC())
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
	run := func(t *testing.T, setup func(dir string, start time.Time)) []string {
		dir := t.TempDir()
		start := time.Now()
		rollout(t, dir, start, "cccccccc-3333-4333-8333-333333333333") // an older session
		before := codexRollouts(dir, start, start)
		x, got := recorded(adapter.Codex)
		stop := make(chan struct{})
		done := make(chan struct{})
		setup(dir, start) // after the snapshot, before the first poll
		go func() { x.watchCodex(dir, start, before, stop); close(done) }()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
			close(stop)
			<-done
		}
		return got()
	}
	if g := run(t, func(dir string, start time.Time) { rollout(t, dir, start, idA) }); len(g) != 1 || g[0] != idA+"/sessions folder" {
		t.Errorf("one new rollout: %v", g)
	}
	if g := run(t, func(string, time.Time) {}); len(g) != 0 {
		t.Errorf("no new rollout: %v", g)
	}
	if g := run(t, func(dir string, start time.Time) {
		// Both appear within one poll: which is ours cannot be told.
		rollout(t, dir, start, idA)
		rollout(t, dir, start, idB)
	}); len(g) != 0 {
		t.Errorf("two new rollouts: %v", g)
	}
	// A hook that already gave the id stops the watch.
	dir := t.TempDir()
	x, got := recorded(adapter.Codex)
	x.set(idB, "startup", "hook", true)
	done := make(chan struct{})
	go func() { x.watchCodex(dir, time.Now(), nil, make(chan struct{})); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the watch kept going after a hook gave the id")
	}
	if g := got(); len(g) != 1 {
		t.Errorf("recorded %v", g)
	}
}

func TestSessionIndexWritesTheIndex(t *testing.T) {
	isolate(t)
	x := newSessionIndex("11111111-2222-4333-8444-555555555555", adapter.Claude)
	x.hook(hookReq(adapter.Claude, "SessionStart", `{"session_id":"`+idA+`","source":"startup"}`))
	code, out, _ := run("log")
	if code != 0 || !strings.Contains(out, "session="+idA) || !strings.Contains(out, "source=startup") {
		t.Errorf("aggy log: %d\n%s", code, out)
	}
}
