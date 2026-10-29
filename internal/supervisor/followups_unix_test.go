//go:build unix

package supervisor

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestSwitchLateChildCheck (T6770, #79): a child check slower than
// childCheckWait lets the switch be offered again; its answer, landing
// after the user typed past that offer, never types the exit command.
func TestSwitchLateChildCheck(t *testing.T) {
	old := childCheckWait
	t.Cleanup(func() { childCheckWait = old })
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	var asked atomic.Bool
	ts, c, done, _ := switchRunOn(t, false, adapter.Claude, func(o *Options) {
		childCheckWait = 100 * time.Millisecond // before Run starts
		o.Children = func(v adapter.Vendor) []ChildChoice {
			if v == adapter.Grok {
				return []ChildChoice{{"g2", "latest"}, {"g1", "older"}}
			}
			return nil
		}
		o.HasChild = func(adapter.Vendor, string) bool {
			asked.Store(true)
			<-gate
			return true
		}
	})
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("aggy: switch vendor"), ts.screen)
	ts.typeIn(t, "2")
	poll(t, "the children", []byte("older"), ts.screen)
	ts.typeIn(t, "2")
	poll(t, "the confirmation", []byte("-> grok. Ctrl+"), ts.screen)
	confirm(t, ts)
	deadline := time.Now().Add(waitLimit)
	for !asked.Load() {
		if time.Now().After(deadline) {
			t.Fatal("HasChild was not asked")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mark := len(ts.screen())
	poll(t, "the offer again", []byte("-> grok. Ctrl+"), func() []byte { return ts.screen()[mark:] })
	ts.typeIn(t, "x") // closes the offer and goes to the CLI
	c.waitGot(t, []byte("x"))
	ts.typeIn(t, "\x7f")
	c.waitGot(t, []byte("x\x7f"))
	release()
	never(t, "the exit command from the late answer", []byte("/exit"), c.received, 300*time.Millisecond)
	finish(t, c, done)
}

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
