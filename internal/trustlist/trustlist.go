// Package trustlist is the one trust question aggy asks the first time it
// starts on screen in a folder (T6977, docs/design.md section 10, "Trust"): a
// list of what may run in the folder, where its code may be sent, and how a
// harness runs there (T6980: Claude Code's own sandbox), which the
// person ticks once. It holds the list's rows as shown, its state while it is
// answered, how a typed line answers it, and what aggy records of the answer
// per folder, in aggy's own trust file. It decides nothing about what ticking a
// row changes: each row registers that itself, in internal/cli, so a new row
// never means editing the list.
package trustlist

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// Group is where a row is listed.
type Group int

// The groups, in the order the list shows them.
const (
	// Harness rows are CLIs that may run in the folder.
	Harness Group = iota + 1
	// Source rows are other places the folder's code may be sent.
	Source
	// Setting rows say how a harness runs in the folder (T6980: Claude
	// Code's own sandbox). Listed last, so the earlier rows keep their
	// numbers.
	Setting
)

// groups are the groups in the order the list shows them.
var groups = []Group{Harness, Source, Setting}

// Title is a group's heading.
func (g Group) Title() string {
	switch g {
	case Harness:
		return "Harnesses that may run here"
	case Setting:
		return "How a harness runs here"
	}
	return "Other places this folder's code may be sent"
}

// AllTicksSources says whether "All" ticks the source rows too. It ticks the
// harness rows only: a source is always its own tick, since it sends the
// folder's code somewhere new. That is the orchestrator's default (2026-11-10);
// the owner has not confirmed it.
const AllTicksSources = false

// Row is one row as shown.
type Row struct {
	// ID is the row's stable name in aggy's record, such as "claude".
	ID string
	// Label is what the list shows.
	Label string
	// Group is where it is listed.
	Group Group
	// Warning, when set, is shown under the row: what ticking it risks.
	Warning string
	// Missing, when set, says the row's CLI is not installed.
	Missing bool
	// Quiet rows are not asked about at a start in a folder answered before
	// they were offered (Pending): they count as off there until aggy trust
	// or /aggy trust shows the list (T6980). A row that only changes how a
	// harness runs is not worth asking everyone who answered again for.
	Quiet bool
	// NotInAll rows are never ticked by "All", which leaves them as they are
	// (T6957): ticking one accepts a caveat its Warning says, which takes a
	// tick of its own. The list says so under the row.
	NotInAll bool
}

// inAll reports whether "All" ticks r.
func (r Row) inAll() bool { return !r.NotInAll && (r.Group == Harness || AllTicksSources) }

// Folder is what aggy records of one folder's answer.
type Folder struct {
	// Shown are the rows the list showed when it was last answered, so a row
	// added since is known to be new without a guess.
	Shown []string `json:"shown"`
	// On are the rows ticked then.
	On []string `json:"on"`
	// Never: the person said never for this folder, so no start asks again.
	Never bool `json:"never,omitempty"`
	// Later are ticked rows whose change waits for the next start here,
	// before the CLI starts: an answer given inside a running session leaves
	// Claude's own file alone while that Claude may be writing it.
	Later []string `json:"later,omitempty"`
}

// File is aggy's trust file (~/.aggy/trust.json): the repos trusted for aggy's
// rules (aggy trust <dir>), and the trust list's answers by folder.
type File struct {
	Repos   []string          `json:"repos"`
	Folders map[string]Folder `json:"folders,omitempty"`
}

// Pending reports the rows of avail a start must ask about for f: those its
// last answer did not show, but for a Quiet row in a folder answered before.
// A folder answered never has none.
func Pending(avail []Row, f Folder) []string {
	if f.Never {
		return nil
	}
	var out []string
	for _, r := range avail {
		if r.Quiet && len(f.Shown) > 0 {
			continue
		}
		if !slices.Contains(f.Shown, r.ID) {
			out = append(out, r.ID)
		}
	}
	return out
}

// State is a list being answered: its rows in the order shown, which are
// ticked, and which are new since the folder was last answered.
type State struct {
	Rows []Row
	On   []bool
	New  []bool
}

// NewState starts the list for rows (any order: it is shown by group, each in
// the order given). f is the folder's record and answered whether it has one;
// current says whether a row's change is in place already, for a row the
// record has not shown. Earlier answers are kept, and a row the last answer
// did not show is marked new.
func NewState(rows []Row, f Folder, answered bool, current func(id string) bool) *State {
	s := &State{}
	for _, g := range groups {
		for _, r := range rows {
			if r.Group != g {
				continue
			}
			shown := slices.Contains(f.Shown, r.ID)
			on := slices.Contains(f.On, r.ID)
			if !shown && current != nil {
				on = current(r.ID)
			}
			s.Rows = append(s.Rows, r)
			s.On = append(s.On, on)
			s.New = append(s.New, answered && !shown)
		}
	}
	return s
}

// Toggle ticks or unticks item n, as numbered on the list: 1 is All, then each
// row in order. It reports whether n is on the list.
func (s *State) Toggle(n int) bool {
	switch {
	case n == 1:
		s.All()
	case n >= 2 && n-2 < len(s.Rows):
		s.On[n-2] = !s.On[n-2]
	default:
		return false
	}
	return true
}

// All ticks every row All covers, or unticks them all when they are ticked.
func (s *State) All() {
	every := true
	for i, r := range s.Rows {
		if r.inAll() && !s.On[i] {
			every = false
		}
	}
	for i, r := range s.Rows {
		if r.inAll() {
			s.On[i] = !every
		}
	}
}

// AllOn reports whether every row All covers is ticked.
func (s *State) AllOn() bool {
	n := 0
	for i, r := range s.Rows {
		if r.inAll() {
			n++
			if !s.On[i] {
				return false
			}
		}
	}
	return n > 0
}

// Ticked are the ids of the rows ticked.
func (s *State) Ticked() []string {
	var out []string
	for i, r := range s.Rows {
		if s.On[i] {
			out = append(out, r.ID)
		}
	}
	return out
}

// Answer is the folder's record for the list as ticked now; never, for a
// never, keeps prev's ticks.
func (s *State) Answer(prev Folder, never bool) Folder {
	f := Folder{On: s.Ticked(), Never: never}
	if never {
		f.On = slices.Clone(prev.On)
	}
	for _, r := range s.Rows {
		f.Shown = append(f.Shown, r.ID)
	}
	for _, id := range prev.Shown {
		// A row shown before and not offered now (its CLI removed) stays
		// answered.
		if !slices.Contains(f.Shown, id) {
			f.Shown = append(f.Shown, id)
			if !never && slices.Contains(prev.On, id) {
				f.On = append(f.On, id)
			}
		}
	}
	if f.On == nil {
		f.On = []string{}
	}
	return f
}

// Choice is what a typed line does.
type Choice int

// The choices.
const (
	// Nothing: the line is ignored (a paste, or not a choice).
	Nothing Choice = iota
	// Toggled: the line ticked or unticked items.
	Toggled
	// Confirm: an empty line confirms what is ticked.
	Confirm
	// NotNow: ask again at the next start; nothing is recorded.
	NotNow
	// Never: never ask for this folder.
	Never
)

// Keys says how a typed line answers the list.
const Keys = "Type numbers to tick or untick (1 is All), then Enter on an empty line to confirm; n: not now (asked next start); x: never for this folder."

// Read takes one typed line: numbers (spaces or commas between) toggle, an
// empty line confirms, n is not now, and x (or never) is never. A line with a
// control character, an escape among them (a bracketed paste's marker),
// does nothing, and neither does a number not on the list.
func (s *State) Read(line string) Choice {
	line = strings.TrimRight(line, "\r\n")
	for _, c := range line {
		if c < 0x20 && c != '\t' || c == 0x7f {
			return Nothing
		}
	}
	switch a := strings.ToLower(strings.TrimSpace(line)); a {
	case "":
		return Confirm
	case "n", "no", "not now":
		return NotNow
	case "x", "never":
		return Never
	}
	fields := strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' })
	var ns []int
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > len(s.Rows)+1 {
			return Nothing
		}
		ns = append(ns, n)
	}
	for _, n := range ns {
		s.Toggle(n)
	}
	return Toggled
}

// Lines are the list as shown: All, then each group's rows with their
// numbers, ticks, new marks, and warnings.
func (s *State) Lines() []string {
	box := func(on bool) string {
		if on {
			return "[x]"
		}
		return "[ ]"
	}
	out := []string{fmt.Sprintf(" 1 %s All harnesses", box(s.AllOn()))}
	var g Group
	for i, r := range s.Rows {
		if r.Group != g {
			g = r.Group
			out = append(out, "", "   "+g.Title())
		}
		label := r.Label
		if r.Missing {
			label += " (not installed)"
		}
		if s.New[i] {
			label += "   new"
		}
		out = append(out, fmt.Sprintf("%2d %s %s", i+2, box(s.On[i]), label))
		if r.Warning != "" {
			out = append(out, "        ! "+r.Warning)
		}
		if r.NotInAll {
			out = append(out, "        (All does not tick this one: tick it on its own)")
		}
	}
	return out
}

// Write writes the list to w, one line each.
func (s *State) Write(w io.Writer) {
	for _, l := range s.Lines() {
		fmt.Fprintln(w, l)
	}
}
