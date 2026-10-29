//go:build unix

package supervisor

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/diag"
)

// A tool change (T6729): aggy tool on or off, run anywhere, sends every
// running session a tools-changed notice. The notice carries nothing and
// proves nothing; the session reads its own tools again (Options.Tools)
// and, when they differ from its launch's, offers to resume the vendor on
// screen on the same session at the turn end, through the switch path:
// the same waits for the turn end, unsent text, holds, and the user's
// switch key, which alone confirms. A vendor with no hold signal (Codex)
// gets a note instead, as after a limit: the switch key there queues the
// resume, still confirmed at the turn end (S-12).
//
// TODO(T6729): no in-place reload is verified for any of the three CLIs
// (docs/verification.md, "Tool reload"), so every change needs a resume.

// toolReading is one answer of Options.Tools, for the read seq.
type toolReading struct {
	set ToolSet
	err error
	seq int
}

// toolStale is how long a read of the session's tools may run before a
// new notice gives up on it (as shareStale, L5).
var toolStale = 30 * time.Second

// onTools is the socket's tools-changed callback: it wakes the main loop
// and never blocks; notices that come while one waits are one.
func (s *session) onTools() {
	select {
	case s.toolNotes <- struct{}{}:
	default:
	}
}

// readTools reads the session's tools off the main loop. One read runs
// at a time; a notice during it reads again once it lands. A read in
// flight past toolStale (a hung file system) is given up on: the notice
// starts its own, and the old answer is dropped when it lands.
func (s *session) readTools() {
	if s.o.Tools == nil {
		return
	}
	if s.toolBusy && time.Since(s.toolAt) <= toolStale {
		s.toolAgain = true
		return
	}
	s.toolSeq++
	s.toolBusy, s.toolAgain, s.toolRead, s.toolAt = true, false, s.toolSeq, time.Now()
	seq := s.toolSeq
	s.goSafe(func() {
		set, err := s.o.Tools()
		select {
		case s.toolReads <- toolReading{set: set, err: err, seq: seq}:
		case <-s.quit:
		}
	})
}

// toolsRead takes a reading of the session's tools. Nothing happens when
// they are the launch's, or as they were when last offered, which the
// user may have turned down: only a change since asks again.
func (s *session) toolsRead(r toolReading) {
	if r.seq != s.toolRead {
		return // a read given up on as stuck
	}
	s.toolBusy = false
	if s.toolAgain {
		s.toolAgain = false
		s.readTools() // a newer notice came: this reading may be stale
		return
	}
	if r.err != nil {
		// Its text can hold a path: the record keeps the code's text.
		s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeToolFailed, Msg: "the session's tools could not be read again"})
		return
	}
	v := s.o.Vendor
	diff := toolDiff(s.o.ToolsAtLaunch, r.set)
	rec := func(how string) {
		s.log(diag.Rec{Event: diag.EvToolsChanged, Attrs: map[string]any{"changed": len(diff), "how": how}})
	}
	if len(diff) == 0 {
		// Back as launched: a resume or a note still waiting is not
		// needed.
		s.toolSeen, s.toolsDue = nil, false
		if s.open && s.pk.note && s.pk.resume {
			s.closePicker() // L4: the note is stale
		}
		s.pk.resume = false
		if s.sw.resume && !s.sw.typed {
			rec("reverted")
			if s.open && s.pk.confirm != "" {
				s.closePicker()
			}
			s.setSwitch(pendingSwitch{})
		}
		return
	}
	if s.toolSeen != nil && maps.Equal(r.set, s.toolSeen) {
		return
	}
	// seen marks the set acted on, so it is not offered again; only what
	// was offered (or is waiting to be) counts (M3).
	seen := func() {
		s.toolSeen = r.set
		if s.toolSeen == nil {
			s.toolSeen = ToolSet{} // every tool off
		}
	}
	switch {
	case s.sw.typed && s.sw.resume:
		// The exit command is typed: the resume wires the tools as they
		// are then.
		seen()
		rec("waiting")
	case s.sw.to != "" && !s.sw.resume:
		// Another vendor waits. If it goes, that vendor's launch wires
		// its own tools; if it is cancelled, the next notice offers this.
		rec("busy")
	case s.sw.resume:
		// M2: the user must see what the resume brings; an open offer is
		// closed and asked again, with its guard re-armed.
		seen()
		s.sw.tools = diff
		if s.open && s.pk.confirm != "" {
			s.closePicker()
		}
		rec("waiting")
	case !holdSignal(v):
		seen()
		if s.open && s.pk.note && s.pk.resume {
			s.closePicker() // shown again with the new changes
		}
		s.toolsDue, s.toolsNote = true, diff
		rec("note")
	default:
		seen()
		s.queueResume(diff, false)
		rec("queued")
	}
}

// queueResume queues a resume of the vendor on screen, for the tool
// changes diff; user is set when the user asked for it at a note.
func (s *session) queueResume(diff []string, user bool) {
	v := s.o.Vendor
	s.setSwitch(pendingSwitch{to: v, resume: true, tools: diff, choice: Choice{Vendor: v, Resume: true}})
	origin := "tools"
	if user {
		origin = "user"
	}
	s.sw.rec = s.log(diag.Rec{Event: diag.EvSwitchQueued, Attrs: map[string]any{"origin": origin, "to": string(v), "resume": true, "tools": len(diff)}})
}

// offerToolsNote shows the note for a tool change on a vendor with no
// hold signal, once the CLI is at its prompt and quiet, the picker is
// closed, no switch waits, and none was turned down at this turn end. A
// switch in the way drops it, and the next notice offers it again.
func (s *session) offerToolsNote() {
	if !s.toolsDue || s.open {
		return
	}
	if s.sw.to != "" {
		s.toolsDue, s.toolSeen = false, nil
		return
	}
	now := time.Now()
	if !s.atPrompt || s.toolsWaitTurn || now.Sub(s.lastIn) < switchQuiet || now.Sub(time.Unix(0, s.lastOut.Load())) < switchQuiet {
		return
	}
	s.toolsDue = false
	v := s.o.Vendor
	s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeToolsNoHold, Cause: s.lastTurnEnd, Msg: "the tools changed on a vendor with no hold signal; the user starts the resume", Attrs: map[string]any{"to": string(v)}})
	s.pk.tell(fmt.Sprintf("Tools changed for %s: %s (+ on, - off, ~ changed).", v, strings.Join(s.toolsNote, " ")),
		fmt.Sprintf("Press %s to resume %s on the same session with them at the turn end; until then they take effect at its next launch.", keyName(s.pk.key), v),
		fmt.Sprintf("aggy cannot tell when %s waits for an approval, so it does not offer the resume itself.", v))
	s.pk.resume = true
	s.openPicker()
}

// toolDiff names the tools whose part in the session differs between was
// and now, by name: "+name" turned on, "-name" off, "~name" changed.
// Short, so the confirmation's keys stay on an 80-column line.
func toolDiff(was, now ToolSet) []string {
	var out []string
	names := slices.Concat(slices.Collect(maps.Keys(was)), slices.Collect(maps.Keys(now)))
	slices.Sort(names)
	for _, n := range slices.Compact(names) {
		w, inWas := was[n]
		switch v, inNow := now[n]; {
		case !inWas:
			out = append(out, "+"+n)
		case !inNow:
			out = append(out, "-"+n)
		case w != v:
			out = append(out, "~"+n)
		}
	}
	return out
}
