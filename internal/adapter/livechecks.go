package adapter

import (
	"fmt"
	"slices"
	"strings"
)

// liveCheck is what a maintainer's live check recorded in docs/verification.md
// shows about a CLI's composer, at the versions it ran on. Each fact holds only
// at those versions: another version keeps aggy's safe side until it is
// checked again (ComposerCheckOwed).
type liveCheck struct {
	versions []string
	// clear: Ctrl+U as its byte, 0x15, empties the composer, a slash menu open
	// or not, which aggy types in place of a typed command's Enter (docs/design.md
	// section 5). Its CSI u form is checked on no CLI, so a switch cleared that
	// way still asks (the supervisor's pendingSwitch.cleared).
	clear bool
	// exit: /exit typed into the empty composer ends the CLI with status 0.
	exit bool
	// emptyKeys are keys that, pressed on an empty composer, leave it empty and
	// change nothing a command typed next would see (EmptyComposerKey's names).
	emptyKeys []string
	// quietIdle: a running turn redraws the CLI's screen continuously (its
	// spinner), so output that stays quiet after a blocked prompt means the
	// block started no turn (QuietMeansIdle).
	quietIdle bool
}

// liveChecks are the recorded checks, per CLI.
//
// Claude Code 2.1.322, live 2026-11-11, a bare pseudo-terminal, no model
// request (docs/verification.md, "Claude Code's Ctrl+U and /exit"):
// - Ctrl+U's byte empties the composer, with text typed one key at a time,
// and with /switch or /switch grok typed while the slash menu was open.
// - /exit typed into the empty composer exits 0.
// - Down on the empty composer, then /switch and Enter: the prompt hook
// reported exactly /switch. The same after Backspace, Right, Home, End,
// Delete (CSI 3~), or Ctrl+U (one session; between keys zz was typed and
// cleared with Ctrl+U, and an Esc was pressed).
// - Left on the empty composer is not harmless: it opened another view
// (its footer read "enter to create · esc to clear · ctrl+f to find"), and
// the /switch and Enter typed next went to that view, with no prompt hook.
// The supervisor keeps the line in doubt after it (sideView).
// - a running turn redraws its spinner line continuously (seen in every live
// run of 2026-11-11, a maintainer's observation, not a measured rate).
// - after a UserPromptSubmit block the composer was empty (the supervisor's
// promptBlocked takes it so on every CLI; TODO(T6975) for the others).
//
// Not checked: Ctrl+U as CSI u, a multi-line composer, Ctrl+U with another
// popup open (the @ file menu, history search), during a running turn,
// fullscreen mode, Apple Terminal; Up (it recalls history) and the keys'
// kitty (CSI u) forms on an empty composer.
// TODO(T6895): the same checks for Codex, Grok, OpenCode, and the keys above.
var liveChecks = map[Vendor]liveCheck{
	Claude: {versions: []string{"2.1.322"}, clear: true, exit: true, emptyKeys: []string{"down", "backspace", "right", "home", "end", "delete", "ctrl+u"}, quietIdle: true},
}

// checkAt returns v's recorded check when version is one it ran on.
func checkAt(v Vendor, version string) (liveCheck, bool) {
	c, ok := liveChecks[v]
	if !ok || !slices.Contains(c.versions, version) {
		return liveCheck{}, false
	}
	return c, true
}

// ClearLineChecked reports whether a live check shows Ctrl+U's byte emptying
// v's composer at version.
func ClearLineChecked(v Vendor, version string) bool {
	c, ok := checkAt(v, version)
	return ok && c.clear
}

// EmptyComposerKey reports whether key, pressed on v's empty composer at
// version, leaves it empty and changes nothing for a command typed next. The
// names are down, up, left, right, home, end, backspace, delete, and ctrl+u;
// only those with a recorded check count.
func EmptyComposerKey(v Vendor, version, key string) bool {
	c, ok := checkAt(v, version)
	return ok && slices.Contains(c.emptyKeys, key)
}

// QuietMeansIdle reports whether, on v at version, output that stays quiet
// after aggy blocked a prompt means no turn started there: a live check shows
// a running turn redrawing the screen continuously, and a blocked prompt
// leaving the composer empty.
func QuietMeansIdle(v Vendor, version string) bool {
	c, ok := checkAt(v, version)
	return ok && c.quietIdle
}

// ComposerCheckOwed says, for a version of v outside the ones its live checks
// ran on, that they are owed again, or "" when none is owed.
func ComposerCheckOwed(v Vendor, version string) string {
	c, ok := liveChecks[v]
	if !ok || slices.Contains(c.versions, version) {
		return ""
	}
	if version == "" {
		version = "an unreadable version"
	}
	return fmt.Sprintf("aggy's live checks of the composer (Ctrl+U, /exit, keys on an empty prompt) were recorded on %s only, not %s: a switch asks on the status line until they are checked again (docs/verification.md)",
		strings.Join(c.versions, ", "), version)
}
