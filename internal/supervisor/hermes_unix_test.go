//go:build unix

package supervisor

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/diag"
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
	if got := r.Answer(b, false); !got.OK || !got.NoPolicy {
		t.Fatalf("resume: %+v", got)
	}
	if got := r.Answer(a, false); got.OK {
		t.Fatalf("resume took another session: %+v", got)
	}
}

// TestHermesClaimShared (T6954): with another aggy Hermes launch live in the
// profile, a new session claims an unclaimed launch only within
// HermesClaimWindow of the last Enter typed in it (or its brief), and never
// under --direct; alone in its profile, a launch claims as before, however
// long after. The claimed session is always answered.
func TestHermesClaimShared(t *testing.T) {
	const a, b = "20261110_101500_a1b2c3", "20261110_101600_d4e5f6"
	clock := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	at := func(p HermesPin) *HermesClaim {
		h := NewHermesClaim(p)
		h.now = func() time.Time { return clock }
		if p.Brief && !p.Direct {
			h.entered = clock
		}
		return h
	}
	late := func(r Response) bool { return !r.OK && r.Code == diag.CodeHermesClaimLate.String() }
	step := func(d time.Duration) { clock = clock.Add(d) }

	// Alone in the profile: no window.
	h := at(HermesPin{})
	h.Entered()
	step(10 * time.Minute)
	if r := h.Answer(a, false); !r.OK {
		t.Fatalf("alone, long after the Enter: %+v", r)
	}

	// Shared: within the window after the Enter, the claim is taken, and the
	// claimed session is answered later whatever the time.
	h = at(HermesPin{})
	h.Entered()
	step(HermesClaimWindow - time.Second)
	if r := h.Answer(a, true); !r.OK {
		t.Fatalf("shared, within the window: %+v", r)
	}
	step(time.Hour)
	if r := h.Answer(a, true); !r.OK {
		t.Fatalf("the claimed session later: %+v", r)
	}
	if r := h.Answer(b, true); r.OK || late(r) {
		t.Fatalf("another session after the claim: %+v", r)
	}

	// Shared, past the window (another launch's /new, say): refused, and the
	// launch stays unclaimed; an Enter typed again opens the window again.
	h = at(HermesPin{})
	h.Entered()
	step(HermesClaimWindow + time.Second)
	if r := h.Answer(b, true); !late(r) {
		t.Fatalf("shared, past the window: %+v", r)
	}
	if h.Claimed() != "" {
		t.Fatalf("claimed %q past the window", h.Claimed())
	}
	h.Entered()
	if r := h.Answer(a, true); !r.OK || h.Claimed() != a {
		t.Fatalf("after another Enter: %+v %q", r, h.Claimed())
	}

	// Shared, before any Enter: refused as before (not the launch's yet).
	h = at(HermesPin{})
	if r := h.Answer(a, true); r.OK {
		t.Fatalf("before the first turn: %+v", r)
	}

	// The brief is the first turn, sent at launch: the window runs from then.
	h = at(HermesPin{Brief: true})
	step(HermesClaimWindow + time.Second)
	if r := h.Answer(a, true); !late(r) {
		t.Fatalf("brief, past the window: %+v", r)
	}
	h = at(HermesPin{Brief: true})
	if r := h.Answer(a, true); !r.OK {
		t.Fatalf("brief, within the window: %+v", r)
	}

	// --direct: aggy sees no keys, so a shared claim is never taken; alone,
	// the first session named is.
	h = at(HermesPin{Direct: true})
	if r := h.Answer(a, true); !late(r) {
		t.Fatalf("--direct, shared: %+v", r)
	}
	if r := h.Answer(a, false); !r.OK {
		t.Fatalf("--direct, alone: %+v", r)
	}
	// A resume knows its session under --direct too.
	h = at(HermesPin{Direct: true, Session: b})
	if r := h.Answer(b, true); !r.OK {
		t.Fatalf("--direct resume, shared: %+v", r)
	}
}
