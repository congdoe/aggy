package switchboard

import (
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/tooldef"
)

func tool(name, scope string, builtIn, def bool, vendors ...adapter.Vendor) tooldef.Tool {
	return tooldef.Tool{BuiltIn: builtIn, Def: tooldef.Def{Name: name, Scope: scope, Default: def,
		MCP: []tooldef.MCP{{Vendors: vendors, Argv: []string{name, "serve"}}}}}
}

func sw(tool, vendor, repo string, on bool) store.ToolSwitch {
	return store.ToolSwitch{Tool: tool, Vendor: vendor, Repo: repo, On: on}
}

const repo = "/r/app"

func TestState(t *testing.T) {
	all := []adapter.Vendor{adapter.Claude, adapter.Codex, adapter.Grok}
	user := tool("demo", "global", false, false, all...)
	perRepo := tool("idx", "repo", true, false, all...)
	claudeOnly := tool("demo", "global", false, false, adapter.Claude)
	cases := []struct {
		name    string
		t       tooldef.Tool
		sws     []store.ToolSwitch
		repoOff []string
		v       adapter.Vendor
		repo    string
		on      bool
	}{
		{"user tool off by default", user, nil, nil, adapter.Claude, repo, false},
		{"user tool claiming default on", tool("demo", "global", false, true, all...), nil, nil, adapter.Claude, repo, false},
		{"built-in default on", tool("b", "global", true, true, all...), nil, nil, adapter.Claude, repo, true},
		{"on everywhere", user, []store.ToolSwitch{sw("demo", "", "", true)}, nil, adapter.Grok, repo, true},
		{"on for claude only", user, []store.ToolSwitch{sw("demo", "claude", "", true)}, nil, adapter.Grok, repo, false},
		{"repo off beats global on", user, []store.ToolSwitch{sw("demo", "", "", true), sw("demo", "", repo, false)}, nil, adapter.Codex, repo, false},
		{"repo on beats global off", user, []store.ToolSwitch{sw("demo", "", "", false), sw("demo", "", repo, true)}, nil, adapter.Codex, repo, true},
		{"vendor in repo beats repo", user, []store.ToolSwitch{sw("demo", "", repo, true), sw("demo", "codex", repo, false)}, nil, adapter.Codex, repo, false},
		{"another repo's on", user, []store.ToolSwitch{sw("demo", "", "/r/other", true)}, nil, adapter.Codex, repo, false},
		// A repo's tools_off wins over every setting of the person's.
		{"tools_off beats repo on", user, []store.ToolSwitch{sw("demo", "claude", repo, true)}, []string{"demo"}, adapter.Claude, repo, false},
		{"tools_off beats default", tool("b", "global", true, true, all...), nil, []string{"b"}, adapter.Claude, repo, false},
		{"no part for the vendor", claudeOnly, []store.ToolSwitch{sw("demo", "", "", true)}, nil, adapter.Grok, repo, false},
		{"per-repo tool outside a repo", perRepo, []store.ToolSwitch{sw("idx", "", "", true)}, nil, adapter.Claude, "", false},
		{"per-repo tool takes no global on", perRepo, []store.ToolSwitch{sw("idx", "", "", true)}, nil, adapter.Claude, repo, false},
		{"per-repo tool on in its repo", perRepo, []store.ToolSwitch{sw("idx", "", repo, true)}, nil, adapter.Claude, repo, true},
		{"per-repo tool global off", perRepo, []store.ToolSwitch{sw("idx", "", repo, true), sw("idx", "claude", "", false)}, nil, adapter.Claude, repo, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := Board{Tools: []tooldef.Tool{c.t}, Switches: c.sws, RepoOff: c.repoOff}
			st := b.State(c.t, c.v, c.repo)
			if st.On != c.on || st.Why == "" {
				t.Fatalf("On = %v (%s), want %v", st.On, st.Why, c.on)
			}
			// An off tool contributes nothing, and On never lists it.
			if !st.On && !st.Parts.Empty() {
				t.Error("an off tool has parts")
			}
			if got := len(b.On(c.v, c.repo)); got != map[bool]int{true: 1, false: 0}[c.on] {
				t.Errorf("On lists %d tools", got)
			}
		})
	}
}
