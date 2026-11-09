//go:build unix

package supervisor

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/sessioncmd"
)

// TestPromptCommandTaken (T6910): a command a prompt hook reports is parsed
// again, taken only from the vendor on screen where aggy installs that hook, and
// never when it changes something.
func TestPromptCommandTaken(t *testing.T) {
	for _, c := range []struct {
		on      adapter.Vendor
		vendor  string
		command string
		code    diag.Code // zero: taken
	}{
		{adapter.Claude, "claude", "/switch grok", diag.Code{}},
		{adapter.Grok, "grok", "/inbox", diag.Code{}},
		{adapter.Claude, "claude", "/aggy", diag.Code{}},
		{adapter.Claude, "claude", "/aggy accept 9f2", diag.CodeCommandKeyboard},
		{adapter.Claude, "claude", "/aggy tool on rtk", diag.CodeCommandKeyboard},
		{adapter.Claude, "claude", "/aggy trust", diag.CodeCommandKeyboard},
		{adapter.Claude, "claude", "/borrow research x", diag.CodeCommandNotTaken},
		{adapter.Claude, "claude", "hello", diag.CodeCommandNotTaken},
		{adapter.Claude, "grok", "/switch codex", diag.CodeWrongVendor},
		{adapter.Codex, "codex", "$switch grok", diag.CodeCommandNotTaken},
		{adapter.OpenCode, "opencode", "/switch grok", diag.CodeCommandNotTaken},
	} {
		t.Run(string(c.on)+" "+c.command, func(t *testing.T) {
			s, _ := viewSess(t, c.on, "")
			s.prompts = make(chan sessioncmd.Command, 1)
			err := s.onSwitch(Request{Type: ReqPromptCommand, Vendor: c.vendor, Command: c.command})
			if c.code.IsZero() {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if len(s.prompts) != 1 {
					t.Fatal("not queued for the main loop")
				}
				return
			}
			if err == nil || codeOf(err, diag.Code{}) != c.code {
				t.Fatalf("err %v, want code %s", err, c.code)
			}
		})
	}
}

// TestHookedSwitchAsks (T6910): a switch a prompt hook reported starts no turn
// (the hook blocked the prompt), so it does not wait for a turn end; but a hook
// can be forged, so the status line asks even on Claude.
func TestHookedSwitchAsks(t *testing.T) {
	ts, ch, done, sock := switchRunTurned(t, adapter.Claude, func(o *Options) {
		o.Aggy = func([]string, bool) string { return "" }
	})
	ts.typeIn(t, "/switch grok\r") // one read: not caught from the keys
	ch.waitGot(t, []byte("/switch grok\r"))
	resp := request(t, sock, `{"type":"prompt-command","vendor":"claude","command":"/switch grok"}`)
	if !strings.Contains(resp, `"ok":true`) {
		t.Fatalf("response %s", resp)
	}
	poll(t, "the status line", []byte("/switch via hook -> grok. Ctrl+"), ts.screen)
	if bytes.Contains(ch.received(), []byte("/exit")) {
		t.Fatalf("the exit command went before the person confirmed: %q", ch.received())
	}
	time.Sleep(confirmGuard + 50*time.Millisecond)
	ts.typeIn(t, "\x1c")
	poll(t, "the exit command", []byte("/exit"), ch.received)
	ch.quit(0)
	select {
	case ex := <-done:
		if ex.SwitchTo != adapter.Grok {
			t.Errorf("exit %+v, want a switch to grok", ex)
		}
	case <-time.After(waitLimit):
		t.Fatal("Run did not return")
	}
}

// TestDecodePromptCommand: the request's fields are checked.
func TestDecodePromptCommand(t *testing.T) {
	for _, c := range []struct {
		line string
		ok   bool
	}{
		{`{"type":"prompt-command","vendor":"claude","command":"/switch grok"}`, true},
		{`{"type":"prompt-command","vendor":"nobody","command":"/switch grok"}`, false},
		{`{"type":"prompt-command","vendor":"claude"}`, false},
		{`{"type":"prompt-command","vendor":"claude","command":"` + strings.Repeat("x", 257) + `"}`, false},
		{`{"type":"prompt-command","vendor":"claude","command":"/inbox","event":"Stop"}`, false},
		{`{"type":"hook-event","vendor":"claude","event":"Stop","payload":{},"command":"/inbox"}`, false},
	} {
		if _, err := decodeRequest([]byte(c.line)); (err == nil) != c.ok {
			t.Errorf("decode %s: err %v, want ok %v", c.line, err, c.ok)
		}
	}
}
