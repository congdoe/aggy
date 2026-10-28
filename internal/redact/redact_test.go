package redact

import (
	"strings"
	"testing"
)

func TestLines(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		removed        int
	}{
		{"clean", "a\nb", "a\nb", 0},
		{"openai key", "x\nkey is sk-abcdefghijklmnop1234 ok\ny", "x\ny", 1},
		{"env line", "export ANTHROPIC_API_KEY=abcd1234efgh\nPATH=/bin", "PATH=/bin", 1},
		{"json secret", `{"client_secret": "hunter2hunter2"}` + "\nz", "z", 1},
		{"codex auth.json shape", "{\n  \"tokens\": {\n    \"id_token\": \"eyJhbGciOi.eyJzdWIi.sig\",\n    \"refresh_token\": \"rt_abcdefghijklmnopqrstu\"\n  }\n}", "{\n  \"tokens\": {\n  }\n}", 2},
		{"pem block", "a\n-----BEGIN RSA PRIVATE KEY-----\nMIIabc\ndef\n-----END RSA PRIVATE KEY-----\nb", "a\nb", 4},
		{"pem unterminated", "a\n-----BEGIN PRIVATE KEY-----\nMIIabc\nmore", "a", 3},
		{"short values stay", "max_tokens: 4096\npassword: no", "max_tokens: 4096\npassword: no", 0},
		{"bearer", "curl -H 'Authorization: Bearer abcdefghijklmnopqrst'", "", 1},
	} {
		got, n := Lines(tc.in)
		if got != tc.want || n != tc.removed {
			t.Errorf("%s: got %q (%d removed), want %q (%d)", tc.name, got, n, tc.want, tc.removed)
		}
		if strings.Contains(got, "hunter2") {
			t.Errorf("%s: secret left", tc.name)
		}
	}
}
