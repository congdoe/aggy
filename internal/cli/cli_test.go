package cli

import (
	"bytes"
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
		{name: "no args prints usage", args: nil, wantCode: 0, wantStdout: "Usage:"},
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "aggy dev"},
		{name: "version flag", args: []string{"--version"}, wantCode: 0, wantStdout: "aggy dev"},
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "Commands:"},
		{name: "unknown command", args: []string{"nope"}, wantCode: 2, wantStderr: `unknown command "nope"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
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
