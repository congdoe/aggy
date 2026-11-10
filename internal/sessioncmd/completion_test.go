package sessioncmd

import (
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestAggyAliases (T6978): /aggy switch, borrow, and inbox are the
// stand-alone commands, word for word, and /aggy allow is keyboard only.
func TestAggyAliases(t *testing.T) {
	cases := []struct {
		on     adapter.Vendor
		line   string
		ok     bool
		kind   Kind
		name   string
		vendor adapter.Vendor
		args   []string
		line2  string // Fill's line
		kbd    bool
	}{
		{adapter.Claude, "/aggy switch", true, Picker, "aggy switch", "", nil, "", false},
		{adapter.Claude, "/aggy switch grok --brief", true, Switch, "aggy switch", adapter.Grok, nil, "", false},
		{adapter.Codex, "$aggy switch claude", true, Switch, "aggy switch", adapter.Claude, nil, "", false},
		{adapter.Claude, "/aggy switch nobody", false, 0, "", "", nil, "", false},
		{adapter.Claude, "/aggy borrow", true, BorrowList, "aggy borrow", "", nil, "", false},
		{adapter.Claude, "/aggy borrow research what changed", false, 0, "", "", nil, "", false},
		{adapter.Claude, `/aggy borrow research "x"`, false, 0, "", "", nil, "", false},
		{adapter.Claude, "/aggy inbox", true, Run, "aggy inbox", "", []string{"inbox"}, "", false},
		{adapter.Claude, "/aggy inbox --all", true, Run, "aggy inbox", "", []string{"inbox", "--all"}, "", false},
		{adapter.Claude, `/aggy allow "Bash(npm test)"`, true, Run, "aggy allow", "", []string{"permissions", "add", "allow", "Bash(npm test)"}, "", true},
		{adapter.Claude, "/aggy allow Bash(git status)", true, Run, "aggy allow", "", []string{"permissions", "add", "allow", "Bash(git status)"}, "", true},
		{adapter.Claude, "/aggy allow", true, Run, "aggy allow", "", []string{"permissions", "--help"}, "", true},
		{adapter.Claude, `/aggy allow "a"b"`, false, 0, "", "", nil, "", false},
		{adapter.Claude, `/aggy allow ""`, false, 0, "", "", nil, "", false},
		{adapter.Claude, "/aggy allowx", false, 0, "", "", nil, "", false},
	}
	for _, c := range cases {
		got, ok := Parse(c.on, c.line)
		if ok != c.ok {
			t.Errorf("Parse(%s, %q) ok = %v, want %v", c.on, c.line, ok, c.ok)
			continue
		}
		if ok && (got.Kind != c.kind || got.Name != c.name || got.Vendor != c.vendor || !slices.Equal(got.Args, c.args) || got.Line != c.line2 || got.KeyboardOnly != c.kbd) {
			t.Errorf("Parse(%s, %q) = %+v", c.on, c.line, got)
		}
	}
	// /aggy allow asks first: its page shows aggy permissions add's own
	// question answered no, and y answers it yes.
	c, _ := Parse(adapter.Claude, `/aggy allow "Bash(npm test)"`)
	if !slices.Equal(c.Confirm, c.Args) || !c.Answer || c.ConfirmIf != Asked {
		t.Errorf("allow's y step: %+v", c)
	}
}

// TestPrefixes (T6978): a word's start names the one command it starts;
// one that starts more, alone, opens the list filtered to it; a command that
// changes setup or handles an edit borrow never runs from a prefix, only
// opens the list; a prefix of nothing, or with words its command does not
// take, is the helper's question.
func TestPrefixes(t *testing.T) {
	cases := []struct {
		on     adapter.Vendor
		line   string
		ok     bool
		kind   Kind
		vendor adapter.Vendor
		list   string // Pick's list and prefix
		prefix string
		fill   string // Fill's line
	}{
		// /aggy <prefix>
		{adapter.Claude, "/aggy sw", true, Picker, "", "", "", ""},
		{adapter.Claude, "/aggy sw gr", true, Switch, adapter.Grok, "", "", ""},
		{adapter.Claude, "/aggy us", true, Run, "", "", "", ""},
		{adapter.Claude, "/aggy i", true, Run, "", "", "", ""},
		{adapter.Claude, "/aggy s", true, Pick, "", "aggy", "s", ""},
		{adapter.Claude, "/aggy sess", true, Pick, "", "aggy", "sess", ""},
		{adapter.Claude, "/aggy session", true, Run, "", "", "", ""}, // whole names win
		{adapter.Claude, "/aggy b", true, Pick, "", "aggy", "b", ""},
		{adapter.Claude, "/aggy bo", true, BorrowList, "", "", "", ""},
		{adapter.Claude, "/aggy bo re", true, Pick, "", "borrow", "re", ""},
		{adapter.Claude, "/aggy bo rese", true, Fill, "", "", "", "/aggy borrow research "},
		{adapter.Claude, "/aggy s x", false, 0, "", "", "", ""},
		{adapter.Claude, "/aggy use the codex", false, 0, "", "", "", ""},
		{adapter.Claude, "/aggy how", false, 0, "", "", "", ""},
		{adapter.Claude, "/aggy zzz", false, 0, "", "", "", ""},
		// Setup and edit borrows: the list, never the command.
		{adapter.Claude, "/aggy acc", true, Pick, "", "aggy", "acc", ""},
		{adapter.Claude, "/aggy acc 9f2", true, Pick, "", "aggy", "acc", ""},
		{adapter.Claude, "/aggy rej 9f2", true, Pick, "", "aggy", "rej", ""},
		{adapter.Claude, "/aggy tr", true, Pick, "", "aggy", "tr", ""},
		{adapter.Claude, "/aggy al", true, Pick, "", "aggy", "al", ""},
		{adapter.Claude, "/aggy to", true, Pick, "", "aggy", "to", ""},
		{adapter.Claude, "/aggy to on rtk", true, Pick, "", "aggy", "to", ""},
		{adapter.Claude, "/aggy p", true, Pick, "", "aggy", "p", ""},
		{adapter.Claude, "/aggy perm list", true, Pick, "", "aggy", "perm", ""},
		{adapter.Claude, "/aggy t", true, Pick, "", "aggy", "t", ""},
		{adapter.Claude, "/aggy a", true, Pick, "", "aggy", "a", ""},
		{adapter.Claude, "/aggy al npm", false, 0, "", "", "", ""},
		{adapter.Claude, "/aggy permission denied", false, 0, "", "", "", ""},
		// /switch <vendor prefix>
		{adapter.Claude, "/switch gr", true, Switch, adapter.Grok, "", "", ""},
		{adapter.Claude, "/switch o --brief", true, Switch, adapter.OpenCode, "", "", ""},
		{adapter.Claude, "/switch c", true, Pick, "", "switch", "c", ""},
		{adapter.Codex, "$switch c", true, Pick, "", "switch", "c", ""},
		{adapter.Claude, "/switch c --brief", false, 0, "", "", "", ""},
		{adapter.Claude, "/switch x", false, 0, "", "", "", ""},
		// /borrow <kind prefix>
		{adapter.Claude, "/borrow i", true, Fill, "", "", "", "/borrow image "},
		{adapter.Codex, "$borrow ed", true, Fill, "", "", "", "$borrow edit "},
		{adapter.Claude, "/borrow research", true, Fill, "", "", "", "/borrow research "},
		{adapter.Claude, "/borrow re", true, Pick, "", "borrow", "re", ""},
		{adapter.Claude, "/borrow fix-it", false, 0, "", "", "", ""},
		{adapter.Claude, "/borrow re something", false, 0, "", "", "", ""},
	}
	for _, c := range cases {
		got, ok := Parse(c.on, c.line)
		if ok != c.ok {
			t.Errorf("Parse(%s, %q) ok = %v, want %v (%+v)", c.on, c.line, ok, c.ok, got)
			continue
		}
		if ok && (got.Kind != c.kind || got.Vendor != c.vendor || got.List != c.list || got.Prefix != c.prefix || got.Line != c.fill) {
			t.Errorf("Parse(%s, %q) = %+v", c.on, c.line, got)
		}
	}
}

// TestSetupNeverFromPrefix: no prefix of a command that changes setup or
// handles an edit borrow runs it, whatever follows; only its whole name, or a
// pick from the list.
func TestSetupNeverFromPrefix(t *testing.T) {
	for _, s := range Subcommands {
		if !s.Setup {
			continue
		}
		for n := 1; n < len(s.Name); n++ {
			for _, rest := range []string{"", " 9f2", " on rtk", " list", ` "Bash(x)"`} {
				line := "/aggy " + s.Name[:n] + rest
				if c, ok := Parse(adapter.Claude, line); ok && c.Kind != Pick {
					t.Errorf("Parse(%q) = %+v: a prefix ran a setup command", line, c)
				}
			}
		}
	}
	want := []string{"accept", "reject", "tool", "permissions", "allow", "trust"}
	for _, n := range want {
		if s, ok := subcommand(n); !ok || !s.Setup {
			t.Errorf("%s must be a setup command", n)
		}
	}
}

// TestListOrder: /aggy's list shows switch, borrow, and inbox first.
func TestListOrder(t *testing.T) {
	var got []string
	for _, s := range Subcommands[:3] {
		got = append(got, s.Name)
	}
	if !slices.Equal(got, []string{"switch", "borrow", "inbox"}) {
		t.Errorf("first three %q", got)
	}
}
