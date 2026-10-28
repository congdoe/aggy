package adapter

import (
	"errors"
	"testing"
)

// TestCheckLockdownFormats (#10-9): only each CLI's own rejection lines
// count, on stderr or stdout, and only on a non-zero exit.
func TestCheckLockdownFormats(t *testing.T) {
	tests := []struct {
		name   string
		v      Vendor
		code   int
		stderr string
		stdout string
		want   bool
	}{
		{"commander unknown option", Claude, 1, "error: unknown option '--strict-mcp-config'\n", "", true},
		{"commander invalid choice", Claude, 1, "error: option '--permission-mode <mode>' argument 'plan' is invalid. Allowed choices are default.\n", "", true},
		{"clap unexpected argument", Codex, 2, "error: unexpected argument '--ignore-user-config' found\n\nUsage: codex exec\n", "", true},
		{"clap invalid value", Codex, 2, "error: invalid value 'read-only' for '--sandbox <SANDBOX_MODE>'\n", "", true},
		{"codex strict config", Codex, 1, "Error: unknown configuration field `features.shell_tool` in -c/--config override\n", "", true},
		{"rejection on stdout", Codex, 2, "", "error: unexpected argument '--disable' found\n", true},
		{"grok, clap form", Grok, 2, "error: unexpected argument '--no-subagents' found\n", "", true},
		{"grok, clap invalid value", Grok, 2, "error: invalid value 'yolo' for '--permission-mode <MODE>'\n", "", true},
		// C17-6: Grok is clap, and the Codex config message is anchored
		// and read on stderr only.
		{"grok, commander form", Grok, 1, "error: unknown option '--no-subagents'\n", "", false},
		{"codex config message quoted", Codex, 1, "turn failed: unknown configuration field `x` in -c/--config override\n", "", false},
		{"codex config message on stdout", Codex, 1, "", "unknown configuration field `x` in -c/--config override\n", false},
		{"codex bare config message", Codex, 1, "unknown configuration field `features.x` in -c/--config override\n", "", true},
		// C17-8: the line codex exec really prints.
		{"codex exec config message", Codex, 1, "Error loading config.toml: unknown configuration field `features.shell_tool` in -c/--config override\n", "", true},
		{"unrelated invalid value", Codex, 1, "stream error: invalid value in response body\n", "", false},
		{"unrelated unknown option text", Claude, 1, "API Error: the model said unknown option '--x'\n", "", false},
		{"a model's reply on stdout", Claude, 1, "", `{"result":"error: unknown option is a phrase"}`, false},
		{"commander form on codex", Codex, 2, "error: unknown option '--x'\n", "", false},
		{"exit 0", Claude, 0, "error: unknown option '--x'\n", "", false},
		// C65-1: a vendor rejected() does not know fails closed on any
		// failed run, and still passes a clean one.
		{"unknown vendor fails closed", Vendor("other"), 1, "some failure\n", "", true},
		{"unknown vendor, exit 0", Vendor("other"), 0, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckLockdown(tt.v, Output{ExitCode: tt.code, Stderr: []byte(tt.stderr), Stdout: []byte(tt.stdout)})
			if errors.Is(err, ErrLockdownRejected) != tt.want {
				t.Errorf("CheckLockdown = %v, want rejected %v", err, tt.want)
			}
		})
	}
}
