//go:build unix

package supervisor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/sessioncmd"
	"github.com/congdoe/aggy/internal/store"
)

// missRecs returns the miss records' attrs.
func missRecs(t *testing.T, recs []store.DiagRecord) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range recs {
		if r.Event != diag.EvCommandPassed.String() {
			continue
		}
		var a map[string]any
		if err := json.Unmarshal(r.Attrs, &a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// TestCommandMissRecord (T6982): an Enter that ends a command line the key
// catch lets through writes one record naming the command and why, in fixed
// words; a line the tracker lost is recorded with no name when it began with /,
// and nothing of what was typed is stored.
func TestCommandMissRecord(t *testing.T) {
	each := func(s string) []string { return strings.Split(s, "") }
	line := func(keys ...string) []string { return append(keys, "\r") }
	cases := []struct {
		name    string
		v       adapter.Vendor
		setup   func(s *session)
		keys    []string
		want    bool   // a record
		command string // its command, "" for none
		why     []string
		other   map[string]any
	}{
		{name: "caught", v: adapter.Claude, keys: line(each("/switch grok")...)},
		{name: "not a command", v: adapter.Claude, keys: line(each("/clear")...)},
		{name: "plain text with an edit", v: adapter.Claude, keys: line(append(each("hello"), "\x7f")...)},
		{name: "an empty Enter", v: adapter.Claude, keys: []string{"\r"}},
		{name: "a newline", v: adapter.Claude, keys: append(each("/switch grok\\"), "\r")},
		{name: "one read", v: adapter.Claude, keys: []string{"/switch grok\r"}, want: true, command: "switch", why: []string{"enter_not_alone"}},
		{name: "last key with the Enter", v: adapter.Claude, keys: append(each("/switch gro"), "k\r"), want: true, command: "switch", why: []string{"enter_not_alone"}},
		{name: "after a popup's Enter", v: adapter.Claude, keys: append(line(each("@fi")...), line(each("/aggy switch")...)...), want: true, command: "aggy switch",
			why: []string{"len_unknown", "comp_len"}, other: map[string]any{"unsettled": "no_turn_end"}},
		{name: "after two Escs: lost", v: adapter.Claude, keys: append([]string{"\x1b", "\x1b"}, line(each("/switch")...)...), want: true,
			why: []string{"not_plain_keys", "esc_empty", "esc_twice"}},
		{name: "turned off", v: adapter.Claude, setup: func(s *session) { s.o.CommandOff = func(n string) bool { return n == "switch" } },
			keys: line(each("/switch grok")...), want: true, command: "switch", why: []string{"command_off"}},
		{name: "not taken", v: adapter.Claude, keys: line(each("/inbox")...), want: true, command: "inbox", why: []string{"not_taken"}},
		{name: "alternate screen", v: adapter.Claude, setup: func(s *session) { s.alt.on = true }, keys: line(each("/switch grok")...), want: true, command: "switch",
			why: []string{"alt_screen"}},
		{name: "kitty Enter with its release", v: adapter.Claude, keys: append(each("/switch grok"), "\x1b[13u\x1b[13;1:3u"), want: true, command: "switch",
			why: []string{"enter_not_plain"}},
		{name: "an edit: lost, no name", v: adapter.Claude, keys: line(append(each("/switch grox"), "\x7f", "k")...), want: true, why: []string{"not_plain_keys"}},
		{name: "a cursor key first: lost", v: adapter.Claude, keys: line(append([]string{"\x1b[D"}, each("/switch grok")...)...), want: true,
			why: []string{"not_plain_keys", "comp_len", "draft_blind"}, other: map[string]any{"seqs": "csiD"}},
		{name: "an unknown reply first: lost", v: adapter.Claude, keys: line(append([]string{"\x1b[?1;2X"}, each("/switch grok")...)...), want: true,
			why: []string{"not_plain_keys"}, other: map[string]any{"seqs": "csi?X"}},
		{name: "pasted: lost", v: adapter.Claude, keys: []string{"\x1b[200~/switch grok\x1b[201~", "\r"}, want: true, why: []string{"not_plain_keys"}},
		{name: "Codex's $ form, lost", v: adapter.Codex, keys: line(append(each("$switcx"), "\x7f", "h")...), want: true, why: []string{"not_plain_keys"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, records := diagHome(t)
			s := cmdSess(t, c.v)
			s.o.Diag = q
			if c.setup != nil {
				c.setup(s)
			}
			for _, k := range c.keys {
				s.send([]byte(k))
			}
			recs := records()
			got := missRecs(t, recs)
			if !c.want {
				if len(got) != 0 {
					t.Fatalf("records %v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("records %v, want one", got)
			}
			a := got[0]
			if cmd, _ := a["command"].(string); cmd != c.command {
				t.Errorf("command %q, want %q", cmd, c.command)
			}
			why, _ := a["why"].(string)
			for _, w := range c.why {
				if !strings.Contains(","+why+",", ","+w+",") {
					t.Errorf("why %q, want %s in it", why, w)
				}
			}
			for k, v := range c.other {
				if a[k] != v {
					t.Errorf("%s = %v, want %v", k, a[k], v)
				}
			}
			for _, r := range recs {
				if strings.Contains(string(r.Attrs), "grok") || strings.Contains(r.Msg, "grok") {
					t.Errorf("the typed text is in a record: %s %s", r.Attrs, r.Msg)
				}
			}
		})
	}
}

// TestHookMissRecord (T6982): a command a prompt hook reported and refused for
// want of its Enter is recorded with the part of the rule that failed.
func TestHookMissRecord(t *testing.T) {
	q, records := diagHome(t)
	s := cmdSess(t, adapter.Claude)
	s.o.Diag = q
	s.alt.on = true // the key catch lets it through
	for _, k := range append(strings.Split("/switch", ""), "\r", "x") {
		s.send([]byte(k))
	}
	c, _ := sessioncmd.Parse(adapter.Claude, "/switch")
	if err := s.hookCommand(c, "/switch"); err == nil {
		t.Fatal("taken with a key after its Enter")
	}
	got := missRecs(t, records())
	if len(got) != 2 {
		t.Fatalf("records %v, want the key catch's and the hook's", got)
	}
	if why := got[1]["why"]; why != "hook_no_enter,keys_after_enter" || got[1]["command"] != "switch" {
		t.Errorf("hook record %v", got[1])
	}
}

// TestCommandMissBound (T6982): at most missPerWindow records a missWindow;
// the next one after it counts the ones skipped.
func TestCommandMissBound(t *testing.T) {
	q, records := diagHome(t)
	s := cmdSess(t, adapter.Claude)
	s.o.Diag = q
	for range missPerWindow + 3 {
		s.send([]byte("/switch grok\r"))
	}
	s.missAt = time.Now().Add(-missWindow)
	s.send([]byte("/switch grok\r"))
	got := missRecs(t, records())
	if len(got) != missPerWindow+1 {
		t.Fatalf("%d records, want %d", len(got), missPerWindow+1)
	}
	if n, _ := got[missPerWindow]["skipped"].(float64); n != 3 {
		t.Errorf("skipped %v, want 3", got[missPerWindow]["skipped"])
	}
}

// TestCommandMissUnsettled (T6982): a command line missed after a / line whose
// Enter went while aggy took a turn to be running (an Enter on a launch
// dialog), with the turn end after it, says that turn end settled nothing and
// why.
func TestCommandMissUnsettled(t *testing.T) {
	noSettleGap(t)
	q, records := diagHome(t)
	s := cmdSess(t, adapter.Claude)
	s.o.Diag = q
	s.turnRan = false
	// The dialog's Enter, then a / line the CLI ran, and its turn end.
	for _, k := range []string{"\r", "/", "x", "\r"} {
		s.send([]byte(k))
	}
	s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})
	for _, k := range append(strings.Split("/switch grok", ""), "\r") {
		s.send([]byte(k))
	}
	got := missRecs(t, records())
	if len(got) != 1 || got[0]["unsettled"] != "enter_not_at_prompt" || got[0]["command"] != "switch" {
		t.Fatalf("records %v, want one for switch, unsettled enter_not_at_prompt", got)
	}
}
