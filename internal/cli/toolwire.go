package cli

import (
	"context"
	"encoding/json"
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
	"github.com/congdoe/aggy/internal/toolwire"
)

// Files of the launch's private folder that hold the tools.
const (
	toolMCPFile   = "mcp.json" // Claude's --mcp-config
	toolAgentFile = "agent.md" // Grok's --agent
)

// maxProjectMCP caps a .mcp.json read for its server names.
const maxProjectMCP = 1 << 20

// grokToolsVerified gates every Grok tool wiring (W71-4): aggy tool on
// --vendor grok and the launch wiring refuse with tool.vendor_unverified
// while it is false. The --agent code stays behind it.
// TODO(T6708): unverified for Grok's --agent: what the agent replaces
// besides mcpServers ([agent] settings, the system prompt, the model);
// whether a resumed session keeps or drops it; and which project MCP
// sources Grok also reads (.mcp.json and .grok/config.toml from cwd up
// to the git root, .cursor/mcp.json, .grok/agents), which the clash
// check would have to read.
var grokToolsVerified = false

// grokUnverified is why Grok gets no tools while grokToolsVerified is off.
const grokUnverified = "tools are not wired into Grok yet: what its --agent flag changes besides the MCP servers is not verified (docs/design.md, Launch wiring)"

// addTools wires the tools that are on for vendor v in the repo holding
// cwd (switchboard.Board.On) into req, at launch and for this session
// only: their MCP servers, next to the user's own, each started through
// aggy tool exec (toolwire.MCP). dir is the launch's private folder
// under aggy's state dir, made here when missing and checked to be
// aggy's own (0700); it holds Claude's file and is where the servers
// run, never the repo (S63-14). session names the tools turned on for
// this session only (--tool). The user's vendor config is never written.
//
// Warnings say what was not wired and why; none stops the launch. With
// no tool on, req gets nothing: no flag and no file.
func addTools(ctx context.Context, v adapter.Vendor, caps []string, cwd, dir string, session []string, req *adapter.InteractiveRequest) []string {
	// The record's msg is the code's registry text (W71-11); class says
	// which step refused, and the warning carries the detail.
	refuse := func(tool string, code diag.Code, class, why string) string {
		attrs := map[string]any{"class": class}
		if tool != "" {
			attrs["tool"] = tool
		}
		dlog.Log(ctx, diag.Rec{Event: diag.EvToolWireRefused, Vendor: string(v), Code: code, Msg: fixedMsg(code), Attrs: attrs})
		if tool == "" {
			return fmt.Sprintf("tools not wired: %s (%s)", why, code)
		}
		return fmt.Sprintf("tool %s not wired: %s (%s)", tool, why, code)
	}
	failed := func(class string, err error) []string {
		return []string{refuse("", diag.CodeToolWireFailed, class, err.Error())}
	}
	repo, _ := toolRepo(cwd, false)
	b, _, _, err := toolBoard(ctx)
	if err != nil {
		return failed("settings", fmt.Errorf("the tool settings could not be read: %w", err))
	}
	b.Session = session
	if repo.root != "" {
		if b.RepoOff, err = repoToolsOff(repo); err != nil {
			// As aggy tool status shows it: every tool off in this repo.
			return failed("repo_config", fmt.Errorf("the repo's config cannot be read, so every tool is off here: %w", err))
		}
	}
	on := b.On(v, repo.key)
	if !slices.ContainsFunc(on, func(st switchboard.State) bool { return st.Parts.MCP != nil }) {
		return nil
	}
	switch {
	case v == adapter.Grok && !grokToolsVerified:
		return []string{refuse("", diag.CodeToolUnverified, "unverified", grokUnverified)}
	case v == adapter.Codex && !slices.Contains(caps, probe.CapConfigOverride):
		return []string{refuse("", diag.CodeToolNoFlag, "no_flag", "codex --help does not list -c")}
	}
	taken, err := takenMCPNames(v, cwd, repo.root)
	if err != nil {
		// Fail closed: a name aggy cannot check is never wired over.
		return failed("names", fmt.Errorf("the CLI's own MCP servers could not be listed to check names: %w", err))
	}
	bin, err := aggyBin()
	if err != nil {
		return failed("bin", err)
	}
	servers, refused := toolwire.MCP(on, repo.root, dir, bin, taken)
	var warns []string
	for _, r := range refused {
		warns = append(warns, refuse(r.Tool, r.Code, r.Code.String(), r.Why))
	}
	if len(servers) == 0 {
		return warns
	}
	// W71-10: the folder is aggy's own, 0700, and the file lands in the
	// folder checked, never through a symlink swapped in after.
	if err := safefs.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return append(warns, failed("mkdir", err)...)
	}
	dirFi, err := safefs.Lstat(dir)
	if err == nil && !ownPrivateDir(dirFi) {
		err = fmt.Errorf("%s is not a private (0700) directory of this user", dir)
	}
	if err != nil {
		return append(warns, failed("dir", err)...)
	}
	write := func(name string, data []byte, err error) (string, error) {
		if err != nil {
			return "", err
		}
		f := filepath.Join(dir, name)
		return f, safefs.WriteFileNoFollowIn(dirFi, f, data, 0o600, nil)
	}
	switch v {
	case adapter.Claude:
		data, err := toolwire.ClaudeConfig(servers)
		if req.MCPConfigFile, err = write(toolMCPFile, data, err); err != nil {
			req.MCPConfigFile = ""
			return append(warns, failed("write", err)...)
		}
	case adapter.Grok:
		data, err := toolwire.GrokAgent(servers)
		if req.AgentFile, err = write(toolAgentFile, data, err); err != nil {
			req.AgentFile = ""
			return append(warns, failed("write", err)...)
		}
	case adapter.Codex:
		req.MCP = servers
	}
	dlog.Log(ctx, diag.Rec{Event: diag.EvToolWired, Vendor: string(v), Attrs: map[string]any{"tools": toolwire.Names(servers)}})
	return warns
}

// takenMCPNames maps the MCP servers vendor v already loads by name to
// where each is set, for the name-clash check. Any file that cannot be
// read or parsed is an error: the check fails closed.
//
//   - Claude: ~/.claude.json's user scope and every project's local
//     scope (W71-5), and each .mcp.json from cwd up to / (W71-5).
//     TODO(T6708): unverified whether Claude reads a .mcp.json above
//     the git root, so every one up to / counts; managed-mcp.json and
//     plugin servers are not read.
//   - Codex: every config layer (codexConfigs: /etc/codex, the user's,
//     a selected profile, and each project .codex/config.toml from the
//     project root down to cwd), servers with enabled = false and those
//     under [profiles.*] included (W71-3).
//   - Grok: user config and Claude compatibility (parity.MCPNames); its
//     wiring is off (grokToolsVerified).
func takenMCPNames(v adapter.Vendor, cwd, root string) (map[string]string, error) {
	out := map[string]string{}
	switch v {
	case adapter.Claude:
		m, err := parity.ClaudeMCPSources(os.Getenv)
		if err != nil {
			return nil, err
		}
		out = m
		for d := filepath.Clean(cwd); ; d = filepath.Dir(d) {
			f := filepath.Join(d, ".mcp.json")
			names, err := projectMCPNames(f)
			if err != nil {
				return nil, err
			}
			for _, n := range names {
				out[n] = f
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	case adapter.Codex:
		if root == "" {
			root = cwd // Codex's project root is cwd outside git
		}
		layers, err := codexConfigs(cwd, root)
		if err != nil {
			return nil, err
		}
		for _, l := range layers {
			if l.Data == nil {
				continue
			}
			names, err := parity.CodexMCPNames(l.Data)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", l.Path, err)
			}
			for _, n := range names {
				out[n] = l.Path
			}
		}
	default:
		names, err := parity.MCPNames(v, root, os.Getenv)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			out[n] = string(v) + "'s config"
		}
	}
	return out, nil
}

// projectMCPNames are the server names of one .mcp.json; none when it
// is absent.
func projectMCPNames(f string) ([]string, error) {
	b, err := safefs.ReadFileLimit(f, maxProjectMCP)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", f, err)
	}
	names := make([]string, 0, len(doc.MCPServers))
	for n := range doc.MCPServers {
		names = append(names, n)
	}
	return names, nil
}
