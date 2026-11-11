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

// TestHermesPin (#134 review): the decision plan's pin and the launch's
// session live in the supervisor. Only a hermes-pin with the token is
// answered; a new session claims the launch only after its first turn was
// sent from the terminal, and no other session is ever answered.
func TestHermesPin(t *testing.T) {
	const tok = "0123456789abcdef0123456789abcdef"
	const a, b = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "20261110_101500_a1b2c3"
	run := filepath.Join(shortDir(t), "run")
	s, err := Listen(run, pid, nil, func(Request) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.SetRoot(1 << 30)
	s.SetToken(tok)
	claim := NewHermesClaim(HermesPin{Plan: "/plan", PlanSum: "sum"})
	s.SetHermesPin(claim.Answer)
	pin := func(id, token string) string {
		return ask(t, s, `{"type":"hermes-pin","vendor":"hermes","payload":{"session_id":"`+id+`"},"token":"`+token+`"}`+"\n")
	}
	if r := pin(a, tok); !strings.Contains(r, "hermes.no_launch") {
		t.Fatalf("before the first turn: %q", r)
	}
	claim.Entered()
	if r := pin(a, tok[1:]+"0"); strings.Contains(r, `"ok":true`) || strings.Contains(r, "sum") {
		t.Fatalf("another token: %q", r)
	}
	if r := pin(a, tok); !strings.Contains(r, `"ok":true`) || !strings.Contains(r, `"plan_sum":"sum"`) {
		t.Fatalf("first session: %q", r)
	}
	if r := pin(b, tok); !strings.Contains(r, "hermes.no_launch") {
		t.Fatalf("a second session: %q", r)
	}
	if r := pin(a, tok); !strings.Contains(r, `"ok":true`) {
		t.Fatalf("the launch's session again: %q", r)
	}
	if r := pin("../x", tok); strings.Contains(r, `"ok":true`) {
		t.Fatalf("a bad id: %q", r)
	}
	if claim.Claimed() != a {
		t.Fatalf("claimed %q", claim.Claimed())
	}
	// A resume knows its session up front, and takes no other.
	r := NewHermesClaim(HermesPin{Session: b, NoPolicy: true})
	if got := r.Answer(b); !got.OK || !got.NoPolicy {
		t.Fatalf("resume: %+v", got)
	}
	if got := r.Answer(a); got.OK {
		t.Fatalf("resume took another session: %+v", got)
	}
}
