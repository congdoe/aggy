package adapter_test

import (
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/adapter/hermes"
	"github.com/congdoe/aggy/internal/adapter/opencode"
)

// TestShortClusters (PR #135 review): a short-flag word is read left to right
// as the CLI reads it, so a value attached to a flag that takes one is never
// read as flags (-ia.png passed before T6980 and must again), while a flag
// aggy checks or takes over, run on after another letter, is still refused
// (-hs danger-full-access), after a letter aggy does not know too.
func TestShortClusters(t *testing.T) {
	cx, cl, gk, oc, hm := codex.Adapter{}.PassFlags(), claude.Adapter{}.PassFlags(), grok.New().PassFlags(), opencode.Adapter{}.PassFlags(), hermes.New().PassFlags()
	cases := []struct {
		name   string
		p      adapter.PassFlags
		o      adapter.ParseOpts
		args   []string
		ok     bool
		passed string // a word that must reach the CLI as given, when ok
	}{
		{"codex -ia.png", cx, adapter.ParseOpts{}, []string{"-ia.png"}, true, "-ia.png"},
		{"codex -i with p, s, and c attached", cx, adapter.ParseOpts{}, []string{"-ipath-with-a-p-s-c.png"}, true, "-ipath-with-a-p-s-c.png"},
		{"codex -sread-only", cx, adapter.ParseOpts{}, []string{"-sread-only"}, true, "-sread-only"},
		{"codex -mgpt-5", cx, adapter.ParseOpts{}, []string{"-mgpt-5"}, true, ""},
		{"codex -pfoo", cx, adapter.ParseOpts{}, []string{"-pfoo"}, true, "-pfoo"},
		{"codex -aon-request", cx, adapter.ParseOpts{}, []string{"-aon-request"}, true, "-aon-request"},
		{"codex -hs danger-full-access", cx, adapter.ParseOpts{}, []string{"-hs", "danger-full-access"}, false, ""},
		{"codex -xs read-only", cx, adapter.ParseOpts{}, []string{"-xs", "read-only"}, false, ""},
		{"codex -hsdanger-full-access", cx, adapter.ParseOpts{}, []string{"-hsdanger-full-access"}, false, ""},
		{"claude -n with r and c attached", cl, adapter.ParseOpts{}, []string{"-nrecap"}, true, "-nrecap"},
		{"claude -pc still runs -c on", cl, adapter.ParseOpts{Print: true}, []string{"-pc"}, false, ""},
		{"grok -p with c and r attached", gk, adapter.ParseOpts{Print: true}, []string{"-pcorrect-this"}, true, "-pcorrect-this"},
		{"opencode -m from its help, an s attached", oc, adapter.ParseOpts{Values: []string{"-m"}}, []string{"-mopenai/gpt-s"}, true, "-mopenai/gpt-s"},
		{"hermes -t with c and r attached", hm, adapter.ParseOpts{}, []string{"-tcron,read"}, true, "-tcron,read"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.p.ParseWith(c.args, c.o)
			if c.ok != (err == nil) {
				t.Fatalf("err %v, want ok %v", err, c.ok)
			}
			if c.ok && c.passed != "" && !slices.Contains(got.Extra, c.passed) {
				t.Errorf("Extra %q lacks %q", got.Extra, c.passed)
			}
		})
	}
}
