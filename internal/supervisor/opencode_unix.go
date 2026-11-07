//go:build unix

package supervisor

import (
	"bytes"
	"fmt"
	"syscall"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/hook"
)

// guardWait is how long after launch aggy waits for the OpenCode guard's
// report. A plugin that fails to load gives no error (fake model, T6930),
// so a missing report is the only sign. Tests shorten it.
var guardWait = 20 * time.Second

// guardCheck is the session's own event for that wait ending. It cannot
// come over the socket: its name fails eventRE.
const guardCheck = "guard check"

// startGuardWatch starts that wait, for OpenCode only.
func (s *session) startGuardWatch() {
	if s.o.Vendor != adapter.OpenCode || s.o.RunDir == "" {
		return
	}
	t := time.AfterFunc(guardWait, func() {
		select {
		case s.events <- hookEvent{name: guardCheck}:
		case <-s.quit:
		}
	})
	go func() {
		<-s.quit
		t.Stop()
	}()
}

// TODO(T6953): a session changed on screen (/new, /sessions, /open) makes the
// turn ends name another id, so they are ignored and no switch is offered, and
// a brief reads the session aggy launched. The re-review of T6932 found that
// /undo reaches the same harm. session.followSession is the way in, once there
// is evidence of a move; nothing calls it for OpenCode yet.

// openCodeEvent handles the events only OpenCode's plugin sends, and says
// whether that is all there is to do. Asked events go on to hookEvent's
// hold, like any event that is no turn end.
//
// Each is a hint, like every hook event: any process in the CLI's tree can
// send one. Forging a reply can only clear what a forged or real ask set,
// and the user still confirms the switch.
func (s *session) openCodeEvent(ev hookEvent) bool {
	if s.o.Vendor != adapter.OpenCode {
		return false
	}
	switch ev.name {
	case exitEnterCheck:
		s.finishExit()
		return true
	case guardCheck:
		if !s.guardSeen {
			s.log(diag.Rec{Event: diag.EvGuardMissing, Code: diag.CodeGuardMissing, Msg: "no report from aggy's OpenCode guard"})
			if !s.open {
				s.pk.tell("aggy has no report from its guard in OpenCode, so its refusals may not be running.",
					"OpenCode's own rules from aggy still hold. A plugin that fails to load gives no error, so aggy cannot say why;",
					"quit and start again, or see aggy diag.")
				s.openPicker()
			}
		}
		return true
	case hook.EventGuardLoaded:
		s.guardSeen = true
		s.log(diag.Rec{Event: diag.EvHookAccepted, Attrs: map[string]any{"event": hookName(ev.name)}})
		return true
	case hook.EventPermissionAsked, hook.EventQuestionAsked:
		if ev.request != "" {
			if s.asked == nil {
				s.asked = map[string]bool{}
			}
			s.asked[ev.request] = true
		}
		s.askStale, s.askTold = false, false
		return false
	case hook.EventPermissionReplied, hook.EventQuestionReplied:
		delete(s.asked, ev.request)
		// A reply may come from a mouse click, which is no typing to the
		// CLI. Once none is open the hold has done its job; a turn end
		// still has to come before a switch.
		if len(s.asked) == 0 {
			s.held, s.askStale, s.askTold = false, false, false
		}
		s.log(diag.Rec{Event: diag.EvHookAccepted, Attrs: map[string]any{"event": hookName(ev.name)}})
		return true
	}
	return false
}

// askOpen reports a permission dialog or a question that has had no reply:
// over a dialog, the exit command's letters are dropped and its Enter
// chooses Allow once (fake model, T6930).
func (s *session) askOpen() bool { return len(s.asked) > 0 }

// typedOf is what in b the user typed. On OpenCode that leaves out mouse and
// focus reports, since it tells aggy when a click answered a dialog; other
// vendors keep terminalReport's rule that a click may submit.
func (s *session) typedOf(b []byte) []byte {
	if s.o.Vendor == adapter.OpenCode {
		return dropReports(b)
	}
	return b
}

// exitEnterGap separates the exit command's letters from its Enter on
// OpenCode. The full screen opens a slash menu on the letters, and an Enter
// in the same burst is lost: the command sat in its composer and the switch
// timed out (live, 2.2.5). Sent 0.6s apart it runs. mini took either.
var exitEnterGap = 500 * time.Millisecond

// exitEnterCheck is the session's own event when the gap is over. Like
// guardCheck, its name cannot come over the socket.
const exitEnterCheck = "exit enter check"

// typeExitCommand types the vendor's exit command and its Enter. On OpenCode
// the Enter waits for the gap, and comes through the session's loop, so what
// happened in the gap is known first (finishExit).
func (s *session) typeExitCommand() {
	cmd, _ := adapter.ExitCommand(s.o.Vendor)
	if s.o.Vendor != adapter.OpenCode || exitEnterGap <= 0 {
		s.toChild.push([]byte(cmd + "\r"))
		return
	}
	s.toChild.push([]byte(cmd))
	s.exitGap, s.gapKeys = true, nil
	time.AfterFunc(exitEnterGap, func() {
		select {
		case s.events <- hookEvent{name: exitEnterCheck}:
		case <-s.quit:
		}
	})
}

// finishExit ends the gap. It sends the Enter unless the gap changed things:
// over a dialog that Enter would choose Allow once (fake model, T6930), and
// behind keys the user just typed it would send them with the command.
//
// This leaves one race (docs/design.md): a dialog OpenCode has opened but not
// yet reported, since its report comes through a process the plugin starts.
func (s *session) finishExit() {
	if !s.exitGap {
		return
	}
	cmd, _ := adapter.ExitCommand(s.o.Vendor)
	keys := s.gapKeys
	s.exitGap, s.gapKeys = false, nil
	to := s.sw.to
	cancel := func(how string) {
		s.log(diag.Rec{Event: diag.EvSwitchCancelled, Cause: s.sw.rec, Attrs: map[string]any{"how": how, "to": string(to)}})
		s.setSwitch(pendingSwitch{})
		s.exitBy = nil
	}
	switch {
	case to == "":
		// Esc on the list in the gap called the switch off, and logged it. No
		// Enter: OpenCode would exit, and aggy with it.
		s.exitBy = nil
		s.takeBackExit(cmd, keys)
	case s.askOpen():
		cancel("dialog_in_gap")
		// The letters may be in the prompt, or a dialog may have dropped
		// them, so they count as unsent text until the user clears them.
		s.compLen, s.lenKnown, s.dirty = len(cmd), true, true
		lines := []string{fmt.Sprintf("%s asked you something as aggy was about to exit it, so aggy did not press Enter and nothing was answered.", s.o.Vendor),
			fmt.Sprintf("%s may be in its prompt: answer the question, clear that text, and pick %s again.", cmd, to)}
		// They were typed before anyone could see the dialog, and an Enter among
		// them would choose Allow once.
		if len(keys) > 0 {
			lines = append(lines, "What you typed in that moment was not sent.")
		}
		s.pk.tell(lines...)
		s.openPicker()
	case len(keys) > 0:
		// Take the command back and pass on what was typed, so nothing is
		// lost and nothing is sent.
		cancel("typed_in_gap")
		s.toChild.push(bytes.Repeat([]byte{0x7f}, len(cmd)))
		s.send(keys)
		s.pk.tell(fmt.Sprintf("You typed as aggy was about to exit %s, so it stayed, with your text in its prompt.", s.o.Vendor),
			fmt.Sprintf("Pick %s again to switch.", to))
		s.openPicker()
	default:
		s.toChild.push([]byte("\r"))
	}
}

// takeBackExit removes the exit command's letters after the switch was called
// off in the gap, and passes on what was typed. With a dialog up the letters
// may have been dropped, and backspaces or keys would go to the dialog, so
// the letters count as unsent text and the keys are not sent.
func (s *session) takeBackExit(cmd string, keys []byte) {
	if s.askOpen() {
		s.compLen, s.lenKnown, s.dirty = len(cmd), true, true
		lines := []string{fmt.Sprintf("%s asked you something as the switch was called off, so %s may be in its prompt.", s.o.Vendor, cmd)}
		if len(keys) > 0 {
			lines = append(lines, "What you typed in that moment was not sent.")
		}
		s.pk.tell(lines...)
		s.openPicker()
		return
	}
	s.toChild.push(bytes.Repeat([]byte{0x7f}, len(cmd)))
	if len(keys) > 0 {
		s.send(keys)
	}
}

// tellAskWaits tells the user why a switch to v is waiting on an ask with no
// answer. Once a turn has ended with it still open, its reply was lost, or
// the turn could not have ended (a plugin reloaded, a report that failed), and
// nothing else would clear it, so picking v again goes on. Before that the
// dialog may well be up, and only an answer or a turn end moves things on.
func (s *session) tellAskWaits(v adapter.Vendor) {
	s.askTold = s.askStale
	s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeSwitchHeld, Cause: s.sw.rec, Msg: "the switch waits: a permission or question has no reply", Attrs: map[string]any{"to": string(v), "stale": s.askStale}})
	next := fmt.Sprintf("Check its prompt and answer it. aggy switches to %s after the turn ends.", v)
	if s.askStale {
		next = fmt.Sprintf("Check its prompt and answer it. If nothing is waiting there, pick %s again to go on.", v)
	}
	s.pk.tell(fmt.Sprintf("%s reported a permission or a question with no answer yet, so aggy will not type its exit command over it.", s.o.Vendor), next)
	s.openPicker()
}

// releaseAsks drops every open ask, on the user's word that none is waiting.
func (s *session) releaseAsks() {
	s.asked = nil
	s.held, s.askStale, s.askTold = false, false, false
	s.atPrompt = s.atPrompt || s.turnRan
}

// hangupWait is how long OpenCode has to exit after its terminal is gone.
var hangupWait = 10 * time.Second

// killAfterHangup ends OpenCode when a hang-up or a SIGTERM to aggy does not.
// The full screen ignored both and ran on with no terminal (live, 2.2.5), and
// under full auto a session nobody can see should not keep running. A SIGKILL
// to aggy cannot be handled, so a session can still outlive that one.
func (s *session) killAfterHangup(child interface{ Signal(syscall.Signal) error }) {
	if s.o.Vendor == adapter.OpenCode && s.hangupKill == nil {
		s.hangupKill = time.AfterFunc(hangupWait, func() { _ = child.Signal(syscall.SIGKILL) })
	}
}

// stopHangupKill drops the kill once the CLI has gone, so a reused process
// group is never signalled.
func (s *session) stopHangupKill() {
	if s.hangupKill != nil {
		s.hangupKill.Stop()
	}
}

// dropReports returns b without what the terminal reports by itself rather
// than the user typing: SGR mouse reports (ESC [ < ... M or m), focus (ESC
// [ I and ESC [ O), and private-mode replies and notices (ESC [ ? ...). A
// CLI on the alternate screen with mouse tracking gets these all the time
// (opencode turns on ?1003 and ?1006, live, T6930); counted as typing, a
// moved mouse would close a prompt aggy opened or block a switch as unsent
// text. A report split across reads stays in: when in doubt it counts. b
// itself is returned when there is none.
func dropReports(b []byte) []byte {
	var out []byte
	start := 0
	for i := 0; i+2 < len(b); i++ {
		if b[i] != esc || b[i+1] != '[' {
			continue
		}
		n := reportLen(b[i:])
		if n == 0 {
			continue
		}
		out = append(out, b[start:i]...)
		i += n - 1
		start = i + 1
	}
	if start == 0 {
		return b
	}
	return append(out, b[start:]...)
}

// reportLen is the length of the report at the start of b (ESC [ first),
// or 0.
func reportLen(b []byte) int {
	switch b[2] {
	case 'I', 'O':
		return 3
	case '<', '?':
	default:
		return 0
	}
	for i := 3; i < len(b) && i < 64; i++ {
		c := b[i]
		switch {
		case c >= '0' && c <= '9' || c == ';' || c == ':':
		case c >= 0x40 && c <= 0x7e:
			return i + 1
		default:
			return 0
		}
	}
	return 0
}
