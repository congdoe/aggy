//go:build unix

package supervisor

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestHermesToken (T6933): a Hermes hook runs in Hermes' daemon, outside the
// CLI's process tree, so a hook-event counts as the session's only with the
// launch's token; any other line, vendor, or token does not.
func TestHermesToken(t *testing.T) {
	const tok = "0123456789abcdef0123456789abcdef"
	run := filepath.Join(shortDir(t), "run")
	var mu sync.Mutex
	var got []Request
	s, err := Listen(run, pid, nil, func(r Request) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.SetRoot(1 << 30) // no process: no connection has a lineage to it
	s.SetToken(tok)
	tests := []struct {
		name string
		line string
		want string // a reply substring
		from bool
	}{
		{"the token", `{"type":"hook-event","vendor":"hermes","event":"Stop","payload":{"session_id":"x"},"token":"` + tok + `"}`, `"ok":true`, true},
		{"another token", `{"type":"hook-event","vendor":"hermes","event":"Stop","payload":{},"token":"` + tok[1:] + `0"}`, `"ok":true`, false},
		{"no token", `{"type":"hook-event","vendor":"hermes","event":"Stop","payload":{}}`, `"ok":true`, false},
		{"a token on another vendor", `{"type":"hook-event","vendor":"claude","event":"Stop","payload":{},"token":"` + tok + `"}`, `token`, false},
		{"a token on a switch", `{"type":"switch-request","vendor":"claude","token":"` + tok + `"}`, `token`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mu.Lock()
			got = nil
			mu.Unlock()
			if reply := ask(t, s, tt.line+"\n"); !strings.Contains(reply, tt.want) {
				t.Fatalf("reply %q, want %q", reply, tt.want)
			}
			mu.Lock()
			defer mu.Unlock()
			if tt.from && (len(got) != 1 || !got[0].FromSession) {
				t.Fatalf("hook = %+v, want one from the session", got)
			}
			if !tt.from && len(got) == 1 && got[0].FromSession {
				t.Fatalf("hook = %+v counted as the session's", got)
			}
		})
	}
	if hermesToken([]byte(`{"type":"hook-event","vendor":"hermes","token":""}`), "") {
		t.Error("an empty token matched")
	}
}
