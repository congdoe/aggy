// Package switchboard decides which managed tools are on, for one vendor
// in one repo, from three inputs only: the tool definitions
// (internal/tooldef), the person's aggy tool on and off settings (the
// index's tool_switches), and the repo's tools_off. A repo's config can
// only turn a tool off. A user tool is off until the person turns it on.
//
// An off tool contributes nothing: On never returns it, so no MCP entry,
// skill link, hook, init step, or line of text of it reaches any CLI, and
// none of its commands runs. Wiring (T6708) is built from On alone.
package switchboard

import (
	"slices"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/tooldef"
)

// Board holds the inputs.
type Board struct {
	Tools    []tooldef.Tool
	Switches []store.ToolSwitch
	// RepoOff is the tools_off of the repo the session runs in.
	RepoOff []string
}

// State is whether a tool is on for one vendor in one repo, and why.
type State struct {
	Tool   tooldef.Tool
	Vendor adapter.Vendor
	On     bool
	Why    string
	// Parts are the tool's parts for the vendor: empty unless On.
	Parts tooldef.Parts
}

// State decides tool t for vendor v in repo, the real path of the repo's
// work-tree root ("" outside a repo).
func (b Board) State(t tooldef.Tool, v adapter.Vendor, repo string) State {
	st := State{Tool: t, Vendor: v}
	off := func(why string) State { st.Why = why; return st }
	parts := t.PartsFor(v)
	switch {
	case parts.Empty():
		return off("nothing for " + string(v))
	case repo != "" && slices.Contains(b.RepoOff, t.Name):
		return off("off by this repo's tools_off")
	case t.Scope == "repo" && repo == "":
		return off("a per-repo tool, outside a repo")
	}
	on, why, set := b.setting(t, v, repo)
	if !set {
		// User tools are always off by default; the validator also
		// refuses default = true on one.
		on, why = t.Default && t.BuiltIn && t.Scope == "global", "off by default"
		if on {
			why = "on by default"
		}
	}
	if !on {
		return off(why)
	}
	st.On, st.Why, st.Parts = true, why, parts
	return st
}

// setting finds the most specific switch for the tool: this repo and
// vendor, this repo, every repo and this vendor, then every repo. A
// per-repo tool takes no "on" from a setting for every repo.
func (b Board) setting(t tooldef.Tool, v adapter.Vendor, repo string) (on bool, why string, ok bool) {
	type key struct{ repo, vendor, why string }
	keys := []key{{"", "", "set for every vendor and repo"}, {"", string(v), "set for " + string(v)}}
	if repo != "" {
		keys = append(keys, key{repo, "", "set for this repo"}, key{repo, string(v), "set for " + string(v) + " in this repo"})
	}
	for i := len(keys) - 1; i >= 0; i-- {
		k := keys[i]
		j := slices.IndexFunc(b.Switches, func(s store.ToolSwitch) bool {
			return s.Tool == t.Name && s.Repo == k.repo && s.Vendor == k.vendor
		})
		if j < 0 {
			continue
		}
		on := b.Switches[j].On
		if on && t.Scope == "repo" && k.repo == "" {
			continue
		}
		state := "off"
		if on {
			state = "on"
		}
		return on, state + ": " + k.why, true
	}
	return false, "", false
}

// On returns the tools on for vendor v in repo, by name. Wiring is
// built from this list alone.
func (b Board) On(v adapter.Vendor, repo string) []State {
	var out []State
	for _, t := range b.Tools {
		if st := b.State(t, v, repo); st.On {
			out = append(out, st)
		}
	}
	return out
}
