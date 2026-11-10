package cli

import (
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/diag"
)

// TestShellGate (R121): accept, reject, and tool on|off refuse in a borrow
// child, and in a session's tool shell with no terminal; a person at a
// terminal, and aggy's own process, pass.
func TestShellGate(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		tty  bool
		ok   bool
	}{
		{"a terminal outside aggy", nil, true, true},
		{"no terminal outside aggy (aggy's own page)", nil, false, true},
		{"a borrow child", map[string]string{"AGGY_DELEGATED": "1"}, true, false},
		{"a session's tool shell", map[string]string{"AGGY_SOCKET": "/s", "AGGY_PARENT": "p", "AGGY_VENDOR": "claude"}, false, false},
		{"only AGGY_VENDOR, no terminal", map[string]string{"AGGY_VENDOR": "claude"}, false, false},
		{"a session's shell at a terminal", map[string]string{"AGGY_SOCKET": "/s"}, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := shellGate(func(k string) string { return c.env[k] }, c.tty)
			if (err == nil) != c.ok {
				t.Fatalf("err %v, want ok %v", err, c.ok)
			}
			if err != nil && codeOf(err, diag.Code{}) != diag.CodeNoTerminal {
				t.Errorf("code %v", codeOf(err, diag.Code{}))
			}
		})
	}
}

// TestChangeRefusedInToolShell (R121): aggy accept --yes, reject, and tool on
// from a session's tool shell, with no terminal, are refused before they read
// or change anything.
func TestChangeRefusedInToolShell(t *testing.T) {
	isolate(t)
	t.Setenv("AGGY_SOCKET", "/s")
	t.Setenv("AGGY_PARENT", "p")
	for _, args := range [][]string{{"accept", "0123456789abcdef", "--yes"}, {"reject", "0123456789abcdef"}, {"tool", "on", "rtk"}} {
		code, out, errOut := run(args...)
		if code == 0 || !strings.Contains(errOut, "session's own tool shell") {
			t.Errorf("aggy %v: %d %q %q", args, code, out, errOut)
		}
	}
}
