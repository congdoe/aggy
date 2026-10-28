package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestHookPreToolUse(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		stdin   string
		code    int
		wantErr string
	}{
		{"deny", []string{"pretooluse"}, `{"tool_name":"Bash","tool_input":{"command":"claude -p x"}}`, 2, "one hop"},
		{"allow", []string{"pretooluse"}, `{"tool_name":"Bash","tool_input":{"command":"ls"}}`, 0, ""},
		{"garbage fails closed", []string{"pretooluse"}, `$(rm -rf /)`, 2, "not a JSON object"},
		{"too large", []string{"pretooluse"}, strings.Repeat("x", 1<<20+1), 2, "could not be read"},
		{"event without vendor", []string{"Stop"}, `{}`, 1, "Usage"},
		{"unknown vendor", []string{"--vendor", "gpt", "Stop"}, `{}`, 1, "gpt"},
		{"extra argument", []string{"--vendor", "claude", "Stop", "{}"}, ``, 1, "Usage"},
		{"no event", nil, ``, 1, "Usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := hookWith(tt.args, strings.NewReader(tt.stdin), func(string) string { return "" }, &out, &errb)
			if code != tt.code || !strings.Contains(errb.String(), tt.wantErr) || out.Len() > 0 {
				t.Errorf("code %d stdout %q stderr %q", code, out.String(), errb.String())
			}
		})
	}
}
