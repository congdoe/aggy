//go:build unix

package supervisor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

const (
	movedSID  = "99999999-2222-4333-8444-555555555555"
	nestedSID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

// The lineages below: aggy hook, its shell, then the CLI (pid 100), the way the
// CLI's own hooks run; and a nested CLI's: aggy hook, its shell, the nested
// CLI, the agent's shell tool, then the CLI.
var (
	ownChain    = []int{501, 500, 100}
	ownChain2   = []int{601, 600, 100} // a later hook of the CLI
	nestedChain = []int{701, 700, 650, 640, 100}
	workerChain = []int{801, 800, 111, 100} // one more process above the shell
	sideChain   = []int{901, 900, 222}      // as deep, under another process
)

// TestFollowSession (T6901): which SessionStart moves the id. This is the attack
// on the design: a nested claude -p or codex exec, with its own real hooks or
// with a forged source, must not move it, since then its Stop would count.
func TestFollowSession(t *testing.T) {
	own := hookEvent{name: "Stop", sessionID: childSID, chain: ownChain}
	tests := []struct {
		name   string
		vendor adapter.Vendor
		before []hookEvent
		ev     hookEvent
		moves  bool
	}{
		{"/clear from the CLI", adapter.Claude, []hookEvent{own}, hookEvent{sessionID: movedSID, source: "clear", chain: ownChain2}, true},
		{"/resume from the CLI", adapter.Claude, []hookEvent{own}, hookEvent{sessionID: movedSID, source: "resume", chain: ownChain2}, true},
		{"a fork from the CLI", adapter.Claude, []hookEvent{own}, hookEvent{sessionID: movedSID, source: "fork", chain: ownChain2}, true},
		{"codex /clear from the CLI", adapter.Codex, []hookEvent{own}, hookEvent{sessionID: movedSID, source: "clear", chain: ownChain2}, true},
		{"grok, no source recorded", adapter.Grok, []hookEvent{own}, hookEvent{sessionID: movedSID, chain: ownChain2}, true},
		{"a nested claude -p starts", adapter.Claude, []hookEvent{own}, hookEvent{sessionID: nestedSID, source: "startup", chain: nestedChain}, false},
		{"a nested claude -p resumes", adapter.Claude, []hookEvent{own}, hookEvent{sessionID: nestedSID, source: "resume", chain: nestedChain}, false},
		{"a nested CLI forges clear", adapter.Claude, []hookEvent{own}, hookEvent{sessionID: nestedSID, source: "clear", chain: nestedChain}, false},
		{"a nested codex exec forges clear", adapter.Codex, []hookEvent{own}, hookEvent{sessionID: nestedSID, source: "clear", chain: nestedChain}, false},
		{"a nested grok", adapter.Grok, []hookEvent{own}, hookEvent{sessionID: nestedSID, chain: nestedChain}, false},
		{"a nested CLI that resumes the on-screen id cannot deepen the host", adapter.Claude,
			[]hookEvent{own, {name: "Stop", sessionID: childSID, chain: nestedChain}},
			hookEvent{sessionID: nestedSID, source: "clear", chain: nestedChain}, false},
		{"as deep, under another process", adapter.Claude, []hookEvent{{name: "Stop", sessionID: childSID, chain: workerChain}},
			hookEvent{sessionID: movedSID, source: "clear", chain: []int{1, 2, 3, 100}}, false},
		{"same worker", adapter.Claude, []hookEvent{{name: "Stop", sessionID: childSID, chain: workerChain}},
			hookEvent{sessionID: movedSID, source: "clear", chain: []int{1, 2, 111, 100}}, true},
		{"as deep, under nothing of the CLI's", adapter.Claude, []hookEvent{own}, hookEvent{sessionID: movedSID, source: "clear", chain: sideChain}, false},
		{"startup from the CLI's depth", adapter.Claude, []hookEvent{own}, hookEvent{sessionID: movedSID, source: "startup", chain: ownChain2}, false},
		{"compact from the CLI's depth", adapter.Claude, []hookEvent{own}, hookEvent{sessionID: movedSID, source: "compact", chain: ownChain2}, false},
		{"no source", adapter.Claude, []hookEvent{own}, hookEvent{sessionID: movedSID, chain: ownChain2}, false},
		{"no hook of the session seen yet", adapter.Claude, nil, hookEvent{sessionID: movedSID, source: "clear", chain: ownChain2}, false},
		{"another session's hook is no baseline", adapter.Claude, []hookEvent{{name: "Stop", sessionID: nestedSID, chain: ownChain}},
			hookEvent{sessionID: movedSID, source: "clear", chain: ownChain2}, false},
		{"an id that is no id", adapter.Claude, []hookEvent{own}, hookEvent{sessionID: "--dangerously", source: "clear", chain: ownChain2}, false},
		{"no chain", adapter.Claude, []hookEvent{own}, hookEvent{sessionID: movedSID, source: "clear"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &session{toChild: newByteQueue(maxQueued), lenKnown: true, quit: make(chan struct{}),
				o: Options{Vendor: tt.vendor, SessionID: func() string { return childSID }}}
			for _, b := range tt.before {
				s.hookEvent(b)
			}
			tt.ev.name = "SessionStart"
			s.hookEvent(tt.ev)
			if got := s.followed != ""; got != tt.moves {
				t.Fatalf("followed %q, want a move %v", s.followed, tt.moves)
			}
			wasRan := s.turnRan
			s.turnRan = false
			s.hookEvent(hookEvent{name: "Stop", sessionID: tt.ev.sessionID, chain: tt.ev.chain})
			if s.turnRan != tt.moves {
				t.Errorf("its Stop counted = %v, want %v", s.turnRan, tt.moves)
			}
			s.turnRan = false
			s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})
			if s.turnRan == tt.moves {
				t.Errorf("the old session's Stop counted = %v, want %v", s.turnRan, !tt.moves)
			}
			s.turnRan = wasRan
		})
	}
}

// TestFollowClearsDoubt (T6901): /clear is a leading /, whose Enter leaves the
// prompt in doubt, and it ends no turn. The move settles both, unless the user
// typed after that Enter.
func TestFollowClearsDoubt(t *testing.T) {
	start := func() *session {
		s := &session{toChild: newByteQueue(maxQueued), lenKnown: true, atPrompt: true, quit: make(chan struct{}),
			o: Options{Vendor: adapter.Claude, SessionID: func() string { return childSID }}}
		s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})
		s.send([]byte("/clear"))
		s.send([]byte("\r"))
		if !s.dirty || s.atPrompt {
			t.Fatalf("after /clear: dirty %v, atPrompt %v", s.dirty, s.atPrompt)
		}
		return s
	}
	clear := hookEvent{name: "SessionStart", sessionID: movedSID, source: "clear", chain: ownChain2}

	s := start()
	s.hookEvent(clear)
	if s.dirty || !s.atPrompt || s.inputSinceTurn {
		t.Errorf("after the move: dirty %v, atPrompt %v, inputSinceTurn %v", s.dirty, s.atPrompt, s.inputSinceTurn)
	}
	s.send([]byte("x"))
	if !s.dirty {
		t.Error("typing after the move was not tracked")
	}

	s = start()
	s.send([]byte("draft"))
	s.hookEvent(clear)
	if !s.dirty {
		t.Error("text typed after the Enter was dropped")
	}

	s = start()
	s.hookEvent(hookEvent{name: "SessionStart", sessionID: nestedSID, source: "clear", chain: nestedChain})
	if !s.dirty || s.atPrompt {
		t.Errorf("a refused move changed the tracker: dirty %v, atPrompt %v", s.dirty, s.atPrompt)
	}

	// A hold stands: a move says no turn runs, not that a prompt was answered.
	s = start()
	s.hookEvent(hookEvent{name: "Notification", sessionID: movedSID})
	s.hookEvent(clear)
	if s.atPrompt || !s.held {
		t.Errorf("a hold did not stand: atPrompt %v, held %v", s.atPrompt, s.held)
	}
}

// TestSwitchAfterClear (T6901): the everyday flow over the socket, with the real
// lineage: a turn, /clear, a turn, and the switch fires.
func TestSwitchAfterClear(t *testing.T) {
	var followed []string
	got := make(chan struct{}, 1)
	ts, c, done, sock := switchRunTurned(t, adapter.Claude, func(o *Options) {
		o.OnFollow = func(id, via string) {
			followed = []string{id, via}
			got <- struct{}{}
		}
	})
	ts.typeIn(t, "/clear\r")
	c.waitGot(t, []byte("/clear\r"))
	_ = request(t, sock, fmt.Sprintf(`{"type":"hook-event","vendor":"claude","event":"SessionStart","payload":{"session_id":%q,"source":"clear"}}`, movedSID))
	select {
	case <-got:
	case <-time.After(waitLimit):
		t.Fatal("OnFollow was not called")
	}
	if followed[0] != movedSID || followed[1] != "clear" {
		t.Errorf("followed %v", followed)
	}
	ts.typeIn(t, "hello\r")
	c.waitGot(t, []byte("hello\r"))
	ts.typeIn(t, "\x1c2")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	never(t, "an offer mid-turn", []byte("-> grok. Ctrl+"), ts.screen, 100*time.Millisecond)
	_ = request(t, sock, fmt.Sprintf(`{"type":"hook-event","vendor":"claude","event":"Stop","payload":{"session_id":%q}}`, movedSID))
	poll(t, "the status line", []byte("-> grok. Ctrl+"), ts.screen)
	finish(t, c, done)
}

// TestAgentSwitchWithoutHooks (T6848): with aggy's hooks not installed, an
// agent's switch is refused with the fix, so it never waits forever, until a
// hook arrives to say otherwise; the list says why too.
func TestAgentSwitchWithoutHooks(t *testing.T) {
	ts, c, done, sock := switchRunOn(t, false, adapter.Claude, func(o *Options) { o.HooksMissing = true })
	ask := `{"type":"switch-request","vendor":"grok"}`
	if got := request(t, sock, ask); !strings.Contains(got, "aggy hooks install") || !strings.Contains(got, "switch.hooks_missing") {
		t.Errorf("reply = %s", got)
	}
	ts.typeIn(t, "\x1c")
	poll(t, "the list's words", []byte("aggy's hooks for claude are not installed"), ts.screen)
	ts.typeIn(t, "\x1b")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	// A hook from another aggy's binary still reports.
	_ = request(t, sock, stopLine)
	if got := request(t, sock, ask); !strings.Contains(got, `"ok":true`) {
		t.Errorf("after a hook arrived: reply = %s", got)
	}
	finish(t, c, done)
}
