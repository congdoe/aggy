package trustlist

import (
	"slices"
	"strings"
	"testing"
)

var (
	claude = Row{ID: "claude", Label: "Claude Code", Group: Harness}
	oc     = Row{ID: "opencode", Label: "OpenCode", Group: Harness, Warning: "plugins run"}
	free   = Row{ID: "opencode-free", Label: "OpenCode free models", Group: Source, Warning: "sent to OpenCode"}
	// sandbox is a setting row (T6980).
	sandbox = Row{ID: "claude-sandbox", Label: "Claude sandboxed", Group: Setting, Quiet: true}
)

// TestSettingRowsLast (T6980): a setting row is listed after the harnesses
// and sources, so their numbers stay as they were, and All leaves it alone.
func TestSettingRowsLast(t *testing.T) {
	s := NewState([]Row{sandbox, free, claude, oc}, Folder{}, false, nil)
	var ids []string
	for _, r := range s.Rows {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []string{"claude", "opencode", "opencode-free", "claude-sandbox"}) {
		t.Fatalf("order %q", ids)
	}
	s.Toggle(1)
	if slices.Contains(s.Ticked(), "claude-sandbox") {
		t.Error("All ticked a setting row")
	}
	got := strings.Join(s.Lines(), "\n")
	if !strings.Contains(got, Setting.Title()) || !strings.Contains(got, " 5 [ ] Claude sandboxed") {
		t.Errorf("list:\n%s", got)
	}
}

func TestPending(t *testing.T) {
	cases := []struct {
		name string
		rows []Row
		f    Folder
		want []string
	}{
		{"first start", []Row{claude, oc}, Folder{}, []string{"claude", "opencode"}},
		{"answered", []Row{claude, oc}, Folder{Shown: []string{"claude", "opencode"}}, nil},
		{"a new row", []Row{claude, oc, free}, Folder{Shown: []string{"claude", "opencode"}, On: []string{"claude"}}, []string{"opencode-free"}},
		{"never", []Row{claude, oc, free}, Folder{Never: true}, nil},
		{"a row gone is no question", []Row{claude}, Folder{Shown: []string{"claude", "opencode"}}, nil},
		// T6980: a quiet row is asked at a first start, not after an answer.
		{"a quiet row at a first start", []Row{claude, sandbox}, Folder{}, []string{"claude", "claude-sandbox"}},
		{"a quiet row, answered before it", []Row{claude, sandbox}, Folder{Shown: []string{"claude"}, On: []string{"claude"}}, nil},
		{"a quiet row beside a new one", []Row{claude, oc, sandbox}, Folder{Shown: []string{"claude"}}, []string{"opencode"}},
		{"a quiet row answered", []Row{claude, sandbox}, Folder{Shown: []string{"claude", "claude-sandbox"}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Pending(c.rows, c.f); !slices.Equal(got, c.want) {
				t.Errorf("Pending = %q, want %q", got, c.want)
			}
		})
	}
}

// TestAllTicksHarnessesOnly: All ticks the harness rows and never a source,
// which is always its own tick; pressed again, it unticks them.
func TestAllTicksHarnessesOnly(t *testing.T) {
	s := NewState([]Row{free, oc, claude}, Folder{}, false, nil)
	if got := []string{s.Rows[0].ID, s.Rows[1].ID, s.Rows[2].ID}; !slices.Equal(got, []string{"opencode", "claude", "opencode-free"}) {
		t.Fatalf("order %q: harnesses first, each group in the order given", got)
	}
	s.Toggle(1)
	if got := s.Ticked(); !slices.Equal(got, []string{"opencode", "claude"}) || !s.AllOn() {
		t.Errorf("All ticked %q", got)
	}
	s.Toggle(4)
	s.Toggle(1)
	if got := s.Ticked(); !slices.Equal(got, []string{"opencode-free"}) {
		t.Errorf("All again left %q", got)
	}
	if AllTicksSources {
		t.Error("All must not tick a source: the owner's default is its own tick")
	}
}

// TestNewRowKeepsAnswers: a row added since the last answer is marked new,
// earlier ticks and unticks are kept, and current fills only a row never
// shown.
func TestNewRowKeepsAnswers(t *testing.T) {
	prev := Folder{Shown: []string{"claude", "opencode"}, On: []string{"opencode"}}
	s := NewState([]Row{claude, oc, free}, prev, true, func(string) bool { return true })
	on := map[string]bool{}
	isNew := map[string]bool{}
	for i, r := range s.Rows {
		on[r.ID], isNew[r.ID] = s.On[i], s.New[i]
	}
	if on["claude"] || !on["opencode"] {
		t.Errorf("earlier answers not kept: %v", on)
	}
	if !on["opencode-free"] || !isNew["opencode-free"] || isNew["claude"] || isNew["opencode"] {
		t.Errorf("new %v, on %v", isNew, on)
	}
	if !strings.Contains(strings.Join(s.Lines(), "\n"), "OpenCode free models   new") {
		t.Errorf("the new row is not marked:\n%s", strings.Join(s.Lines(), "\n"))
	}
	// A first list marks nothing new.
	first := NewState([]Row{claude, free}, Folder{}, false, nil)
	if slices.Contains(first.New, true) {
		t.Error("a first list marked a row new")
	}
}

func TestAnswer(t *testing.T) {
	s := NewState([]Row{claude, free}, Folder{}, false, nil)
	s.Toggle(2)
	a := s.Answer(Folder{}, false)
	if !slices.Equal(a.Shown, []string{"claude", "opencode-free"}) || !slices.Equal(a.On, []string{"claude"}) || a.Never {
		t.Errorf("answer %+v", a)
	}
	// Never keeps the earlier ticks, and records every row as shown.
	prev := Folder{Shown: []string{"claude"}, On: []string{"claude"}}
	n := NewState([]Row{claude, free}, prev, true, nil).Answer(prev, true)
	if !n.Never || !slices.Equal(n.On, []string{"claude"}) || len(n.Shown) != 2 {
		t.Errorf("never %+v", n)
	}
	// A row shown before and not offered now stays answered.
	prev = Folder{Shown: []string{"claude", "opencode"}, On: []string{"opencode"}}
	k := NewState([]Row{claude}, prev, true, nil).Answer(prev, false)
	if !slices.Contains(k.Shown, "opencode") || !slices.Contains(k.On, "opencode") {
		t.Errorf("a row not offered now lost its answer: %+v", k)
	}
	if e := NewState(nil, Folder{}, false, nil).Answer(Folder{}, false); e.On == nil {
		t.Error("an empty answer must record an empty list, not null")
	}
}

// TestRead: how a typed line answers the list. A paste's marker, or any
// control character, picks nothing; a number off the list picks nothing.
func TestRead(t *testing.T) {
	cases := []struct {
		line string
		want Choice
		on   []string
	}{
		{"", Confirm, nil},
		{"\r", Confirm, nil},
		{"n", NotNow, nil},
		{"Not now", NotNow, nil},
		{"x", Never, nil},
		{"never", Never, nil},
		{"2", Toggled, []string{"claude"}},
		{"2, 3", Toggled, []string{"claude", "opencode-free"}},
		{"1", Toggled, []string{"claude"}},
		{"9", Nothing, nil},
		{"0", Nothing, nil},
		{"2 9", Nothing, nil},
		{"yes", Nothing, nil},
		{"\x1b[200~2\x1b[201~", Nothing, nil},
		{"2\x03", Nothing, nil},
	}
	for _, c := range cases {
		s := NewState([]Row{claude, free}, Folder{}, false, nil)
		if got := s.Read(c.line); got != c.want || !slices.Equal(s.Ticked(), c.on) {
			t.Errorf("Read(%q) = %d, ticked %q; want %d, %q", c.line, got, s.Ticked(), c.want, c.on)
		}
	}
}

func TestLines(t *testing.T) {
	s := NewState([]Row{claude, oc, free, {ID: "x", Label: "X", Group: Harness, Missing: true}}, Folder{}, false, nil)
	s.Toggle(2)
	got := strings.Join(s.Lines(), "\n")
	for _, w := range []string{" 1 [ ] All harnesses", Harness.Title(), Source.Title(), " 2 [x] Claude Code", "! plugins run", "! sent to OpenCode", "X (not installed)"} {
		if !strings.Contains(got, w) {
			t.Errorf("list lacks %q:\n%s", w, got)
		}
	}
	if strings.Index(got, Harness.Title()) > strings.Index(got, Source.Title()) {
		t.Error("harnesses must come first")
	}
}
