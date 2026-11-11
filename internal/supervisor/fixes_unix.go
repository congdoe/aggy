//go:build unix

package supervisor

import (
	"fmt"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
)

// Fixes asked in place (T6979): where a switch is refused for something aggy
// can put right itself, its own page says so and offers it, and a y on the
// page makes the fix (an aggy command run as a typed /aggy command is) and
// goes on. The page is keyboard only, as every page is: nothing a hook or the
// model sends reaches its y.

// offerSwitchFix opens the fix page for a switch to v that CanSwitch refused
// with err, when Options.SwitchFix names one, and reports whether it did. A y
// runs the fix, and the switch goes on once it succeeded.
func (s *session) offerSwitchFix(v adapter.Vendor, c Choice, keyed bool, err error) bool {
	if s.o.SwitchFix == nil || s.o.Aggy == nil {
		return false
	}
	lines, confirm, ok := s.o.SwitchFix(v, err)
	if !ok || len(confirm) == 0 {
		return false
	}
	s.openFixPage("aggy "+strings.Join(confirm, " "), lines, confirm,
		fmt.Sprintf("y does it, then switches to %s", v), &switchAfter{c: c, keyed: keyed})
	return true
}

// offerHooks opens, once a session, the page that offers to install aggy's
// hooks for the CLI on screen, after an agent's switch request their absence
// refused. The request stays refused: this session started without them.
func (s *session) offerHooks() {
	if s.hooksOffered || len(s.o.HooksFix) == 0 || s.o.Aggy == nil || s.open {
		return
	}
	s.hooksOffered = true
	v := s.o.Vendor
	s.openFixPage("aggy "+strings.Join(s.o.HooksFix, " "), []string{
		fmt.Sprintf("%s asked aggy to switch, and aggy refused: its hooks for %s are not installed,", v, v),
		"so aggy never sees a turn end in this session to switch at.",
		"",
		fmt.Sprintf("y installs them now (aggy %s); aggy unwire undoes it.", strings.Join(s.o.HooksFix, " ")),
		fmt.Sprintf("This %s started without them, and aggy cannot tell whether it loads hooks", v),
		"added while it runs, so start a new session afterwards for the switch to work.",
		"Ctrl+\\ switches now without them.",
	}, s.o.HooksFix, "y installs them", nil)
}

// openFixPage opens a page whose y runs confirm, answering its question yes.
func (s *session) openFixPage(title string, lines, confirm []string, label string, then *switchAfter) {
	if s.open {
		s.closePicker()
	}
	s.cmdSeq++
	s.pk.list()
	s.pk.page = &page{title: title, seq: s.cmdSeq, lines: lines, confirm: confirm, confirmLabel: label, answer: true, then: then}
	s.viewPaste = pasteFilter{}
	s.log(diag.Rec{Event: diag.EvCommandCaught, Attrs: map[string]any{"command": title, "via": "fix"}})
	s.openPicker()
}

// fixDone goes on with the switch a fix page held, once its command
// succeeded (said "Done" or "Nothing to change"), and reports whether it did.
func (s *session) fixDone(g *page, out string) bool {
	if g.then == nil || g.busy {
		return false
	}
	then := g.then
	g.then = nil
	if !strings.Contains(out, "Done (run ") && !strings.Contains(out, "Nothing to change") {
		return false
	}
	s.closeView()
	s.pickSwitch(then.c, then.keyed)
	return true
}
