package switchboard

import (
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/tooldef"
)

func tool(name, scope string, builtIn, def bool, vendors ...adapter.Vendor) tooldef.Tool {
	return tooldef.Tool{BuiltIn: builtIn, Digest: "d1", Def: tooldef.Def{Name: name, Scope: scope, Default: def,
		MCP: []tooldef.MCP{{Vendors: vendors, Argv: []string{name, "serve"}}}}}
}

func sw(tool, vendor, repo string, on bool) store.ToolSwitch {
	return store.ToolSwitch{Tool: tool, Vendor: vendor, Repo: repo, On: on, Digest: "d1"}
}

// changed is a setting made for an earlier version of the definition.
func changed(s store.ToolSwitch) store.ToolSwitch { s.Digest = "d0"; return s }

// builtIn is a setting made for a built-in.
func builtIn(s store.ToolSwitch) store.ToolSwitch { s.Digest = BuiltInDigest; return s }

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
		session []string
	}{
		// S63-1: an edited or replaced user file is off until turned on again.
		{"user file changed", user, []store.ToolSwitch{changed(sw("demo", "", "", true))}, nil, adapter.Claude, repo, false, nil},
		{"built-in updated", tool("b", "global", true, false, all...), []store.ToolSwitch{builtIn(sw("b", "", "", true))}, nil, adapter.Claude, repo, true, nil},
		{"built-in, dev row with its digest", tool("b", "global", true, false, all...), []store.ToolSwitch{sw("b", "", "", true)}, nil, adapter.Claude, repo, true, nil},
		// S63-18: a user tool's on never turns on a later built-in of its name.
		{"user on, name now built-in", tool("b", "global", true, false, all...), []store.ToolSwitch{changed(sw("b", "", "", true))}, nil, adapter.Claude, repo, false, nil},
		// S63-10: one session's --tool, under the repo's rules.
		{"session on", user, nil, nil, adapter.Claude, repo, true, []string{"demo"}},
		{"session beats a setting off", user, []store.ToolSwitch{sw("demo", "", "", false)}, nil, adapter.Claude, repo, true, []string{"demo"}},
		{"tools_off beats session", user, nil, []string{"demo"}, adapter.Claude, repo, false, []string{"demo"}},
		{"per-repo tool outside a repo, session", perRepo, nil, nil, adapter.Claude, "", false, []string{"idx"}},
		{"session, no part for the vendor", claudeOnly, nil, nil, adapter.Grok, repo, false, []string{"demo"}},
		{"user tool off by default", user, nil, nil, adapter.Claude, repo, false, nil},
		{"user tool claiming default on", tool("demo", "global", false, true, all...), nil, nil, adapter.Claude, repo, false, nil},
		{"built-in default on", tool("b", "global", true, true, all...), nil, nil, adapter.Claude, repo, true, nil},
		{"on everywhere", user, []store.ToolSwitch{sw("demo", "", "", true)}, nil, adapter.Grok, repo, true, nil},
		{"on for claude only", user, []store.ToolSwitch{sw("demo", "claude", "", true)}, nil, adapter.Grok, repo, false, nil},
		{"repo off beats global on", user, []store.ToolSwitch{sw("demo", "", "", true), sw("demo", "", repo, false)}, nil, adapter.Codex, repo, false, nil},
		{"repo on beats global off", user, []store.ToolSwitch{sw("demo", "", "", false), sw("demo", "", repo, true)}, nil, adapter.Codex, repo, true, nil},
		{"vendor in repo beats repo", user, []store.ToolSwitch{sw("demo", "", repo, true), sw("demo", "codex", repo, false)}, nil, adapter.Codex, repo, false, nil},
		{"another repo's on", user, []store.ToolSwitch{sw("demo", "", "/r/other", true)}, nil, adapter.Codex, repo, false, nil},
		// A repo's tools_off wins over every setting of the person's.
		{"tools_off beats repo on", user, []store.ToolSwitch{sw("demo", "claude", repo, true)}, []string{"demo"}, adapter.Claude, repo, false, nil},
		{"tools_off beats default", tool("b", "global", true, true, all...), nil, []string{"b"}, adapter.Claude, repo, false, nil},
		{"no part for the vendor", claudeOnly, []store.ToolSwitch{sw("demo", "", "", true)}, nil, adapter.Grok, repo, false, nil},
		{"per-repo tool outside a repo", perRepo, []store.ToolSwitch{sw("idx", "", "", true)}, nil, adapter.Claude, "", false, nil},
		{"per-repo tool takes no global on", perRepo, []store.ToolSwitch{sw("idx", "", "", true)}, nil, adapter.Claude, repo, false, nil},
		{"per-repo tool on in its repo", perRepo, []store.ToolSwitch{sw("idx", "", repo, true)}, nil, adapter.Claude, repo, true, nil},
		{"per-repo tool global off", perRepo, []store.ToolSwitch{sw("idx", "", repo, true), sw("idx", "claude", "", false)}, nil, adapter.Claude, repo, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := Board{Tools: []tooldef.Tool{c.t}, Switches: c.sws, RepoOff: c.repoOff, Session: c.session}
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

// TestStateProxy (T6780): a proxy runs only on an "on" for one named
// vendor that holds the typed confirmation, and repo scope stacks on top.
func TestStateProxy(t *testing.T) {
	px := tool("px", "global", true, false, adapter.Claude, adapter.Codex)
	px.Proxy = &tooldef.Proxy{Vendors: []adapter.Vendor{adapter.Claude}, Argv: []string{"px"}, Listen: "127.0.0.1:8787", Health: "/h",
		Env: map[adapter.Vendor]string{adapter.Claude: "ANTHROPIC_BASE_URL"}}
	ok := func(s store.ToolSwitch) store.ToolSwitch {
		s.ProxyDigest = tooldef.ProxyDigest(*px.Proxy, adapter.Vendor(s.Vendor))
		return builtIn(s)
	}
	for _, c := range []struct {
		name    string
		sws     []store.ToolSwitch
		session []string
		v       adapter.Vendor
		repo    string
		on, prx bool
	}{
		{"confirmed for claude", []store.ToolSwitch{ok(sw("px", "claude", "", true))}, nil, adapter.Claude, repo, true, true},
		{"unconfirmed on", []store.ToolSwitch{builtIn(sw("px", "claude", "", true))}, nil, adapter.Claude, repo, true, false},
		{"on for every vendor", []store.ToolSwitch{ok(sw("px", "", "", true))}, nil, adapter.Claude, repo, true, false},
		{"session only", nil, []string{"px"}, adapter.Claude, repo, true, false},
		{"session with a confirmed row", []store.ToolSwitch{ok(sw("px", "claude", "", true))}, []string{"px"}, adapter.Claude, repo, true, true},
		{"unsupported vendor", []store.ToolSwitch{ok(sw("px", "codex", "", true))}, nil, adapter.Codex, repo, true, false},
		{"confirmed in this repo", []store.ToolSwitch{ok(sw("px", "claude", repo, true))}, nil, adapter.Claude, repo, true, true},
		{"confirmed in another repo", []store.ToolSwitch{ok(sw("px", "claude", "/r/other", true))}, nil, adapter.Claude, repo, false, false},
		{"repo off beats confirmed", []store.ToolSwitch{ok(sw("px", "claude", "", true)), sw("px", "claude", repo, false)}, nil, adapter.Claude, repo, false, false},
		{"repo on, unconfirmed, beats confirmed", []store.ToolSwitch{ok(sw("px", "claude", "", true)), builtIn(sw("px", "", repo, true))}, nil, adapter.Claude, repo, true, false},
		{"default never", nil, nil, adapter.Claude, repo, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := Board{Tools: []tooldef.Tool{px}, Switches: c.sws, Session: c.session}
			st := b.State(px, c.v, c.repo)
			if st.On != c.on || (st.Parts.Proxy != nil) != c.prx {
				t.Errorf("on %v proxy %v (%s), want %v %v", st.On, st.Parts.Proxy != nil, st.Why, c.on, c.prx)
			}
		})
	}
	// HIGH-1 (consent): a confirmation is of the proxy as it was; a new
	// argv, port, or health path in a release asks again.
	for _, edit := range []func(*tooldef.Proxy){
		func(p *tooldef.Proxy) { p.Argv = []string{"px", "--evil"} },
		func(p *tooldef.Proxy) { p.Listen = "127.0.0.1:9999" },
		func(p *tooldef.Proxy) { p.Health = "/other" },
	} {
		newer := px
		np := *px.Proxy
		np.Argv = slices.Clone(np.Argv)
		edit(&np)
		newer.Proxy = &np
		st := (Board{Switches: []store.ToolSwitch{ok(sw("px", "claude", "", true))}}).State(newer, adapter.Claude, "")
		if !st.On || st.Parts.Proxy != nil || !st.ProxyChanged {
			t.Errorf("%+v: on %v proxy %v changed %v", np, st.On, st.Parts.Proxy != nil, st.ProxyChanged)
		}
	}
	if st := (Board{Switches: []store.ToolSwitch{ok(sw("px", "claude", "", true))}}).State(px, adapter.Claude, ""); st.ProxyChanged {
		t.Error("an unchanged proxy counts as changed")
	}
	user := px
	user.BuiltIn = false
	if st := (Board{Switches: []store.ToolSwitch{changed(ok(sw("px", "claude", "", true)))}}).State(user, adapter.Claude, ""); st.On || st.Parts.Proxy != nil {
		t.Error("a changed user file keeps its proxy")
	}
}
