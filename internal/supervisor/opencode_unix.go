//go:build unix

package supervisor

import (
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
		return false
	case hook.EventPermissionReplied, hook.EventQuestionReplied:
		delete(s.asked, ev.request)
		// A reply may come from a mouse click, which is no typing to the
		// CLI. Once none is open the hold has done its job; a turn end
		// still has to come before a switch.
		if len(s.asked) == 0 {
			s.held = false
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

// typeExitCommand types the vendor's exit command and its Enter.
func (s *session) typeExitCommand() {
	cmd, _ := adapter.ExitCommand(s.o.Vendor)
	if s.o.Vendor != adapter.OpenCode || exitEnterGap <= 0 {
		s.toChild.push([]byte(cmd + "\r"))
		return
	}
	s.toChild.push([]byte(cmd))
	time.AfterFunc(exitEnterGap, func() { s.toChild.push([]byte("\r")) })
}

// hangupWait is how long OpenCode has to exit after its terminal is gone.
var hangupWait = 10 * time.Second

// killAfterHangup ends OpenCode when a hang-up does not. The full screen
// ignored SIGHUP and SIGTERM and ran on with no terminal (live, 2.2.5), and
// under full auto a session nobody can see should not keep running.
func (s *session) killAfterHangup(child Child) {
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
