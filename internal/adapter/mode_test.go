package adapter_test

import (
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/adapter/hermes"
	"github.com/congdoe/aggy/internal/adapter/opencode"
)

// TestClusterContinue (PR #135 re-review): a Continue flag leading a cluster
// is refused, as before e804f55: the CLI would continue its own last session
// while aggy records a new one.
func TestClusterContinue(t *testing.T) {
	cl, gk := claude.Adapter{}.PassFlags(), grok.New().PassFlags()
	for _, c := range []struct {
		name string
		p    adapter.PassFlags
		args []string
	}{
		{"claude -cd", cl, []string{"-cd"}},
		{"claude -cv", cl, []string{"-cv"}},
		{"claude -cdv", cl, []string{"-cdv"}},
		{"claude -cp hi", cl, []string{"-cp", "hi"}},
		{"grok -cv", gk, []string{"-cv"}},
		{"grok -cd", gk, []string{"-cd"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, o := range []adapter.ParseOpts{{}, {Print: true}} {
				if _, err := c.p.ParseWith(c.args, o); err == nil || !strings.Contains(err.Error(), "write -c on its own") {
					t.Errorf("print %v: err %v", o.Print, err)
				}
			}
		})
	}
}

// TestModeReadsClusters (PR #135 re-review): Mode reads a short-flag word as
// ParseWith does, so a Print short with its value attached, or run on after
// another letter, runs the CLI in print mode, where no one answers an ask,
// and a letter inside another flag's value does not.
func TestModeReadsClusters(t *testing.T) {
	cl, cx, gk, oc, hm := claude.Adapter{}.PassFlags(), codex.Adapter{}.PassFlags(), grok.New().PassFlags(), opencode.Adapter{}.PassFlags(), hermes.New().PassFlags()
	for _, c := range []struct {
		name string
		p    adapter.PassFlags
		args []string
		want adapter.RunMode
	}{
		{"grok -pfoo", gk, []string{"-pfoo"}, adapter.RunPrint},
		{"grok -p=foo", gk, []string{"-p=foo"}, adapter.RunPrint},
		{"grok -dp x", gk, []string{"-dp", "x"}, adapter.RunPrint},
		{"grok -p x", gk, []string{"-p", "x"}, adapter.RunPrint},
		{"grok a p in a model value", gk, []string{"-mgrok-p"}, adapter.RunScreen},
		{"claude -p", cl, []string{"-p", "hi"}, adapter.RunPrint},
		{"claude -pfoo, -p takes no value", cl, []string{"-pfoo"}, adapter.RunPrint},
		{"claude -dp", cl, []string{"-dp", "hi"}, adapter.RunPrint},
		{"claude a p in -n's value", cl, []string{"-nmy-p"}, adapter.RunScreen},
		{"codex --worktree=x", cx, []string{"--worktree=x"}, adapter.RunPrint},
		{"codex exec", cx, []string{"exec", "hi"}, adapter.RunPrint},
		{"codex a p in -i's value", cx, []string{"-ia.png"}, adapter.RunScreen},
		{"opencode has no print flag", oc, []string{"-p"}, adapter.RunScreen},
		{"hermes has no print flag", hm, []string{"-tp"}, adapter.RunScreen},
		{"after -- is prompt", gk, []string{"--", "-pfoo"}, adapter.RunScreen},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.p.Mode(c.args); got != c.want {
				t.Errorf("Mode(%q) = %d, want %d", c.args, got, c.want)
			}
		})
	}
}
