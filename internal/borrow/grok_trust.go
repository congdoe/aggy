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
// The live check is test/smoke/smoke.sh (grok-review-untrusted): a
// scratch repo whose .claude/settings.json and .grok/hooks hold
// SessionStart and PreToolUse hooks that touch a marker file, the folder
// never trusted, run with aggy's exact Grok review argv; the marker must
// stay absent. Not yet recorded in docs/verification.md.

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

// grokInstructionSources are project instructions and skills; folder
// trust gates them too (folder_trust.rs:539-546). They no longer refuse a
// Grok review (C17-10, relaxed by T6742), and grokProjectSources does not
// look for them; TrustMarker still keeps an edit diff off them, and one
// that is or holds a symlink still refuses a review (grokInstructionLinks).
//
// Why that is safe now: they run no code, and their only risk was that
// Grok loads them as system-level text while a review keeps read_file,
// which takes an absolute or ~ path as is (grok-build 2bdd1d6a,
// xai-grok-tools/src/types/resources.rs:404-430, resolve_model_path;
// implementations/grok_build/read_file/mod.rs:135, :419), so a hostile
// repo could steer the reviewer to read ~/.ssh or another CLI's auth
// file and echo it. A review now runs under --sandbox strict, which
// refused a read_file of a canary under $HOME and still read the
// workspace (docs/verification.md S2), plus aggy's ~/Library denies (S4)
// and, on darwin, the /System/Volumes/Data firmlink denies (S5), all
// live-verified on grok 1.0.70 (2026-10-28), and a deny of Grok's own
// home (S8, inferred from S4 and S5). That confinement does not depend on
// who asks for the read. What strict leaves readable (system dirs, the
// temp dirs; docs/verification.md after the S table) was never closed by
// this gate either: the diff under review is
// attacker text too and can ask for the same reads in any repo. The
// names: INSTRUCTION_FILENAMES
// and agent_filenames (xai-grok-config/src/compat.rs:311-318, :431-438),
// rules dirs (:418-427), checked by has_project_instruction_markers_in
// (xai-grok-agent/src/prompt/agents_md.rs:213-223); skill dirs are
// skills/ or commands/ under each skill config dir (compat.rs:405-414,
// xai-grok-tools/src/implementations/skills/discovery.rs:26-27), checked
// by has_project_skill_dirs_in (prompt/skills.rs:51-64).
var grokInstructionSources = []string{
	"AGENTS.md", "CLAUDE.md", "CLAUDE.local.md", "AGENT.md", "Agents.md", "Claude.md",
	".claude/CLAUDE.md", ".claude/CLAUDE.local.md",
	".grok/rules", ".claude/rules", ".cursor/rules",
	".grok/skills", ".grok/commands", ".agents/skills", ".agents/commands",
	".claude/skills", ".claude/commands", ".cursor/skills", ".cursor/commands",
}

// present reports whether p exists in any form, or cannot be checked.
func present(p string) bool {
	_, err := safefs.Lstat(p)
	return !errors.Is(err, fs.ErrNotExist)
}

// grokChain returns the dirs from cwd up to its repo root, cwd first, and
// that root; with no repo root, cwd alone.
func grokChain(cwd string) (chain []string, root string) {
	cwd = filepath.Clean(cwd)
	for d := cwd; ; d = filepath.Dir(d) {
		chain = append(chain, d)
		if present(filepath.Join(d, ".git")) {
			return chain, d
		}
		if filepath.Dir(d) == d {
			return []string{cwd}, cwd
		}
	}
}

// relFound adds p, relative to cwd, to found unless it is there already.
func relFound(found []string, cwd, p string) []string {
	r, err := filepath.Rel(cwd, p)
	if err != nil {
		r = p
	}
	for _, f := range found {
		// A case-insensitive disk finds CLAUDE.md as Claude.md too.
		if strings.EqualFold(f, r) {
			return found
		}
	}
	return append(found, r)
}

// grokProjectSources returns the code-running config Grok would load
// with cwd as its working dir, relative to cwd. Instructions and skills
// (grokInstructionSources) are not counted: see there.
func grokProjectSources(cwd string) []string {
	cwd = filepath.Clean(cwd)
	chain, root := grokChain(cwd)
	var found []string
	add := func(dir, rel string) {
		if p := filepath.Join(dir, filepath.FromSlash(rel)); present(p) {
			found = relFound(found, cwd, p)
		}
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

// maxInstructionEntries caps the walk of the instruction and skill dirs;
// a bigger tree refuses the review rather than go unchecked.
const maxInstructionEntries = 10000

// grokInstructionLinks returns, relative to cwd, each instruction, rules,
// or skill path Grok would load (grokInstructionSources, in every dir from
// cwd to the repo root) that is a symlink, has a symlink on its way down
// from that dir (a linked .claude), holds one anywhere below it, is not a
// regular file or dir, or cannot be checked (N54-2). Grok follows a link
// there and loads the target as instructions with no permission check
// (xai-grok-agent/src/prompt/agents_md.rs:46-79, grok-src 559751fd), so a
// committed AGENTS.md -> ../../.grok/auth.json would put the token in the
// prompt; the sandbox allows that read, and the deny rules do not see it.
// Plain files and dirs inside the repo pass: with no link on the way they
// cannot resolve outside it.
func grokInstructionLinks(cwd string) []string {
	cwd = filepath.Clean(cwd)
	chain, _ := grokChain(cwd)
	var found []string
	entries := 0
	var walk func(dir string) (string, bool)
	walk = func(dir string) (string, bool) {
		list, err := safefs.ReadDir(dir)
		if err != nil {
			return dir, false
		}
		for _, e := range list {
			p := filepath.Join(dir, e.Name())
			if entries++; entries > maxInstructionEntries {
				return dir, false
			}
			switch t := e.Type(); {
			case t&fs.ModeSymlink != 0:
				return p, false
			case t.IsDir():
				if bad, ok := walk(p); !ok {
					return bad, false
				}
			case !t.IsRegular():
				return p, false
			}
		}
		return "", true
	}
	for _, d := range chain {
	sources:
		for _, s := range grokInstructionSources {
			p := d
			var fi fs.FileInfo
			for _, part := range strings.Split(s, "/") {
				p = filepath.Join(p, part)
				var err error
				if fi, err = safefs.Lstat(p); errors.Is(err, fs.ErrNotExist) {
					continue sources
				} else if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
					found = relFound(found, cwd, p)
					continue sources
				}
			}
			switch {
			case fi.IsDir():
				if bad, ok := walk(p); !ok {
					found = relFound(found, cwd, bad)
				}
			case !fi.Mode().IsRegular():
				found = relFound(found, cwd, p)
			}
		}
	}
	return found
}

// grokTrustGate refuses a Grok borrow that runs in a repo holding config
// Grok would load and run: hooks, MCP, LSP, plugins, agents, workflows,
// .envrc. The sandbox does not cover these: a hook is code Grok runs. It
// also refuses one whose instruction or skill files are symlinks
// (grokInstructionLinks), or whose committed .cursor paths hold one
// (Request.CursorLinks); plain ones pass.
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
	if found := grokInstructionLinks(req.Repo); len(found) > 0 {
		return fmt.Errorf("grok: %s: %w: instruction or skill files Grok would load are, or hold, a symlink or something that cannot be checked (%s), and Grok would load the link's target as instructions",
			req.Kind, adapter.ErrUnsupported, strings.Join(found, ", "))
	}
	// Grok's cursor rules (a feature off by default) load .md/.mdc files
	// from <dir>/.cursor/rules of any dir it reads in, following a linked
	// .cursor or rules dir (grok-build 2bdd1d6a,
	// xai-grok-tools/src/implementations/cursor_rules_on_read.rs:235-241,
	// tokio::fs::metadata); grokInstructionLinks only checks the cwd's
	// chain, so a nested sub/.cursor link is caught here (T6751).
	if len(req.CursorLinks) > 0 {
		return fmt.Errorf("grok: %s: %w: the repo commits a symlink under a .cursor path (%s), and Grok's cursor rules would follow it",
			req.Kind, adapter.ErrUnsupported, strings.Join(req.CursorLinks, ", "))
	}
	return nil
}

// TrustMarker reports whether the repo-relative, slash-separated path
// rel is, or is inside, one of the sources above (code-running config,
// hooks, and instructions a CLI loads from a repo) at any depth,
// compared case-insensitively. An edit's diff that touches one is not
// applied: it would change what the next session in the repo runs or is
// told.
func TrustMarker(rel string) bool {
	parts := strings.Split(rel, "/")
	for i := range parts {
		sub := strings.ToLower(strings.Join(parts[i:], "/"))
		for _, list := range [][]string{grokChainSources, grokCwdSources, grokRootSources, grokInstructionSources} {
			for _, s := range list {
				s = strings.ToLower(s)
				if sub == s || strings.HasPrefix(sub, s+"/") {
					return true
				}
			}
		}
	}
	return false
}
