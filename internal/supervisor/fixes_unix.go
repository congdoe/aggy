//go:build unix

package supervisor

import (
	"fmt"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
)

// Fixes asked in place (T6979): where a switch is refused for something aggy
// can put right itself, its own page says so and offers it, and a y on the
// page makes the fix (an aggy command run as a typed /aggy command is) and
// goes on. The page is keyboard only, as every page is: nothing a hook or the
// model sends reaches its y.
//
// A page opens by itself only for a switch the person picked (#133 review).
// One the person did not ask for (an agent's switch request, a switch a hook
// sent) is told in a note, as aggy's other notes are: any key goes on to the
// CLI, and only the switch key opens the page. Every fix page ignores keys
// that arrive within checkTypeAhead of it opening.

// fixOffer is a fix page waiting behind a note for the person's switch key.
type fixOffer struct {
	title, label   string
	lines, confirm []string
	then           *switchAfter
}

// offerSwitchFix offers the fix for a switch to v that CanSwitch refused
// with err, when Options.SwitchFix names one, and reports whether it did. A
// switch the person picked opens the page; one a hook sent is told in a note.
// A y runs the fix, and the switch goes on once it succeeded.
func (s *session) offerSwitchFix(v adapter.Vendor, c Choice, keyed bool, err error) bool {
	if s.o.SwitchFix == nil || s.o.Aggy == nil {
		return false
	}
	lines, confirm, ok := s.o.SwitchFix(v, err)
	if !ok || len(confirm) == 0 {
		return false
	}
	f := &fixOffer{title: "aggy " + strings.Join(confirm, " "), lines: lines, confirm: confirm,
		label: fmt.Sprintf("y does it, then switches to %s", v), then: &switchAfter{c: c, keyed: keyed}}
	if s.agentPick {
		s.noteFix(f, fmt.Sprintf("A switch to %s was asked for, and aggy cannot switch yet: %s.", v, firstLine(lines)))
		return true
	}
	s.openFixPage(f.title, f.lines, f.confirm, f.label, f.then)
	return true
}

// offerHooks tells, once a session, that aggy can install its hooks for the
// CLI on screen, after an agent's switch request their absence refused. The
// request stays refused: this session started without them. The page opens
// only on the person's switch key at the note.
func (s *session) offerHooks() {
	if s.hooksOffered || len(s.o.HooksFix) == 0 || s.o.Aggy == nil || s.open {
		return
	}
	s.hooksOffered = true
	v := s.o.Vendor
	s.noteFix(&fixOffer{title: "aggy " + strings.Join(s.o.HooksFix, " "), lines: []string{
		fmt.Sprintf("%s asked aggy to switch, and aggy refused: its hooks for %s are not installed,", v, v),
		"so aggy never sees a turn end in this session to switch at.",
		"",
		fmt.Sprintf("y installs them now (aggy %s); aggy unwire undoes it.", strings.Join(s.o.HooksFix, " ")),
		fmt.Sprintf("This %s started without them, and aggy cannot tell whether it loads hooks", v),
		"added while it runs, so start a new session afterwards for the switch to work.",
		"Ctrl+\\ switches now without them.",
	}, confirm: s.o.HooksFix, label: "y installs them"},
		fmt.Sprintf("%s asked to switch; aggy refused, as its hooks for %s are not installed.", v, v))
}

// noteFix tells of f in a one-line note naming the key that opens its page.
func (s *session) noteFix(f *fixOffer, what string) {
	if s.open {
		s.closePicker()
	}
	s.pk.tell(what, fmt.Sprintf("Press %s for aggy's page that offers %s; any other key goes on.", keyName(s.pk.key), f.title))
	s.pk.fix = f
	s.log(diag.Rec{Event: diag.EvCommandCaught, Attrs: map[string]any{"command": f.title, "via": "fix_note"}})
	s.openPicker()
}

// firstLine is lines' first line without its full stop, or a plain stand-in.
func firstLine(lines []string) string {
	if len(lines) == 0 {
		return "a fix is needed first"
	}
	return strings.TrimSuffix(lines[0], ".")
}

// openFixPage opens a page whose y runs confirm, answering its question yes.
// Keys within checkTypeAhead of it opening do nothing.
func (s *session) openFixPage(title string, lines, confirm []string, label string, then *switchAfter) {
	if s.open {
		s.closePicker()
	}
	s.cmdSeq++
	s.pk.list()
	s.pk.page = &page{title: title, seq: s.cmdSeq, lines: lines, confirm: confirm, confirmLabel: label, answer: true, then: then, opened: time.Now()}
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
