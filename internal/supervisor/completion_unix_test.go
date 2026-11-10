//go:build unix

package supervisor

import (
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// labels are the items a menu shows now, by their keys.
func shownKeys(m *menu) []string {
	var out []string
	for _, it := range m.shown() {
		out = append(out, it.key)
	}
	return out
}

// TestAggyForms (T6978): /aggy switch, borrow, and inbox do what the
// stand-alone commands do, and keep working when [commands] turns the
// stand-alone name off.
func TestAggyForms(t *testing.T) {
	t.Run("aggy switch opens the picker", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		s.o.CommandOff = func(name string) bool { return name == "switch" }
		typeLine(s, "/aggy switch")
		if !s.open || s.pk.menu != nil || s.pk.page != nil {
			t.Fatal("the picker did not open")
		}
	})
	t.Run("aggy switch grok queues the switch", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		s.o.CommandOff = func(name string) bool { return name == "switch" }
		typeLine(s, "/aggy switch grok")
		if s.sw.to != adapter.Grok {
			t.Fatalf("switch to %q", s.sw.to)
		}
	})
	t.Run("the stand-alone form off goes to the CLI", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		s.o.CommandOff = func(name string) bool { return name == "switch" }
		typeLine(s, "/switch grok")
		if s.sw.to != "" || s.open {
			t.Fatal("a command turned off was caught")
		}
	})
	t.Run("aggy inbox shows the reports", func(t *testing.T) {
		s, ran := viewSess(t, adapter.Codex, "no reports")
		s.o.CommandOff = func(name string) bool { return name == "inbox" }
		typeLine(s, "$aggy inbox")
		settle(t, s)
		if len(*ran) != 1 || (*ran)[0][0] != "inbox" {
			t.Fatalf("ran %q", *ran)
		}
	})
	t.Run("aggy borrow lists the kinds and fills the aggy form", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		s.o.CommandOff = func(name string) bool { return name == "borrow" }
		typeLine(s, "/aggy borrow")
		if s.pk.menu == nil {
			t.Fatal("no kinds")
		}
		s.viewInput([]byte("3")) // research
		if got := sentTo(s); got != "/aggy borrow\x15/aggy borrow research " {
			t.Fatalf("the CLI got %q", got)
		}
	})
	t.Run("the list shows switch, borrow, inbox first, and runs them under aggy", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		typeLine(s, "/aggy")
		items := s.pk.menu.items
		if len(items) < 3 || items[0].line != "/aggy switch" || items[1].line != "/aggy borrow" || items[2].line != "/aggy inbox" {
			t.Fatalf("first items %+v", items[:3])
		}
	})
}

// TestPrefixLists (T6978): an ambiguous prefix opens the list filtered to it,
// typing narrows it and Backspace widens it, and picking runs or fills in as
// the full list does; a setup command opens its list and runs only on a pick.
func TestPrefixLists(t *testing.T) {
	t.Run("aggy s narrows as typed", func(t *testing.T) {
		s, ran := viewSess(t, adapter.Claude, "ok")
		typeLine(s, "/aggy s")
		m := s.pk.menu
		if m == nil || strings.Join(shownKeys(m), ",") != "switch,sessions,session,stats" {
			t.Fatalf("menu %+v", m)
		}
		s.viewInput([]byte("t"))
		if strings.Join(shownKeys(s.pk.menu), ",") != "stats" {
			t.Fatalf("after t: %q", shownKeys(s.pk.menu))
		}
		s.viewInput([]byte("\x7f"))
		if len(shownKeys(s.pk.menu)) != 4 {
			t.Fatalf("Backspace: %q", shownKeys(s.pk.menu))
		}
		s.viewInput([]byte("ta\r")) // stats
		settle(t, s)
		if len(*ran) != 1 || (*ran)[0][0] != "stats" {
			t.Fatalf("ran %q", *ran)
		}
	})
	t.Run("a setup command from a prefix only lists", func(t *testing.T) {
		s, ran := viewSess(t, adapter.Claude, "Not applied: run again with --yes")
		typeLine(s, "/aggy acc 9f2")
		m := s.pk.menu
		if m == nil || strings.Join(shownKeys(m), ",") != "accept" || len(*ran) != 0 {
			t.Fatalf("menu %+v ran %q", m, *ran)
		}
		s.viewInput([]byte("\r")) // a pick fills /aggy accept for the id
		if got := sentTo(s); !strings.HasSuffix(got, "/aggy accept ") {
			t.Fatalf("the CLI got %q", got)
		}
		if len(*ran) != 0 {
			t.Fatalf("ran %q", *ran)
		}
	})
	t.Run("aggy tr lists trust, and a pick opens it", func(t *testing.T) {
		s, answers := trustSess(t)
		typeLine(s, "/aggy tr")
		if s.pk.menu == nil || strings.Join(shownKeys(s.pk.menu), ",") != "trust" {
			t.Fatalf("menu %+v", s.pk.menu)
		}
		s.viewInput([]byte("\r"))
		settle(t, s)
		if s.pk.menu == nil || s.pk.menu.check == nil || len(*answers) != 0 {
			t.Fatal("picking trust did not open its list")
		}
	})
	t.Run("switch c lists the CLIs to switch to", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Grok, "")
		typeLine(s, "/switch c")
		if s.pk.menu == nil || strings.Join(shownKeys(s.pk.menu), ",") != "codex,claude" {
			t.Fatalf("menu %+v", s.pk.menu)
		}
		s.viewInput([]byte("l\r"))
		if s.sw.to != adapter.Claude {
			t.Fatalf("switch to %q", s.sw.to)
		}
	})
	t.Run("switch gr switches", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		typeLine(s, "/switch gr")
		if s.sw.to != adapter.Grok {
			t.Fatalf("switch to %q", s.sw.to)
		}
	})
	t.Run("borrow re lists two kinds; borrow res fills", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		typeLine(s, "/borrow re")
		if s.pk.menu == nil || strings.Join(shownKeys(s.pk.menu), ",") != "research,review" {
			t.Fatalf("menu %+v", s.pk.menu)
		}
		s.viewInput([]byte("\x1b"))
		s2, _ := viewSess(t, adapter.Claude, "")
		typeLine(s2, "/borrow res")
		if got := sentTo(s2); got != "/borrow res\x15/borrow research " {
			t.Fatalf("the CLI got %q", got)
		}
	})
	t.Run("q j k are letters in a list that narrows", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		typeLine(s, "/aggy")
		s.viewInput([]byte("q"))
		if !s.open || s.pk.menu == nil || s.pk.menu.filter != "q" {
			t.Fatal("q closed a list that narrows")
		}
	})
}

// TestToolNamePrefix (T6978): /aggy tool on|off runs only on a tool's whole
// name; a prefix opens the tools list filtered to it, and a pick runs it.
func TestToolNamePrefix(t *testing.T) {
	s, ran := viewSess(t, adapter.Claude, "ok")
	typeLine(s, "/aggy tool on rt")
	settle(t, s)
	if len(*ran) != 0 || s.pk.menu == nil || strings.Join(shownKeys(s.pk.menu), ",") != "rtk" {
		t.Fatalf("ran %q menu %+v", *ran, s.pk.menu)
	}
	s.viewInput([]byte("\r"))
	settle(t, s)
	if len(*ran) != 1 || strings.Join((*ran)[0], " ") != "tool on rtk false" {
		t.Fatalf("ran %q", *ran)
	}

	s2, ran2 := viewSess(t, adapter.Claude, "ok")
	typeLine(s2, "/aggy tool off rtk")
	settle(t, s2) // the names, read
	settle(t, s2) // the run
	if len(*ran2) != 1 || strings.Join((*ran2)[0], " ") != "tool off rtk false" {
		t.Fatalf("a whole name: ran %q", *ran2)
	}
}

// TestAggyAllowKeyboardOnly (T6978): /aggy allow runs aggy permissions add
// allow on the page, answering its question no until y, and no hook runs it.
func TestAggyAllowKeyboardOnly(t *testing.T) {
	s, ran := viewSess(t, adapter.Claude, "Add the allow rule Bash(npm test) for this repo? [y/N] Nothing changed.")
	if err := s.onPromptCommand(Request{Vendor: "claude", Command: `/aggy allow "Bash(npm test)"`}); err == nil || !strings.Contains(err.Error(), "only typed in full") {
		t.Errorf("a hook's /aggy allow: %v", err)
	}
	typeLine(s, `/aggy allow "Bash(npm test)"`)
	settle(t, s)
	if len(*ran) != 1 || strings.Join((*ran)[0], " ") != "permissions add allow Bash(npm test) false" {
		t.Fatalf("ran %q", *ran)
	}
	s.viewInput([]byte("y"))
	settle(t, s)
	if len(*ran) != 2 || strings.Join((*ran)[1], " ") != "permissions add allow Bash(npm test) true" {
		t.Fatalf("ran %q", *ran)
	}
}
