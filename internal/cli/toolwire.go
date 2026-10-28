package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/tooldef"
	"github.com/congdoe/aggy/internal/toolwire"
)

// Files of the launch's private folder that hold the tools.
const (
	toolMCPFile   = "mcp.json" // Claude's --mcp-config
	toolAgentFile = "agent.md" // Grok's --agent
)


// addTools wires the tools that are on for vendor v in the repo holding
// cwd (switchboard.Board.On) into req, at launch and for this session
// only: their MCP servers, next to the user's own. dir is the launch's
// private folder under aggy's state dir, made here when missing; it
// holds Claude's and Grok's file and is the servers' working directory,
// never the repo (S63-14). session names the tools turned on for this
// session only (--tool). The user's vendor config is never written.
//
// Warnings say what was not wired and why; none stops the launch. With
// no tool on, req gets nothing: no flag and no file.
func addTools(ctx context.Context, v adapter.Vendor, caps []string, cwd, dir string, session []string, req *adapter.InteractiveRequest) []string {
	refuse := func(tool string, code diag.Code, why string) string {
		attrs := map[string]any{}
		if tool != "" {
			attrs["tool"] = tool
		}
		dlog.Log(ctx, diag.Rec{Event: diag.EvToolWireRefused, Vendor: string(v), Code: code, Msg: registryText(code), Attrs: attrs})
		if tool == "" {
			return fmt.Sprintf("tools not wired: %s (%s)", why, code)
		}
		return fmt.Sprintf("tool %s not wired: %s (%s)", tool, why, code)
	}
	repo, _ := toolRepo(cwd, false)
	b, _, _, err := toolBoard(ctx)
	if err != nil {
		return []string{refuse("", diag.CodeToolWireFailed, "the tool settings could not be read: "+err.Error())}
	}
	b.Session = session
	var warns []string
	for _, n := range session {
		if !slices.ContainsFunc(b.Tools, func(t tooldef.Tool) bool { return t.Name == n }) {
			warns = append(warns, refuse(n, diag.CodeToolUnknown, "no tool of that name is loaded (aggy tool list)"))
		}
	}
	if repo.root != "" {
		if b.RepoOff, err = repoToolsOff(repo); err != nil {
			// As aggy tool status shows it: every tool off in this repo.
			return []string{refuse("", diag.CodeToolWireFailed, "the repo's config cannot be read, so every tool is off here: "+err.Error())}
		}
	}
	on := b.On(v, repo.key)
	warns = append(warns, addHooks(v, caps, on, req, refuse)...)
	if !slices.ContainsFunc(on, func(st switchboard.State) bool { return st.Parts.MCP != nil }) {
		return warns
	}
	if v == adapter.Codex && !slices.Contains(caps, probe.CapConfigOverride) {
		return append(warns, refuse("", diag.CodeToolNoFlag, "codex --help does not list -c"))
	}
	taken, err := takenMCPNames(v, cwd, repo.root)
	if err != nil {
		return append(warns, refuse("", diag.CodeToolWireFailed, "the CLI's own MCP servers could not be listed to check names: "+err.Error()))
	}
	servers, refused := toolwire.MCP(on, repo.root, dir, taken)
	for _, r := range refused {
		warns = append(warns, refuse(r.Tool, r.Code, r.Why))
	}
	if len(servers) == 0 {
		return warns
	}
	if err := safefs.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return append(warns, refuse("", diag.CodeToolWireFailed, err.Error()))
	}
	write := func(name string, data []byte, err error) (string, error) {
		if err != nil {
			return "", err
		}
		f := filepath.Join(dir, name)
		return f, safefs.WriteFile(f, data, 0o600)
	}
	switch v {
	case adapter.Claude:
		data, err := toolwire.ClaudeConfig(servers)
		req.MCPConfigFile, err = write(toolMCPFile, data, err)
		if err != nil {
			req.MCPConfigFile = ""
			return append(warns, refuse("", diag.CodeToolWireFailed, err.Error()))
		}
	case adapter.Grok:
		data, err := toolwire.GrokAgent(servers)
		req.AgentFile, err = write(toolAgentFile, data, err)
		if err != nil {
			req.AgentFile = ""
			return append(warns, refuse("", diag.CodeToolWireFailed, err.Error()))
		}
	case adapter.Codex:
		req.MCP = servers
	}
	dlog.Log(ctx, diag.Rec{Event: diag.EvToolWired, Vendor: string(v), Attrs: map[string]any{"tools": toolwire.Names(servers)}})
	return warns
}

// toolHookBin is the aggy path a tool's hook entry calls; a test swaps
// it, since a test binary is a go build.
var toolHookBin = aggyBin

// addHooks puts the hooks of the tools in on into req, for Claude's
// inline --settings layer: each runs aggy hook-tool, which never passes
// an "allow" on (toolwire.Hooks). A hook that approves, and any hook
// for Codex or Grok, is refused.
func addHooks(v adapter.Vendor, caps []string, on []switchboard.State, req *adapter.InteractiveRequest, refuse func(string, diag.Code, string) string) []string {
	if !slices.ContainsFunc(on, func(st switchboard.State) bool { return len(st.Parts.Hooks) > 0 }) {
		return nil
	}
	bin := ""
	if v == adapter.Claude {
		if !slices.Contains(caps, probe.CapSettings) {
			return []string{refuse("", diag.CodeToolNoFlag, "claude --help does not list --settings, which carries the hooks")}
		}
		var err error
		if bin, err = toolHookBin(); err != nil {
			return []string{refuse("", diag.CodeToolWireFailed, "aggy's own path cannot go in a hook command: "+err.Error())}
		}
	}
	hooks, refused := toolwire.Hooks(on, v, bin)
	var warns []string
	for _, r := range refused {
		warns = append(warns, refuse(r.Tool, r.Code, r.Why))
	}
	req.Hooks = hooks
	return warns
}


// takenMCPNames are the MCP servers vendor v already loads by name, for
// a session in cwd under the repo at root: its user config
// (parity.MCPNames: for Grok also ~/.claude.json's unless that is off;
// for Claude also its local scope for the repo) and its project config
// (parity.ProjectMCPNames). Any that cannot be read is an error, so
// nothing is wired.
func takenMCPNames(v adapter.Vendor, cwd, root string) ([]string, error) {
	names, err := parity.MCPNames(v, root, os.Getenv)
	if err != nil {
		return nil, err
	}
	more, err := parity.ProjectMCPNames(v, cwd, root)
	return append(names, more...), err
}
