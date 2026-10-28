package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no vendor", []string{"run"}, "usage: aggy run"},
		{"two vendors", []string{"run", "claude", "codex"}, "usage: aggy run"},
		{"unknown vendor", []string{"run", "gpt"}, "unknown vendor"},
		{"--tool without a name", []string{"run", "--tool"}, "usage: aggy run"},
		{"--tool not a name", []string{"run", "--tool", "../x", "claude"}, "not a tool name"},
		{"--tool= not a name", []string{"run", "--tool=A B", "claude"}, "not a tool name"},
		{"unknown flag", []string{"run", "--tools", "x", "claude"}, "usage: aggy run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			t.Setenv("PATH", t.TempDir())
			var out, errb bytes.Buffer
			if code := Run(tt.args, &out, &errb); code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.Contains(errb.String(), tt.want) {
				t.Errorf("stderr = %q, want %q", errb.String(), tt.want)
			}
		})
	}
}
