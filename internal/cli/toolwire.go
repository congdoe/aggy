package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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

// maxProjectMCP caps a repo's .mcp.json read for its server names.
const maxProjectMCP = 1 << 20

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
		dlog.Log(ctx, diag.Rec{Event: diag.EvToolWireRefused, Vendor: string(v), Code: code, Msg: why, Attrs: attrs})
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
	if !slices.ContainsFunc(on, func(st switchboard.State) bool { return st.Parts.MCP != nil }) {
		return warns
	}
	if v == adapter.Codex && !slices.Contains(caps, probe.CapConfigOverride) {
		return append(warns, refuse("", diag.CodeToolNoFlag, "codex --help does not list -c"))
	}
	taken, err := takenMCPNames(v, repo.root)
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

// takenMCPNames are the MCP servers vendor v already loads by name: its
// user config (parity.MCPNames: for Grok also ~/.claude.json's unless
// that is off; for Claude also its local scope for the repo) and, for
// Claude, the repo's .mcp.json.
// TODO(T6708): Codex's and Grok's project config layers are not read;
// unverified whether they may hold MCP servers.
func takenMCPNames(v adapter.Vendor, root string) ([]string, error) {
	names, err := parity.MCPNames(v, root, os.Getenv)
	if err != nil || v != adapter.Claude || root == "" {
		return names, err
	}
	b, err := safefs.ReadFileLimit(filepath.Join(root, ".mcp.json"), maxProjectMCP)
	if errors.Is(err, fs.ErrNotExist) {
		return names, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("the repo's .mcp.json: %w", err)
	}
	return append(names, slices.Collect(maps.Keys(doc.MCPServers))...), nil
}
