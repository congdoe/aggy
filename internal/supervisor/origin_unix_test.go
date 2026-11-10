//go:build unix

package supervisor

import (
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestSwitchOriginField (T6925): who asked for a switch, as aggy log shows
// it, comes from the fields the confirmation line reads.
func TestSwitchOriginField(t *testing.T) {
	tests := []struct {
		name string
		p    pendingSwitch
		want string
	}{
		{"user pick", pendingSwitch{picked: true}, "user"},
		{"agent ask", pendingSwitch{asked: true}, "agent"},
		{"agent ask picked again", pendingSwitch{picked: true}, "user"},
		{"limit", pendingSwitch{limit: true}, "limit"},
		{"hook-only limit", pendingSwitch{limit: true, unverified: true}, "limit, unverified"},
		{"limit picked again", pendingSwitch{limit: true, unverified: true, picked: true}, "user"},
		{"none set", pendingSwitch{}, "user"},
	}
	for _, tt := range tests {
		if got := tt.p.origin(); got != tt.want {
			t.Errorf("%s: origin %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestSwitchOriginFacts (T6925): Run reports the origin in RunFacts: a
// limit aggy queued, one only a hook reported, and a limit's switch the user
// picked again in the list, which becomes the user's.
func TestSwitchOriginFacts(t *testing.T) {
	tests := []struct {
		name    string
		hit     LimitHit
		repick  bool
		confirm string
		want    string
	}{
		{"limit", LimitHit{Vendor: adapter.Claude}, false, "claude hit its limit, brief -> grok.", "limit"},
		{"unverified", LimitHit{Vendor: adapter.Claude, Unverified: true}, false, "claude reports a limit (unverified), brief -> grok.", "limit, unverified"},
		{"picked again", LimitHit{Vendor: adapter.Claude, Unverified: true}, true, "-> grok. Ctrl+", "user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var facts RunFacts
			ch := make(chan LimitHit, 1)
			ts, c, done, sock := switchRunOn(t, true, adapter.Claude, limitOpts(ch, adapter.Grok), func(o *Options) { o.Facts = &facts })
			if err := pty.Setsize(ts.master, &pty.Winsize{Rows: 24, Cols: 160}); err != nil {
				t.Fatal(err)
			}
			ch <- tt.hit
			if tt.repick {
				time.Sleep(100 * time.Millisecond) // the limit is queued first
				ts.typeIn(t, "\x1c")
				poll(t, "the list", []byte("2  grok"), ts.screen)
				ts.typeIn(t, "2")
				poll(t, "the list to close", []byte(altScreenOff), ts.screen)
			}
			_ = request(t, sock, `{"type":"hook-event","vendor":"claude","event":"StopFailure","payload":{"session_id":"`+childSID+`","error":"rate_limit"}}`)
			poll(t, "the confirmation", []byte(tt.confirm), ts.screen)
			confirm(t, ts)
			c.waitGot(t, []byte("/exit\r"))
			c.quit(0)
			wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok, Brief: true}})
			if facts.SwitchOrigin != tt.want {
				t.Errorf("origin %q, want %q", facts.SwitchOrigin, tt.want)
			}
		})
	}
}
