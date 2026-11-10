package cli

import (
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/config"
)

// TestConfigCommandsOff (R121): the person's config can leave aggy's
// commands inside a session to the CLI, all at once or one name at a time,
// and aggy config list shows each.
func TestConfigCommandsOff(t *testing.T) {
	isolate(t)
	for _, tc := range []struct {
		key, val string
		off      []string // CatchOff after
	}{
		{"command.inbox", "off", []string{"inbox"}},
		{"command.switch", "off", []string{"switch", "inbox"}},
		{"command.inbox", "on", []string{"switch"}},
		{"commands", "off", []string{"switch", "borrow", "inbox", "aggy"}},
		{"commands", "on", []string{"switch"}},
	} {
		atKeyboard(t, "y\n")
		if code, out, errb := run("config", "set", tc.key, tc.val); code != 0 {
			t.Fatalf("%s %s: %d %q %q", tc.key, tc.val, code, out, errb)
		}
		cfg, err := config.LoadDefault()
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range config.CommandNames {
			want := false
			for _, o := range tc.off {
				want = want || o == n
			}
			if cfg.Commands.CatchOff(n) != want {
				t.Errorf("after %s %s: %s off = %v", tc.key, tc.val, n, !want)
			}
		}
	}
	_, out, _ := run("config", "list")
	for _, w := range []string{"commands           on", "command.switch     off", "command.inbox      on"} {
		if !strings.Contains(out, w) {
			t.Errorf("list lacks %q:\n%s", w, out)
		}
	}
	if code, _, errb := run("config", "set", "command.nope", "off"); code == 0 || !strings.Contains(errb, "not one of aggy's commands") {
		t.Errorf("command.nope: %d %q", code, errb)
	}
}
