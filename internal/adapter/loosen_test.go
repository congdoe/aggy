package adapter_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
)

// TestLoosenAllowed (T6979): a loosening flag the person let through by name
// passes alone, listed in Loosens and left out of Safe; every other loosening
// flag is still refused, naming itself; and each CLI says what the flag does
// to the policy, or the generic line where aggy has not checked.
func TestLoosenAllowed(t *testing.T) {
	cl, cx, gk := claude.Adapter{}.PassFlags(), codex.Adapter{}.PassFlags(), grok.New().PassFlags()
	for _, c := range []struct {
		name    string
		p       adapter.PassFlags
		args    []string
		flag    string // as LoosenedFlag names it
		effect  string // a phrase of its Effect
		another []string
	}{
		{"claude bypass", cl, []string{"--permission-mode", "bypassPermissions"}, "--permission-mode bypassPermissions", "Claude asks nothing in this mode", []string{"--dangerously-skip-permissions"}},
		{"claude skip", cl, []string{"--dangerously-skip-permissions"}, "--dangerously-skip-permissions", "Claude asks nothing in this mode", nil},
		{"codex never", cx, []string{"--ask-for-approval", "never"}, "--ask-for-approval never", "exec_policy.rs", []string{"--yolo"}},
		{"codex yolo", cx, []string{"--yolo"}, "--yolo", "unverified", nil},
		{"codex sandbox", cx, []string{"--sandbox", "danger-full-access"}, "--sandbox danger-full-access", "sandbox", nil},
		{"grok always approve", gk, []string{"--always-approve"}, "--always-approve", "approves every call", []string{"--trust"}},
		{"grok fs-write", gk, []string{"--fs-write", "x"}, "--fs-write", "aggy has not checked", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.p.Parse(c.args, false)
			f, ok := adapter.LoosenedFlag(err)
			if !ok || f != c.flag {
				t.Fatalf("LoosenedFlag(%v) = %q %v", err, f, ok)
			}
			if e := c.p.LoosenEffect(f); !strings.Contains(e, c.effect) {
				t.Errorf("effect %q lacks %q", e, c.effect)
			}
			p, err := c.p.ParseWith(c.args, adapter.ParseOpts{Allowed: []string{c.flag}})
			if err != nil || len(p.Loosens) == 0 || len(p.Safe) != 0 {
				t.Fatalf("allowed: %+v %v", p, err)
			}
			if c.another != nil {
				_, err := c.p.ParseWith(append(slices.Clone(c.args), c.another...), adapter.ParseOpts{Allowed: []string{c.flag}})
				if f2, ok := adapter.LoosenedFlag(err); !ok || f2 == c.flag {
					t.Errorf("another flag passed: %q %v", f2, err)
				}
			}
		})
	}
	if _, ok := adapter.LoosenedFlag(errors.New("x")); ok {
		t.Error("a plain error named a flag")
	}
}
