//go:build unix

package supervisor

import (
	"bytes"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// toolsLine is the notice aggy tool on|off sends.
const toolsLine = `{"type":"tools-changed"}`

// statusErased is what closing a status line writes.
var statusErased = []byte("\x1b[2K\x1b8")

// fakeTools is a session's tool settings, changed by the test.
type fakeTools struct {
	mu  sync.Mutex
	set ToolSet
}

func (f *fakeTools) put(set ToolSet) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set = set
}

func (f *fakeTools) get() (ToolSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.set), nil
}

// toolsRun is switchRunOn with tools: none at launch.
func toolsRun(t *testing.T, startsTurn bool, v adapter.Vendor) (*termSide, *fakeChild, <-chan Exit, string, *fakeTools) {
	t.Helper()
	ft := &fakeTools{}
	ts, c, done, sock := switchRunOn(t, startsTurn, v, func(o *Options) {
		o.Tools, o.ToolsAtLaunch = ft.get, ToolSet{}
	})
	return ts, c, done, sock, ft
}

// clearScreen forgets what the terminal showed so far.
func (s *termSide) clearScreen() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shown.Reset()
}

// TestToolsChangedOffersResume (T6729): a tools-changed notice offers a
// resume of the vendor on screen at the turn end, never mid-turn; only
// the switch key types the exit command, and the exit is a resume.
func TestToolsChangedOffersResume(t *testing.T) {
	ts, c, done, sock, ft := toolsRun(t, true, adapter.Claude) // a turn is running
	ft.put(ToolSet{"context7": "mcp"})
	if r := request(t, sock, toolsLine); !strings.Contains(r, `"ok":true`) {
		t.Fatalf("notice reply %q", r)
	}
	never(t, "mid-turn", []byte("same session -> claude"), ts.screen, 300*time.Millisecond)
	_ = request(t, sock, stopLine)
	poll(t, "the offer", []byte(`aggy: tools +context7, same session -> claude. Ctrl+\ resumes, Esc stays.`), ts.screen)
	// Another notice, or a Stop, from the session cannot confirm it.
	_ = request(t, sock, toolsLine)
	_ = request(t, sock, stopLine)
	never(t, "no key", []byte("/exit"), c.received, 200*time.Millisecond)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Claude, Next: Choice{Vendor: adapter.Claude, Resume: true}})
}

// TestToolsChangedUnchanged: a notice whose settings leave the session's
// tools as launched offers nothing.
func TestToolsChangedUnchanged(t *testing.T) {
	ts, c, done, sock, _ := toolsRun(t, false, adapter.Claude)
	_ = request(t, sock, toolsLine)
	_ = request(t, sock, stopLine)
	never(t, "no change", []byte("same session"), ts.screen, 300*time.Millisecond)
	finish(t, c, done)
}

// TestToolsChangedUnsentText: the resume waits for text the user typed
// and did not send, as a switch does (S-1).
func TestToolsChangedUnsentText(t *testing.T) {
	ts, c, done, sock, ft := toolsRun(t, false, adapter.Claude)
	ts.typeIn(t, "draft")
	c.waitGot(t, []byte("draft"))
	ft.put(ToolSet{"context7": "mcp"})
	_ = request(t, sock, toolsLine)
	poll(t, "the unsent-text note", []byte("ready to resume claude with the changed tools, but you typed text"), ts.screen)
	if bytes.Contains(ts.screen(), []byte("same session -> claude")) {
		t.Error("offered past unsent text")
	}
	finish(t, c, done)
}

// TestToolsChangedEsc: Esc drops the resume; the same settings again
// offer nothing, another change offers again.
func TestToolsChangedEsc(t *testing.T) {
	ts, c, done, sock, ft := toolsRun(t, false, adapter.Claude)
	ft.put(ToolSet{"context7": "mcp"})
	_ = request(t, sock, toolsLine)
	poll(t, "the offer", []byte("tools +context7, same session -> claude"), ts.screen)
	ts.typeIn(t, "\x1b")
	poll(t, "the offer to close", statusErased, ts.screen)
	ts.clearScreen()
	_ = request(t, sock, toolsLine)
	_ = request(t, sock, stopLine)
	never(t, "dismissed", []byte("same session"), ts.screen, 300*time.Millisecond)
	ft.put(ToolSet{"context7": "mcp", "rtk": "hook"})
	_ = request(t, sock, toolsLine)
	poll(t, "the new offer", []byte("tools +context7 +rtk, same session -> claude"), ts.screen)
	finish(t, c, done)
}

// TestToolsChangedReverted: settings put back as launched drop a resume
// still waiting for the turn end.
func TestToolsChangedReverted(t *testing.T) {
	ts, c, done, sock, ft := toolsRun(t, true, adapter.Claude)
	ft.put(ToolSet{"context7": "mcp"})
	_ = request(t, sock, toolsLine)
	ft.put(ToolSet{})
	_ = request(t, sock, toolsLine)
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("aggy: switch vendor"), ts.screen)
	if bytes.Contains(ts.screen(), []byte("Resuming claude")) {
		t.Error("the reverted resume still waits")
	}
	finish(t, c, done)
}

// TestToolsChangedCodexNote (S-12): on a vendor with no hold signal the
// change is only a note; the switch key there queues the resume, which
// still waits for the user's key at the turn end.
func TestToolsChangedCodexNote(t *testing.T) {
	ts, c, done, sock, ft := toolsRun(t, false, adapter.Codex)
	ft.put(ToolSet{"context7": "mcp"})
	_ = request(t, sock, toolsLine)
	poll(t, "the note", []byte("Tools changed for codex: +context7 (+ on"), ts.screen)
	if bytes.Contains(ts.screen(), []byte("same session -> codex")) {
		t.Fatal("offered without the user")
	}
	confirm(t, ts) // the note's key, past its guard (L1)
	poll(t, "the offer", []byte("tools +context7, same session -> codex. Ctrl+"), ts.screen)
	never(t, "no key", []byte("/exit"), c.received, 150*time.Millisecond)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Codex, Next: Choice{Vendor: adapter.Codex, Resume: true}})
}

// TestToolsChangedRefused: a notice with fields is refused; one from
// outside the session is taken; a session that takes none (aggy
// --direct) refuses it.
func TestToolsChangedRefused(t *testing.T) {
	for _, line := range []string{`{"type":"tools-changed","vendor":"claude"}`, `{"type":"tools-changed","brief":true}`, `{"type":"tools-changed","tools":["a"]}`} {
		if _, err := decodeRequest([]byte(line)); err == nil {
			t.Errorf("%s was taken", line)
		}
	}
	if r := answer([]byte(toolsLine), false, nil, nil, nil, nil); r.OK || r.Code != "switch.not_supervised" {
		t.Errorf("no taker: %+v", r)
	}
	n := 0
	if r := answer([]byte(toolsLine), false, nil, nil, func() { n++ }, nil); !r.OK || n != 1 {
		t.Errorf("from outside the session: %+v, %d", r, n)
	}
}

func TestToolDiff(t *testing.T) {
	tests := []struct {
		was, now ToolSet
		want     []string
	}{
		{nil, nil, nil},
		{ToolSet{"a": "mcp"}, ToolSet{"a": "mcp"}, nil},
		{nil, ToolSet{"b": "mcp", "a": "skill"}, []string{"+a", "+b"}},
		{ToolSet{"a": "mcp"}, ToolSet{"a": "mcp+skill"}, []string{"~a"}},
		{ToolSet{"a": "mcp", "c": "hook"}, ToolSet{"b": "mcp"}, []string{"-a", "+b", "-c"}},
	}
	for _, tt := range tests {
		if got := toolDiff(tt.was, tt.now); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("toolDiff(%v, %v) = %q, want %q", tt.was, tt.now, got, tt.want)
		}
	}
}
