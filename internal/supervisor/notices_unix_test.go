//go:build unix

package supervisor

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// streams checks that the CLI's output reaches the terminal, past the quiet
// gap, with no key pressed and no notice holding it.
func streams(t *testing.T, ts *termSide, c *fakeChild, notice string) {
	t.Helper()
	time.Sleep(5 * switchQuietTest)
	c.print("STREAMED")
	poll(t, "the CLI's output", []byte("STREAMED"), ts.screen)
	if strings.Contains(string(ts.screen()), notice) {
		t.Errorf("%q opened", notice)
	}
}

// TestNoticesHoldNoOutput (T6941): a notice must never hold a person's output
// while a turn may be running; each comes at the next turn end instead.
func TestNoticesHoldNoOutput(t *testing.T) {
	t.Run("a limit's note after Enter", func(t *testing.T) {
		ch := make(chan LimitHit, 1)
		ts, c, done, sock := switchRunOn(t, false, adapter.Codex, limitOpts(ch, adapter.Grok))
		ts.typeIn(t, "go\r")
		c.waitGot(t, []byte("go\r"))
		ch <- LimitHit{Vendor: adapter.Codex}
		streams(t, ts, c, "hit its limit")
		_ = request(t, sock, codexStop)
		poll(t, "the note at the turn end", []byte("codex hit its limit. grok has quota left"), ts.screen)
		finish(t, c, done)
	})
	t.Run("a no-quota note after any input", func(t *testing.T) {
		ch := make(chan LimitHit, 1)
		ts, c, done, _ := switchRunTurned(t, adapter.Claude, limitOpts(ch))
		ts.typeIn(t, "x")
		c.waitGot(t, []byte("x"))
		ch <- LimitHit{Vendor: adapter.Claude}
		streams(t, ts, c, "hit its limit")
		finish(t, c, done)
	})
	t.Run("a limit with no turn at all: the list says it", func(t *testing.T) {
		ch := make(chan LimitHit, 1)
		ts, c, done, _ := switchRunOn(t, false, adapter.Codex, limitOpts(ch, adapter.Grok))
		ts.typeIn(t, "go\r")
		c.waitGot(t, []byte("go\r"))
		ch <- LimitHit{Vendor: adapter.Codex}
		time.Sleep(5 * switchQuietTest)
		ts.typeIn(t, "\x1c")
		poll(t, "the list's line", []byte("codex hit its limit."), ts.screen)
		ts.typeIn(t, "\x1b")
		poll(t, "the list to close", []byte(altScreenOff), ts.screen)
		finish(t, c, done)
	})
	t.Run("a clean turn end drops a waiting limit's note", func(t *testing.T) {
		ch := make(chan LimitHit, 1)
		ts, c, done, sock := switchRunTurned(t, adapter.Claude, limitOpts(ch))
		ts.typeIn(t, "go\r")
		c.waitGot(t, []byte("go\r"))
		ch <- LimitHit{Vendor: adapter.Claude}
		time.Sleep(3 * switchQuietTest)
		_ = request(t, sock, stopLine)
		never(t, "the note", []byte("hit its limit"), ts.screen, 300*time.Millisecond)
		finish(t, c, done)
	})
	for _, enter := range []string{"hi\x1b[13u", "hi\x1b[13;1u", "hi\x1b[27;1;13~"} {
		t.Run("the unsent-text note after "+enter[2:], func(t *testing.T) {
			ts, c, done, _ := switchRunTurned(t, adapter.Claude)
			ts.typeIn(t, enter)
			c.waitGot(t, []byte(enter))
			ts.typeIn(t, "\x1c1")
			poll(t, "the list to close", []byte(altScreenOff), ts.screen)
			streams(t, ts, c, "you typed text")
			finish(t, c, done)
		})
	}
	t.Run("a tool change's note after an Enter as CSI 13 u", func(t *testing.T) {
		ts, c, done, sock, ft := toolsRun(t, false, adapter.Codex)
		ts.typeIn(t, "hi\x1b[13u")
		c.waitGot(t, []byte("hi\x1b[13u"))
		ft.put(ToolSet{"context7": "mcp"})
		_ = request(t, sock, toolsLine)
		streams(t, ts, c, "Tools changed for codex")
		_ = request(t, sock, codexStop)
		poll(t, "the note at the turn end", []byte("Tools changed for codex"), ts.screen)
		finish(t, c, done)
	})
	t.Run("a terminal's reply is not input", func(t *testing.T) {
		ts, c, done, _ := switchRunOn(t, false, adapter.Codex)
		ts.typeIn(t, "\x1b[I")
		c.waitGot(t, []byte("\x1b[I"))
		ts.typeIn(t, "\x1c1")
		poll(t, "the note", []byte("will not type /exit into it"), ts.screen)
		finish(t, c, done)
	})
}

// TestNoTurnListChanges (T6941): the list's no-turn words fit a line drawn over
// the CLI's screen, do not promise a turn end, and go when a turn end comes.
func TestNoTurnListChanges(t *testing.T) {
	ts, c, done, sock := switchRunOn(t, false, adapter.Claude)
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte(noTurnList), ts.screen)
	if s := string(ts.screen()); strings.Contains(s, "at the turn end") {
		t.Errorf("the footer promises a turn end: %q", s)
	}
	for _, l := range strings.Split(string(ts.screen()), "\r\n") {
		if strings.Contains(l, "no turn end") || strings.Contains(l, noTurnList) || strings.Contains(l, "some CLIs") {
			if len(l) > 79 {
				t.Errorf("%d columns: %q", len(l), l)
			}
		}
	}
	ts.clearScreen()
	_ = request(t, sock, stopLine)
	poll(t, "the redrawn list", []byte("Number or Enter to switch at the turn end"), ts.screen)
	if strings.Contains(string(ts.screen()), noTurnList) {
		t.Error("the list still says to quit")
	}
	ts.typeIn(t, "\x1b")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	finish(t, c, done)
}

// TestNoTurnNoteFits (T6941): the note's lines fit 80 columns too.
func TestNoTurnNoteFits(t *testing.T) {
	ts, c, done, _ := switchRunOn(t, false, adapter.Claude)
	ts.typeIn(t, "\x1c1")
	poll(t, "the note", []byte(noTurnNote), ts.screen)
	for _, l := range strings.Split(string(ts.screen()), "\r\n") {
		if strings.Contains(l, "turn end") || strings.Contains(l, "dialog") || strings.Contains(l, "yourself") {
			if len(l) > 79 {
				t.Errorf("%d columns: %q", len(l), l)
			}
		}
	}
	finish(t, c, done)
}

// TestSignalAfterConfirmStops (T6941): a signal taken after the user confirmed,
// but before the picked session's check lets aggy type the exit command, stops
// the switch, though the CLI survives it and exits cleanly.
func TestSignalAfterConfirmStops(t *testing.T) {
	release := make(chan struct{})
	ts, c, done, _ := switchRunTurned(t, adapter.Claude, func(o *Options) {
		o.Children = func(v adapter.Vendor) []ChildChoice {
			if v == adapter.Grok {
				return []ChildChoice{{"g2", "latest"}, {"g1", "older"}}
			}
			return nil
		}
		o.HasChild = func(adapter.Vendor, string) bool {
			<-release
			return true
		}
	})
	ts.typeIn(t, "\x1c2")
	poll(t, "the child list", []byte("which session?"), ts.screen)
	ts.typeIn(t, "2")
	poll(t, "the confirmation", []byte("-> grok"), ts.screen)
	confirm(t, ts)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.sigs:
	case <-time.After(waitLimit):
		t.Fatal("the signal was not passed on")
	}
	close(release)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{})
	poll(t, "why", []byte("aggy was sent a signal, so aggy did not switch to grok."), ts.screen)
}
