package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/congdoe/aggy/internal/supervisor"
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

// hookServer runs a supervisor socket in a fresh AGGY_HOME and records
// the hook events it gets.
func hookServer(t *testing.T) (sock string, got func() []supervisor.Request) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no supervisor socket")
	}
	d, err := os.MkdirTemp("/tmp", "aggyh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	d, _ = filepath.EvalSymlinks(d)
	t.Setenv("AGGY_HOME", d)
	var mu sync.Mutex
	var reqs []supervisor.Request
	s, err := supervisor.Listen(filepath.Join(d, "run"), "11111111-2222-4333-8444-555555555555", nil,
		func(r supervisor.Request) { mu.Lock(); reqs = append(reqs, r); mu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.Path(), func() []supervisor.Request {
		mu.Lock()
		defer mu.Unlock()
		return append([]supervisor.Request(nil), reqs...)
	}
}

func TestHookForwards(t *testing.T) {
	sock, got := hookServer(t)
	env := func(kv ...string) func(string) string {
		m := map[string]string{"AGGY_SOCKET": sock}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	tests := []struct {
		name      string
		args      []string
		stdin     string
		env       func(string) string
		wantEvent string
		wantBody  string // the forwarded payload; "" means nothing forwarded
	}{
		{"claude stop", []string{"--vendor", "claude", "Stop"},
			`{"session_id":"s1","hook_event_name":"Stop","last_assistant_message":"secret text","stop_hook_active":false}`,
			env("AGGY_VENDOR", "claude"), "Stop", `{"hook_event_name":"Stop","session_id":"s1","stop_hook_active":false}`},
		{"claude stop failure", []string{"--vendor", "claude", "StopFailure"},
			`{"session_id":"s1","error":"rate_limit","error_details":"429"}`,
			env("AGGY_VENDOR", "claude"), "StopFailure", `{"error":"rate_limit","error_details":"429","session_id":"s1"}`},
		{"claude hook inside grok is dropped", []string{"--vendor", "claude", "Stop"}, `{"sessionId":"g"}`,
			env("AGGY_VENDOR", "grok", "GROK_HOOK_EVENT", "Stop", "GROK_SESSION_ID", "g"), "", ""},
		{"grok's own hook", []string{"--vendor", "grok", "StopCancelled"}, `{"sessionId":"g","reason":"user_interrupt"}`,
			env("AGGY_VENDOR", "grok", "GROK_HOOK_EVENT", "StopCancelled"), "StopCancelled", `{"reason":"user_interrupt","sessionId":"g"}`},
		{"another vendor on screen", []string{"--vendor", "codex", "Stop"}, `{"session_id":"c"}`,
			env("AGGY_VENDOR", "claude"), "", ""},
		{"codex stop", []string{"--vendor", "codex", "Stop"},
			`{"session_id":"t1","turn_id":"u","cwd":"/x","last_assistant_message":"hi"}`,
			env("AGGY_VENDOR", "codex"), "Stop", `{"cwd":"/x","session_id":"t1","turn_id":"u"}`},
		{"not JSON", []string{"--vendor", "claude", "Stop"}, `$(touch /tmp/pwned)`, env("AGGY_VENDOR", "claude"), "", ""},
		{"not under aggy", []string{"--vendor", "claude", "Stop"}, `{"session_id":"s"}`, func(string) string { return "" }, "", ""},
		{"socket elsewhere", []string{"--vendor", "claude", "Stop"}, `{"session_id":"s"}`,
			env("AGGY_VENDOR", "claude", "AGGY_SOCKET", "/tmp/11111111-2222-4333-8444-555555555555.sock"), "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(got())
			var out, errb bytes.Buffer
			if code := hookWith(tt.args, strings.NewReader(tt.stdin), tt.env, &out, &errb); code != 0 || out.Len() > 0 {
				t.Fatalf("code %d stdout %q stderr %q", code, out.String(), errb.String())
			}
			reqs := got()[before:]
			if tt.wantBody == "" {
				if len(reqs) != 0 {
					t.Errorf("forwarded %+v", reqs)
				}
				return
			}
			if len(reqs) != 1 || reqs[0].Event != tt.wantEvent || string(reqs[0].Payload) != tt.wantBody {
				t.Errorf("forwarded %+v (stderr %q)", reqs, errb.String())
			}
		})
	}
}
