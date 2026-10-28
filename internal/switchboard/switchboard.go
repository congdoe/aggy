// Package switchboard decides which managed tools are on, for one vendor
// in one repo, from these inputs only: the tool definitions
// (internal/tooldef), the person's aggy tool on and off settings (the
// index's tool_switches), the tools turned on for one session (--tool),
// and the repo's tools_off. A repo's config can only turn a tool off. A
// user tool is off until the person turns it on, and off again once its
// definition changes.
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
	// Session names the tools turned on for one session only (aggy
	// switch or borrow --tool). It never beats RepoOff or the per-repo
	// scope rule.
	Session []string
	// RepoOff is the tools_off of the repo the session runs in: its main
	// checkout's and, in a linked worktree, the worktree's own.
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

// Changed is the reason a user tool turned on is off again.
const Changed = "off: the definition changed since it was turned on"

// WasUser is the reason a built-in is off although an "on" names it: the
// on was for a user tool of the same name.
const WasUser = "off: it was turned on for a user tool of this name; turn the built-in on to use it"

// BuiltInDigest is the digest an "on" for a built-in records: a
// built-in's own file changes with aggy's releases, so its on holds this
// marker rather than a file's sha256.
const BuiltInDigest = "built-in"

// State decides tool t for vendor v in repo, the key of the repo's main
// checkout ("" outside a repo).
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
	var on bool
	var why string
	if slices.Contains(b.Session, t.Name) {
		on, why = true, "on for this session"
	} else if sw, set := b.setting(t, v, repo); set {
		on, why = sw.On, "off: "+sw.why
		if on {
			why = "on: " + sw.why
			switch {
			case !t.BuiltIn && sw.digest != t.Digest:
				// A user file edited or replaced after it was turned on.
				on, why = false, Changed
			case t.BuiltIn && sw.digest != BuiltInDigest && sw.digest != t.Digest:
				// S63-18: turned on for a user tool of this name, before
				// a release made the name a built-in. (Rows a dev build
				// wrote for a built-in hold its digest at the time, and
				// go off once its file changes.)
				on, why = false, WasUser
			}
		}
	} else {
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

type setting struct {
	On          bool
	why, digest string
}

// setting finds the most specific switch for the tool: this repo and
// vendor, this repo, every repo and this vendor, then every repo. A
// per-repo tool takes no "on" from a setting for every repo.
func (b Board) setting(t tooldef.Tool, v adapter.Vendor, repo string) (setting, bool) {
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
		sw := b.Switches[j]
		if sw.On && t.Scope == "repo" && k.repo == "" {
			continue
		}
		return setting{sw.On, k.why, sw.Digest}, true
	}
	return setting{}, false
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
