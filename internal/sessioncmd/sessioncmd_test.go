package sessioncmd

import (
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

func TestParse(t *testing.T) {
	long := "/switch grok --model " + strings.Repeat("m", 129)
	cases := []struct {
		on   adapter.Vendor
		line string
		ok   bool
		kind Kind
		name string
		args []string
		kbd  bool
	}{
		{adapter.Claude, "/switch", true, Picker, "switch", nil, false},
		{adapter.Claude, "/switch  ", true, Picker, "switch", nil, false},
		{adapter.Claude, "/switch grok", true, Switch, "switch", nil, false},
		{adapter.Claude, "/switch grok --brief --new --model grok-4", true, Switch, "switch", nil, false},
		{adapter.Claude, "/switch --model=grok-4 grok", true, Switch, "switch", nil, false},
		{adapter.Claude, "/switch grok codex", false, 0, "", nil, false},
		{adapter.Claude, "/switch nobody", false, 0, "", nil, false},
		{adapter.Claude, "/switch --brief", false, 0, "", nil, false},
		{adapter.Claude, "/switch grok --brief --brief", false, 0, "", nil, false},
		{adapter.Claude, "/switch grok --model", false, 0, "", nil, false},
		{adapter.Claude, long, false, 0, "", nil, false},
		{adapter.Claude, "$switch", false, 0, "", nil, false}, // $ is Codex's
		{adapter.Codex, "$switch", true, Picker, "switch", nil, false},
		{adapter.Codex, "/switch grok", true, Switch, "switch", nil, false},
		{adapter.Claude, "/ switch", false, 0, "", nil, false},
		{adapter.Claude, " /switch", false, 0, "", nil, false},
		{adapter.Claude, "/switchy", false, 0, "", nil, false},
		{adapter.Claude, "/switch grök", false, 0, "", nil, false},
		{adapter.Claude, "/switch grok\t", false, 0, "", nil, false},
		{adapter.Claude, "/borrow", true, BorrowList, "borrow", nil, false},
		{adapter.Claude, "/borrow research what changed", false, 0, "", nil, false},
		{adapter.Claude, `/borrow research "x"`, false, 0, "", nil, false},
		{adapter.Claude, "/inbox", true, Run, "inbox", []string{"inbox"}, false},
		{adapter.Claude, "/inbox 01JX", true, Run, "inbox", []string{"inbox", "01JX"}, false},
		{adapter.Claude, "/inbox --all", true, Run, "inbox", []string{"inbox", "--all"}, false},
		{adapter.Claude, "/inbox a b", false, 0, "", nil, false},
		{adapter.Claude, "/aggy", true, AggyList, "aggy", nil, false},
		{adapter.Claude, "/aggy usage", true, Run, "aggy usage", []string{"usage"}, false},
		{adapter.Claude, "/aggy usage please", false, 0, "", nil, false},
		{adapter.Claude, "/aggy why did my switch not go", false, 0, "", nil, false},
		{adapter.Claude, "/aggy diag", true, Run, "aggy diag", []string{"diag", "-n", "50"}, false},
		{adapter.Claude, "/aggy diag 01JXABC", true, Run, "aggy diag", []string{"diag", "show", "01JXABC"}, false},
		{adapter.Claude, "/aggy diag --json", false, 0, "", nil, false},
		{adapter.Claude, "/aggy accept", true, Run, "aggy accept", []string{"accept", "--help"}, false},
		{adapter.Claude, "/aggy accept 9f2", true, Run, "aggy accept", []string{"accept", "9f2"}, true},
		{adapter.Claude, "/aggy reject 9f2", true, Run, "aggy reject", nil, true},
		{adapter.Claude, "/aggy check 9f2", true, Run, "aggy check", []string{"check", "9f2"}, true},
		{adapter.Claude, "/aggy tool", true, ToolList, "aggy tool", nil, true},
		{adapter.Claude, "/aggy tool on rtk", true, Run, "aggy tool", []string{"tool", "on", "rtk"}, true},
		{adapter.Claude, "/aggy tool on rtk --proxy", false, 0, "", nil, false},
		{adapter.Claude, "/aggy tool on RTK", false, 0, "", nil, false},
		{adapter.Claude, "/aggy permissions", true, Run, "aggy permissions", []string{"permissions", "list"}, true},
		{adapter.Claude, "/aggy permissions add x", false, 0, "", nil, false},
		{adapter.Claude, "/aggy trust", true, TrustList, "aggy trust", nil, true},
		{adapter.Claude, "/aggy trust --opencode", true, Run, "aggy trust", []string{"trust", "--opencode", "."}, true},
		{adapter.Claude, "/aggy unwire", false, 0, "", nil, false},
		{adapter.Grok, "/aggy sessions", true, Run, "aggy sessions", []string{"sessions"}, false},
		{adapter.Grok, "/sessions", false, 0, "", nil, false},
		{adapter.Claude, "/usage", false, 0, "", nil, false},
		{adapter.Codex, "$aggy stats", true, Run, "aggy stats", []string{"stats"}, false},
		{adapter.OpenCode, "/switch claude", true, Switch, "switch", nil, false},
		{adapter.Claude, "/", false, 0, "", nil, false},
		{adapter.Claude, "", false, 0, "", nil, false},
	}
	for _, c := range cases {
		got, ok := Parse(c.on, c.line)
		if ok != c.ok {
			t.Errorf("Parse(%s, %q) ok = %v, want %v", c.on, c.line, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Kind != c.kind || got.Name != c.name || !slices.Equal(got.Args, c.args) || got.KeyboardOnly != c.kbd {
			t.Errorf("Parse(%s, %q) = %+v, want kind %d name %q args %q keyboard %v", c.on, c.line, got, c.kind, c.name, c.args, c.kbd)
		}
	}
}

func TestParseSwitchOptions(t *testing.T) {
	c, ok := Parse(adapter.Claude, "/switch grok --brief --new --model grok-4")
	if !ok || c.Vendor != adapter.Grok || !c.Brief || !c.New || c.Model != "grok-4" {
		t.Fatalf("got %+v, %v", c, ok)
	}
}

func TestConfirmSteps(t *testing.T) {
	cases := []struct {
		line         string
		confirm      []string
		answer, text bool
		args         []string
	}{
		{"/aggy accept 9f2", []string{"accept", "9f2", "--yes"}, false, false, []string{"accept", "9f2"}},
		{"/aggy check 9f2", []string{"check", "9f2", "--yes"}, false, false, []string{"check", "9f2"}},
		{"/aggy reject 9f2", []string{"reject", "9f2"}, false, true, nil},
		{"/aggy trust --opencode", []string{"trust", "--opencode", "."}, true, false, []string{"trust", "--opencode", "."}},
	}
	for _, c := range cases {
		got, ok := Parse(adapter.Claude, c.line)
		if !ok || !slices.Equal(got.Confirm, c.confirm) || got.Answer != c.answer || (got.Text != "") != c.text || !slices.Equal(got.Args, c.args) || got.ConfirmLabel == "" {
			t.Errorf("Parse(%q) = %+v", c.line, got)
		}
	}
}

func TestPrefix(t *testing.T) {
	for _, c := range []struct {
		v    adapter.Vendor
		want string
	}{{adapter.Claude, "/"}, {adapter.Grok, "/"}, {adapter.OpenCode, "/"}, {adapter.Codex, "$"}} {
		if got := Prefix(c.v); got != c.want {
			t.Errorf("Prefix(%s) = %q, want %q", c.v, got, c.want)
		}
	}
}
