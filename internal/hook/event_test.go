package hook

import (
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

func TestProjectAndParse(t *testing.T) {
	long := strings.Repeat("x", maxField)
	tests := []struct {
		name    string
		v       adapter.Vendor
		payload string
		want    string
		ev      Event
	}{
		{"claude start", adapter.Claude, `{"session_id":"a","source":"resume","prompt":"p"}`,
			`{"session_id":"a","source":"resume"}`, Event{SessionID: "a", Source: "resume"}},
		{"long details dropped", adapter.Claude, `{"session_id":"a","error":"rate_limit","error_details":"` + long + `"}`,
			`{"error":"rate_limit","session_id":"a"}`, Event{SessionID: "a", Error: "rate_limit"}},
		{"grok camel case", adapter.Grok, `{"sessionId":"g","session_id":"no","error":"rate_limit"}`,
			`{"error":"rate_limit","sessionId":"g"}`, Event{SessionID: "g", Error: "rate_limit"}},
		{"codex stop", adapter.Codex, `{"session_id":"t","turn_id":"u","last_assistant_message":"m"}`,
			`{"session_id":"t","turn_id":"u"}`, Event{SessionID: "t"}},
		{"non-string id ignored", adapter.Codex, `{"session_id":5}`, `{"session_id":5}`, Event{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Project(tt.v, []byte(tt.payload))
			if err != nil || string(p) != tt.want {
				t.Fatalf("Project = %s, %v", p, err)
			}
			if ev := Parse(tt.v, p); ev != tt.ev {
				t.Errorf("Parse = %+v", ev)
			}
		})
	}
	for _, bad := range []string{``, `[]`, `"x"`, `{`, `null`} {
		if _, err := Project(adapter.Claude, []byte(bad)); err == nil {
			t.Errorf("Project(%q) worked", bad)
		}
	}
}

func TestForward(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	tests := []struct {
		name  string
		label adapter.Vendor
		env   func(string) string
		want  bool
	}{
		{"no socket", adapter.Claude, env("AGGY_VENDOR", "claude"), false},
		{"claude", adapter.Claude, env("AGGY_SOCKET", "s", "AGGY_VENDOR", "claude"), true},
		{"claude hook run by grok", adapter.Claude, env("AGGY_SOCKET", "s", "AGGY_VENDOR", "grok", "GROK_HOOK_EVENT", "Stop"), false},
		{"claude hook, grok session env", adapter.Claude, env("AGGY_SOCKET", "s", "AGGY_VENDOR", "claude", "GROK_SESSION_ID", "x"), false},
		{"grok", adapter.Grok, env("AGGY_SOCKET", "s", "AGGY_VENDOR", "grok", "GROK_HOOK_EVENT", "Stop"), true},
		{"vendor mismatch", adapter.Codex, env("AGGY_SOCKET", "s", "AGGY_VENDOR", "claude"), false},
		{"no vendor", adapter.Codex, env("AGGY_SOCKET", "s"), false},
	}
	for _, tt := range tests {
		if got := Forward(tt.label, tt.env); got != tt.want {
			t.Errorf("%s: Forward = %v", tt.name, got)
		}
	}
}
