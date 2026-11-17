package adapter

import (
	"fmt"
	"slices"
	"strings"
)

// liveCheck is what a maintainer's live check recorded in docs/verification.md
// shows about a CLI's composer, at the versions it ran on. Each fact holds at
// those versions and at later patch versions of the same major.minor line
// (2.1.x with x at least the first checked patch), until a check at a newer
// version records a difference: these are editor behaviours (kill-line, the
// exit command) that vendors rarely change in a patch release. If one does
// change, the worst case is that Ctrl+U leaves text in the composer and the
// /exit and Enter aggy types after it send that text, with /exit, to the model
// as a prompt: a turn the person did not mean, which they see. An older
// version, another major or minor, or an unreadable version keeps aggy's safe
// side (covers). aggy doctor says, per fact, when the running version is newer
// than the last check of it (ComposerCheckOwed), so a maintainer runs it again.
type liveCheck struct {
	// versions are the versions checks ran on, oldest first, all on one
	// major.minor line.
	versions []string
	// last is, per group of facts, the last version a check of it ran on.
	last []factCheck
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
	// blockedEmpty: after a prompt hook blocked a prompt, the composer was
	// empty (BlockedPromptEmpty).
	blockedEmpty bool
}

// factCheck is a group of facts and the last version a live check of it ran
// on.
type factCheck struct{ facts, version string }

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
// - after a UserPromptSubmit block the composer was empty (the supervisor's
// promptBlocked takes it so on every CLI; TODO(T6975) for the others).
//
// Claude Code 2.1.326, live 2026-11-17, a bare pseudo-terminal, no model
// request: Ctrl+U's byte emptied the composer with /switch grok typed (the
// slash menu open) and with plain text typed, and /exit then exited 0, both as
// at 2.1.322. The other facts were not checked again there; they carry over
// from 2.1.322 as same-line facts.
//
// - a maintainer's run in Apple Terminal, 2026-11-12: SessionStart came 1 s
// after launch, and /switch codex typed in full 5 s later was caught from the
// keys at the prompt (no turn had ended). Before the first turn end aggy cannot
// tell Claude's prompt from a start-up dialog (folder trust, a project's MCP
// servers, an update notice, new terms) from hooks alone: SessionStart came
// 0.4 s after launch in a run of 2026-11-11, before an Enter that likely
// answered the folder-trust dialog. So such a switch asks once on the status
// line (T6982, #147 review); reading the screen is planned to remove that.
//
// Not checked: Ctrl+U as CSI u, a multi-line composer, Ctrl+U with another
// popup open (the @ file menu, history search), during a running turn,
// fullscreen mode, Apple Terminal; Up (it recalls history) and the keys'
// kitty (CSI u) forms on an empty composer.
// TODO(T6895): the same checks for Codex, Grok, OpenCode, and the keys above.
var liveChecks = map[Vendor]liveCheck{
	Claude: {versions: []string{"2.1.322", "2.1.326"}, last: []factCheck{
		{"Ctrl+U and /exit", "2.1.326"},
		{"keys on an empty prompt, Left's other view, the composer after a blocked prompt", "2.1.322"},
	}, clear: true, exit: true, emptyKeys: []string{"down", "backspace", "right", "home", "end", "delete", "ctrl+u"}, blockedEmpty: true},
}

// checkAt returns v's recorded check when it covers version.
func checkAt(v Vendor, version string) (liveCheck, bool) {
	c, ok := liveChecks[v]
	if !ok || !c.covers(version) {
		return liveCheck{}, false
	}
	return c, true
}

// semver parses a plain major.minor.patch version; ok is false for anything
// else (a pre-release suffix, a missing part, text).
func semver(v string) (major, minor, patch int, ok bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var n [3]int
	for i, p := range parts {
		if p == "" || len(p) > 6 {
			return 0, 0, 0, false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return 0, 0, 0, false
			}
			n[i] = n[i]*10 + int(r-'0')
		}
	}
	return n[0], n[1], n[2], true
}

// covers reports whether version is on the checked major.minor line, at or
// after the first checked patch.
func (c liveCheck) covers(version string) bool {
	if len(c.versions) == 0 {
		return false
	}
	ma, mi, pa, ok := semver(version)
	cma, cmi, cpa, cok := semver(c.versions[0])
	return ok && cok && ma == cma && mi == cmi && pa >= cpa
}

// stale returns the groups of facts whose last check ran on a version before
// version, which covers (the same line).
func (c liveCheck) stale(version string) []factCheck {
	_, _, pa, _ := semver(version)
	var out []factCheck
	for _, f := range c.last {
		if _, _, at, _ := semver(f.version); pa > at {
			out = append(out, f)
		}
	}
	return out
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

// BlockedPromptEmpty reports whether a live check shows v's composer empty
// after its prompt hook blocked a prompt, at version: what lets a command the
// person typed, which the hook blocked, switch in one step (the supervisor's
// actTyped), as Ctrl+U's byte does for one caught from the keys.
func BlockedPromptEmpty(v Vendor, version string) bool {
	c, ok := checkAt(v, version)
	return ok && c.blockedEmpty
}

// ComposerCheckOwed says, for aggy doctor, that v's live checks are owed
// again at version: it is outside the line they cover, so a switch asks on
// the status line, or newer than the last check, so they carry over until a
// maintainer runs them again. "" when none is owed.
func ComposerCheckOwed(v Vendor, version string) string {
	c, ok := liveChecks[v]
	if !ok {
		return ""
	}
	if c.covers(version) {
		st := c.stale(version)
		if len(st) == 0 {
			return ""
		}
		var parts []string
		for _, f := range st {
			parts = append(parts, fmt.Sprintf("%s on %s", f.facts, f.version))
		}
		return fmt.Sprintf("%s is newer than the last live checks of the composer (%s); they carry over within the same minor version, and a maintainer should run them again (docs/verification.md)",
			version, strings.Join(parts, "; "))
	}
	if version == "" {
		version = "an unreadable version"
	}
	return fmt.Sprintf("aggy's live checks of the composer (Ctrl+U, /exit, keys on an empty prompt) were recorded on %s and hold for later %s versions, not %s: a switch asks on the status line until they are checked there (docs/verification.md)",
		strings.Join(c.versions, ", "), lineOf(c.versions[0]), version)
}

// lineOf is version's major.minor line, as "2.1.x".
func lineOf(version string) string {
	ma, mi, _, _ := semver(version)
	return fmt.Sprintf("%d.%d.x", ma, mi)
}
