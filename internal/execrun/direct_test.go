//go:build unix

package execrun

import (
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

func TestRunDirect(t *testing.T) {
	cmd := func(script string, env ...string) adapter.Command {
		return adapter.Command{Path: "/bin/sh", Args: []string{"-c", script}, Dir: t.TempDir(), Stdin: adapter.StdinTerminal, Env: env}
	}
	tests := []struct {
		name string
		c    adapter.Command
		code int
		err  string
	}{
		{"exit code", cmd("exit 7"), 7, ""},
		{"signal", cmd("kill -TERM $$"), 128 + 15, ""},
		{"aggy vars reach it", cmd(`test "$AGGY_VENDOR" = claude || exit 9`, "AGGY_VENDOR=claude"), 0, ""},
		{"other vars refused", cmd("exit 0", "ANTHROPIC_BASE_URL=x"), 1, "refusing to pass ANTHROPIC_BASE_URL"},
		{"relative path", adapter.Command{Path: "sh", Dir: "/", Stdin: adapter.StdinTerminal}, 1, "not a clean absolute path"},
		{"headless command", adapter.Command{Path: "/bin/sh", Dir: "/", Stdin: adapter.StdinNull}, 1, "interactive commands only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, err := RunDirect(tt.c)
			if code != tt.code || (tt.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.err)) {
				t.Errorf("RunDirect = %d, %v", code, err)
			}
		})
	}
}
