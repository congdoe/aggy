//go:build unix

package supervisor

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/congdoe/aggy/internal/adapter"
)

// The #85 review's findings (T6729), each a regression test.

// wantReturn quits the CLI and fails if Run does not return: a session
// that froze holding outMu never does.
func wantReturn(t *testing.T, c *fakeChild, done <-chan Exit) {
	t.Helper()
	c.quit(3)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run hung (outMu taken twice?)")
	}
}

// widen gives the terminal room for a whole status line.
func widen(t *testing.T, ts *termSide) {
	t.Helper()
	if err := pty.Setsize(ts.master, &pty.Winsize{Rows: 24, Cols: 160}); err != nil {
		t.Fatal(err)
	}
}

// pastDouble waits out a double-press window of d: nothing on screen
// shows the timer firing, so this one wait is a sleep.
func pastDouble(d time.Duration) { time.Sleep(d + 200*time.Millisecond) }

// TestReviewPendingPressOverOffer (H1): one switch-key press whose
// double-press timer fires after an offer opened leaves the offer alone;
// it used to open the list over it and freeze on outMu.
func TestReviewPendingPressOverOffer(t *testing.T) {
	const double = 300 * time.Millisecond
	ft := &fakeTools{}
	ts, c, done, sock := switchRunOn(t, false, adapter.Claude, func(o *Options) {
		o.Tools, o.ToolsAtLaunch = ft.get, ToolSet{}
		o.DoublePress = double
	})
	ft.put(ToolSet{"context7": "mcp"})
	ts.typeIn(t, "\x1c") // one press: the list would open after double
	_ = request(t, sock, toolsLine)
	poll(t, "the offer", []byte("same session -> claude"), ts.screen)
	pastDouble(double)
	if bytes.Contains(ts.screen(), []byte("aggy: switch vendor")) {
		t.Error("the list opened over the offer")
	}
	ts.typeIn(t, "\x1b")
	poll(t, "the offer to close", statusErased, ts.screen)
	wantReturn(t, c, done)
}

// TestReviewPendingPressOverAgentAsk (H1): the same with an agent's
// switch-request, the path that predates T6729.
func TestReviewPendingPressOverAgentAsk(t *testing.T) {
	const double = 300 * time.Millisecond
	ts, c, done, sock := switchRunOn(t, false, adapter.Claude, func(o *Options) { o.DoublePress = double })
	ts.typeIn(t, "\x1c")
	_ = request(t, sock, `{"type":"switch-request","vendor":"grok"}`)
	poll(t, "the offer", []byte("-> grok"), ts.screen)
	pastDouble(double)
	ts.typeIn(t, "\x1b")
	poll(t, "the offer to close", statusErased, ts.screen)
	wantReturn(t, c, done)
}

// TestReviewCodexNoteDoublePress (H1, L1): on Codex, the note's key
// queues the resume and a quick second press arms the double-press
// timer; it fires with the offer up and changes nothing. A key within
// the guard of the note's opening does not queue anything.
func TestReviewCodexNoteDoublePress(t *testing.T) {
	const double = 300 * time.Millisecond
	ft := &fakeTools{}
	ts, c, done, sock := switchRunOn(t, false, adapter.Codex, func(o *Options) {
		o.Tools, o.ToolsAtLaunch = ft.get, ToolSet{}
		o.DoublePress = double
	})
	ft.put(ToolSet{"context7": "mcp"})
	_ = request(t, sock, toolsLine)
	poll(t, "the note", []byte("Tools changed for codex"), ts.screen)
	time.Sleep(confirmGuard + 50*time.Millisecond) // past the note's guard
	ts.typeIn(t, "\x1c")
	poll(t, "the note to close", []byte(altScreenOff), ts.screen) // two presses in one read are one double press
	ts.typeIn(t, "\x1c")
	poll(t, "the offer", []byte("same session -> codex"), ts.screen)
	pastDouble(double)
	ts.typeIn(t, "\x1b")
	poll(t, "the offer to close", statusErased, ts.screen)
	wantReturn(t, c, done)
}

// TestReviewNoteKeyGuard (L1): the switch key right as the note appears
// is not taken as queuing the resume.
func TestReviewNoteKeyGuard(t *testing.T) {
	old := confirmGuard
	ft := &fakeTools{}
	ts, c, done, sock := switchRunOn(t, false, adapter.Codex, func(o *Options) {
		o.Tools, o.ToolsAtLaunch = ft.get, ToolSet{}
	})
	confirmGuard = time.Hour // set after switchRunOn, restored by its cleanup
	t.Cleanup(func() { confirmGuard = old })
	ft.put(ToolSet{"context7": "mcp"})
	_ = request(t, sock, toolsLine)
	poll(t, "the note", []byte("Tools changed for codex"), ts.screen)
	ts.typeIn(t, "\x1c")
	never(t, "an offer within the guard", []byte("same session -> codex"), ts.screen, 300*time.Millisecond)
	finish(t, c, done)
}

// TestReviewLimitBeatsResume (M1): a resume waiting for changed tools
// gives way to a limit's handoff.
func TestReviewLimitBeatsResume(t *testing.T) {
	ch := make(chan LimitHit, 1)
	ft := &fakeTools{}
	ts, c, done, sock := switchRunOn(t, false, adapter.Claude, limitOpts(ch, adapter.Grok), func(o *Options) {
		o.Tools, o.ToolsAtLaunch = ft.get, ToolSet{}
	})
	widen(t, ts)
	ft.put(ToolSet{"context7": "mcp"})
	_ = request(t, sock, toolsLine)
	poll(t, "the resume offer", []byte("same session -> claude"), ts.screen)
	ts.clearScreen()
	ch <- LimitHit{Vendor: adapter.Claude}
	poll(t, "the limit's offer", []byte("claude hit its limit, brief -> grok. Ctrl+"), ts.screen)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok, Brief: true}})
}

// TestReviewOfferRedrawn (M2): the tools changing again while the offer
// is up close it and ask again with the new list, guard re-armed.
func TestReviewOfferRedrawn(t *testing.T) {
	ts, c, done, sock, ft := toolsRun(t, false, adapter.Claude)
	widen(t, ts)
	ft.put(ToolSet{"context7": "mcp"})
	_ = request(t, sock, toolsLine)
	poll(t, "the offer", []byte("tools +context7, same session -> claude"), ts.screen)
	ft.put(ToolSet{"context7": "mcp", "other": "mcp"})
	_ = request(t, sock, toolsLine)
	poll(t, "the new offer", []byte("tools +context7 +other, same session -> claude"), ts.screen)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Claude, Next: Choice{Vendor: adapter.Claude, Resume: true}})
}

// TestReviewBusyChangeKept (M3): a change while another vendor's switch
// waits is not marked seen; once that switch is cancelled, the next
// notice offers it.
func TestReviewBusyChangeKept(t *testing.T) {
	ts, c, done, sock, ft := toolsRun(t, true, adapter.Claude)
	ts.typeIn(t, "\x1c2") // grok, waiting for the turn end
	poll(t, "the picker to close", []byte(altScreenOff), ts.screen)
	ft.put(ToolSet{"context7": "mcp"})
	_ = request(t, sock, toolsLine)
	ts.clearScreen()
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("Switching to grok"), ts.screen)
	ts.typeIn(t, "\x1b") // cancels grok
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	ts.clearScreen()
	_ = request(t, sock, toolsLine)
	_ = request(t, sock, stopLine)
	poll(t, "the resume offer", []byte("same session -> claude"), ts.screen)
	finish(t, c, done)
}

// TestReviewRevertClosesNote (L4): settings put back close a Codex note
// still on screen.
func TestReviewRevertClosesNote(t *testing.T) {
	ts, c, done, sock, ft := toolsRun(t, false, adapter.Codex)
	ft.put(ToolSet{"context7": "mcp"})
	_ = request(t, sock, toolsLine)
	poll(t, "the note", []byte("Tools changed for codex"), ts.screen)
	ts.clearScreen()
	ft.put(ToolSet{})
	_ = request(t, sock, toolsLine)
	poll(t, "the note to close", []byte(altScreenOff), ts.screen)
	finish(t, c, done)
}

// TestReviewOneOfferPerTurnEnd (L2): an offer turned down is not asked
// again, whatever the settings do, before the next turn end.
//
// The settings change to a new set in one step: toggling off and back on
// is racy to drive, since a read that lands after both is the set turned
// down, which is never offered again (CI failure on #87). The reads are
// slow, as on a loaded runner.
func TestReviewOneOfferPerTurnEnd(t *testing.T) {
	ft := &fakeTools{}
	slow := func() (ToolSet, error) { time.Sleep(50 * time.Millisecond); return ft.get() }
	ts, c, done, sock := switchRunOn(t, false, adapter.Claude, func(o *Options) { o.Tools, o.ToolsAtLaunch = slow, ToolSet{} })
	ft.put(ToolSet{"context7": "mcp"})
	_ = request(t, sock, toolsLine)
	poll(t, "the offer", []byte("same session -> claude"), ts.screen)
	ts.typeIn(t, "\x1b")
	poll(t, "the offer to close", statusErased, ts.screen)
	ts.clearScreen()
	ft.put(ToolSet{"context7": "mcp", "rtk": "hook"})
	_ = request(t, sock, toolsLine)
	never(t, "a second offer at that turn end", []byte("same session"), ts.screen, 300*time.Millisecond)
	_ = request(t, sock, stopLine)
	poll(t, "the offer at the next turn end", []byte("tools +context7 +rtk, same session -> claude"), ts.screen)
	finish(t, c, done)
}

// TestReviewCodexNoteEscWaitsTurn (L2): Esc on a Codex tools note counts
// as turning it down: a new change shows no note before the next turn
// end.
func TestReviewCodexNoteEscWaitsTurn(t *testing.T) {
	ts, c, done, sock, ft := toolsRun(t, false, adapter.Codex)
	ft.put(ToolSet{"context7": "mcp"})
	_ = request(t, sock, toolsLine)
	poll(t, "the note", []byte("Tools changed for codex: +context7 "), ts.screen)
	ts.typeIn(t, "\x1b")
	poll(t, "the note to close", []byte(altScreenOff), ts.screen)
	ts.clearScreen()
	ft.put(ToolSet{"context7": "mcp", "rtk": "mcp"})
	_ = request(t, sock, toolsLine)
	never(t, "a note at that turn end", []byte("Tools changed"), ts.screen, 300*time.Millisecond)
	_ = request(t, sock, `{"type":"hook-event","vendor":"codex","event":"Stop","payload":{"session_id":"`+childSID+`"}}`)
	poll(t, "the note at the next turn end", []byte("Tools changed for codex: +context7 +rtk "), ts.screen)
	finish(t, c, done)
}

// TestReviewStaleToolRead (L5): a read stuck past toolStale is given up
// on; a later notice reads again, and the stuck read's late answer is
// dropped.
func TestReviewStaleToolRead(t *testing.T) {
	old := toolStale
	toolStale = 50 * time.Millisecond
	t.Cleanup(func() { toolStale = old })
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	tools := func() (ToolSet, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			<-release
			return ToolSet{"stale": "mcp"}, nil
		}
		return ToolSet{"context7": "mcp"}, nil
	}
	ts, c, done, sock := switchRunOn(t, false, adapter.Claude, func(o *Options) {
		o.Tools, o.ToolsAtLaunch = tools, ToolSet{}
	})
	_ = request(t, sock, toolsLine) // stuck
	time.Sleep(2 * toolStale)       // the stuck read goes stale
	_ = request(t, sock, toolsLine)
	poll(t, "the offer from the new read", []byte("tools +context7, same session"), ts.screen)
	close(release)
	never(t, "the stuck read's answer", []byte("+stale"), ts.screen, 300*time.Millisecond)
	finish(t, c, done)
}

// TestReviewExitNoteResume (L6): a CLI slow to exit after a resume was
// confirmed is told about as a resume.
func TestReviewExitNoteResume(t *testing.T) {
	ts, c, done, sock, ft := toolsRun(t, false, adapter.Claude)
	ft.put(ToolSet{"context7": "mcp"})
	_ = request(t, sock, toolsLine)
	poll(t, "the offer", []byte("same session -> claude"), ts.screen)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	poll(t, "the late-exit note", []byte("aggy resumes claude on the same session"), ts.screen)
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Claude, Next: Choice{Vendor: adapter.Claude, Resume: true}})
}
