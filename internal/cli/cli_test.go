package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no args needs a terminal", args: nil, wantCode: 1, wantStderr: "needs a terminal"},
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "aggy dev"},
		{name: "version flag", args: []string{"--version"}, wantCode: 0, wantStdout: "aggy dev"},
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "Commands:"},
		{name: "unknown command", args: []string{"nope"}, wantCode: 2, wantStderr: `unknown command "nope"`},
	}
	// Bare aggy launches a session: keep it off the real home and CLIs.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			t.Setenv("PATH", t.TempDir())
			var stdout, stderr bytes.Buffer
			stdin := os.Stdin
			os.Stdin = nil // not a terminal, whatever go test was given
			defer func() { os.Stdin = stdin }()
			code := Run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestPickVersion(t *testing.T) {
	tests := []struct{ set, module, want string }{
		{"v0.1.0", "v0.1.0", "v0.1.0"},
		{"v0.1.0", "(devel)", "v0.1.0"},
		{"dev", "v0.1.0", "v0.1.0"},
		{"dev", "(devel)", "dev"},
		{"dev", "", "dev"},
	}
	for _, tt := range tests {
		if got := pickVersion(tt.set, tt.module); got != tt.want {
			t.Errorf("pickVersion(%q, %q) = %q, want %q", tt.set, tt.module, got, tt.want)
		}
	}
}
