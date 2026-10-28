package borrow

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// Grok loads hooks and other code-running config from the repo it runs
// in whenever its folder trust says Trusted, and that is also the case
// when trust is off, the folder was once trusted interactively, or the
// key cannot be recorded (grok-build 2bdd1d6,
// xai-grok-permission-rules/src/folder_trust.rs:69-77). Grok 1.0.70's
// folder-trust behaviour is unverified, so aggy does not rely on it: a
// Grok borrow whose cwd is in a repo (review) is refused when any source
// below is present, and the borrow passes Grok over.
//
// TODO(T6649): live check on grok 1.0.70: a scratch repo whose
// .claude/settings.json and .grok/hooks hold SessionStart and PreToolUse
// hooks that touch a marker file, the folder never trusted, run with
// aggy's exact Grok review argv; the marker must stay absent.

// grokChainSources are checked in every dir from the cwd up to the git
// root: .mcp.json (project_config.rs:19-39), .grok/config.toml (:77-87),
// Claude settings (claude_settings.rs:354-384), and plugin and agent
// dirs (plugins/discovery.rs:249-251, agent discovery.rs:16,31).
var grokChainSources = []string{
	".mcp.json", ".grok/config.toml",
	".claude/settings.json", ".claude/settings.local.json",
	".grok/plugins", ".claude/plugins", ".grok/agents", ".claude/agents",
}

// grokCwdSources are checked in the cwd only: .grok/lsp.json,
// .cursor/mcp.json, .envrc (sourced by bash when direnv is missing,
// envrc.rs:147-188), roles, and personas
// (xai-grok-workspace/src/folder_trust.rs:488-501, 528-535).
var grokCwdSources = []string{".grok/lsp.json", ".cursor/mcp.json", ".envrc", ".grok/roles", ".grok/personas"}

// grokRootSources are checked at the git root: the project hook sources
// (xai-grok-hooks/src/discovery.rs:385-401) and workflows
// (folder_trust.rs:507-515, 536).
var grokRootSources = []string{
	".claude/settings.json", ".claude/settings.local.json", ".grok/hooks", ".cursor/hooks.json", ".grok/workflows",
}

// Not checked: AGENTS.md-style instructions and skill dirs, which
// folder trust also gates (folder_trust.rs:539-546). They are prompt
// text, which a review treats as untrusted data anyway, and nearly every
// repo has one; no review tool runs code.

// present reports whether p exists in any form, or cannot be checked.
func present(p string) bool {
	_, err := safefs.Lstat(p)
	return !errors.Is(err, fs.ErrNotExist)
}

// grokProjectSources returns the code-running config Grok would load
// with cwd as its working dir, relative to cwd.
func grokProjectSources(cwd string) []string {
	cwd = filepath.Clean(cwd)
	root := cwd
	var chain []string
	for d := cwd; ; d = filepath.Dir(d) {
		chain = append(chain, d)
		if present(filepath.Join(d, ".git")) {
			root = d
			break
		}
		if filepath.Dir(d) == d {
			chain = []string{cwd} // no git root: the cwd alone
			break
		}
	}
	var found []string
	add := func(dir, rel string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if !present(p) {
			return
		}
		r, err := filepath.Rel(cwd, p)
		if err != nil {
			r = p
		}
		for _, f := range found {
			if f == r {
				return
			}
		}
		found = append(found, r)
	}
	for _, d := range chain {
		for _, s := range grokChainSources {
			add(d, s)
		}
	}
	for _, s := range grokCwdSources {
		add(cwd, s)
	}
	for _, s := range grokRootSources {
		add(root, s)
	}
	return found
}

// grokTrustGate refuses a Grok borrow that runs in a repo holding config
// Grok would load and run.
func grokTrustGate(v adapter.Vendor, req Request) error {
	if v != adapter.Grok || req.Kind != adapter.Review {
		// Every other kind runs in a fresh empty dir outside any git
		// tree (makeDirs).
		return nil
	}
	if found := grokProjectSources(req.Repo); len(found) > 0 {
		return fmt.Errorf("grok: %s: %w: the repo holds config Grok may load and run (%s), and its folder trust is not relied on",
			req.Kind, adapter.ErrUnsupported, strings.Join(found, ", "))
	}
	return nil
}
