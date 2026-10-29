//go:build unix

package supervisor

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestSwitchPickedChildGone (T6770): a confirmed switch to a picked
// child checks it is still recorded before the exit command; one gone is
// refused with a note, and one still there switches.
func TestSwitchPickedChildGone(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		var asked atomic.Value
		ts, c, done, _ := switchRunOn(t, false, adapter.Claude, func(o *Options) {
			o.Children = func(v adapter.Vendor) []ChildChoice {
				if v == adapter.Grok {
					return []ChildChoice{{"g2", "latest"}, {"g1", "older"}}
				}
				return nil
			}
			o.HasChild = func(v adapter.Vendor, id string) bool {
				asked.Store(string(v) + "/" + id)
				return recorded
			}
		})
		ts.typeIn(t, "\x1c")
		poll(t, "the list", []byte("aggy: switch vendor"), ts.screen)
		ts.typeIn(t, "2")
		poll(t, "the children", []byte("older"), ts.screen)
		ts.typeIn(t, "2")
		poll(t, "the confirmation", []byte("-> grok. Ctrl+"), ts.screen)
		confirm(t, ts)
		if !recorded {
			poll(t, "the note", []byte("no longer recorded"), ts.screen)
			if got := c.received(); bytes.Contains(got, []byte("/exit")) {
				t.Fatalf("the CLI got %q", got)
			}
			if a, _ := asked.Load().(string); a != "grok/g1" {
				t.Errorf("HasChild asked for %q, want grok/g1", a)
			}
			finish(t, c, done)
			continue
		}
		c.waitGot(t, []byte("/exit\r"))
		c.quit(0)
		wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok, Child: "g1"}})
	}
}

// TestSwitchLongModelKeepsKeys (T6770): a model name of maxModel bytes
// is cut on the confirmation's status line, so its keys stay within 80
// columns.
func TestSwitchLongModelKeepsKeys(t *testing.T) {
	model := strings.Repeat("m", maxModel)
	ts, c, done, _ := switchRun(t, false)
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("aggy: switch vendor"), ts.screen)
	steps := []struct{ key, shows string }{
		{"b", "brief: on"},
		{"m", "model for this launch: "},
		{model, model},
		{"\r", "model for this launch: " + model},
	}
	for _, st := range steps {
		ts.typeIn(t, st.key)
		poll(t, "the key to land", []byte(st.shows), ts.screen)
	}
	ts.typeIn(t, "1")
	poll(t, "the keys on the status line", []byte("..., brief -> codex. Ctrl+\\ switches, Esc stays."), ts.screen)
	finish(t, c, done)
}
