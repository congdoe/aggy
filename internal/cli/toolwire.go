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
	"runtime"
	"slices"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/tooldef"
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
	var warns []string
	if repo.root != "" {
		if b.RepoOff, err = repoToolsOff(repo); err != nil {
			// As aggy tool status shows it: every tool off in this repo.
			return append(warns, failed("repo_config", fmt.Errorf("the repo's config cannot be read, so every tool is off here: %w", err))...)
		}
	}
	var sw []string
	b.Session, sw = sessionTools(ctx, b, v, repo.key, session, refuse)
	warns = append(warns, sw...)
	on := b.On(v, repo.key)
	// R80-3: a linked skill folder changed since its first link keeps its
	// tool's servers and hooks out of this launch.
	if changed := changedSkills(ctx, on); len(changed) > 0 {
		on = slices.DeleteFunc(on, func(st switchboard.State) bool {
			err, ok := changed[st.Tool.Name]
			if ok {
				warns = append(warns, refuse(st.Tool.Name, codeOf(err, diag.CodeToolSkillChanged), "skill_changed", err.Error()))
			}
			return ok
		})
	}
	warns = append(warns, addHooks(v, caps, on, req, refuse)...)
	if !slices.ContainsFunc(on, func(st switchboard.State) bool { return st.Parts.MCP != nil }) {
		return warns
	}
	switch {
	case v == adapter.Grok && !grokToolsVerified:
		return append(warns, refuse("", diag.CodeToolUnverified, "unverified", grokUnverified))
	case v == adapter.Codex && !slices.Contains(caps, probe.CapConfigOverride):
		return append(warns, refuse("", diag.CodeToolNoFlag, "no_flag", "codex --help does not list -c"))
	}
	taken, err := takenMCPNames(v, cwd, repo.root)
	if err != nil {
		// Fail closed: a name aggy cannot check is never wired over.
		return append(warns, failed("names", fmt.Errorf("the CLI's own MCP servers could not be listed to check names: %w", err))...)
	}
	bin, err := aggyBin()
	if err != nil {
		return append(warns, failed("bin", err)...)
	}
	servers, refused := toolwire.MCP(on, repo.root, dir, bin, taken)
	for _, r := range refused {
		warns = append(warns, refuse(r.Tool, r.Code, r.Code.String(), r.Why))
	}
	if len(servers) == 0 {
		return warns
	}
	// W71-10: the folder is aggy's own, 0700, and the file lands in the
	// folder checked, never through a symlink swapped in after.
	dirFi, class, err := launchDir(dir)
	if err != nil {
		return append(warns, failed(class, err)...)
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
	warns = append(warns, copyIndexes(ctx, on, toolwire.Names(servers), repo)...) // T6711, before the CLI starts
	dlog.Log(ctx, diag.Rec{Event: diag.EvToolWired, Vendor: string(v), Attrs: map[string]any{"tools": toolwire.Names(servers)}})
	return warns
}

// launchDir makes the launch's private folder when missing and checks
// it is aggy's own (0700, W71-10). class names the step that failed.
func launchDir(dir string) (fs.FileInfo, string, error) {
	if err := safefs.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, "mkdir", err
	}
	fi, err := safefs.Lstat(dir)
	if err == nil && !ownPrivateDir(fi) {
		err = fmt.Errorf("%s is not a private (0700) directory of this user", dir)
	}
	return fi, "dir", err
}

// sessionTools keeps the names of session (--tool) that may turn on for
// vendor v in repo, and warns for each one dropped or off:
//
//   - a name no loaded tool has;
//   - a user tool whose file is not the one aggy tool add showed, or that
//     aggy tool add never recorded (S63-16, W76-1): a session tool has
//     no switch row to carry a digest, so the check runs here, as aggy
//     tool on runs it;
//   - one the board keeps off: nothing for v, a per-repo tool outside a
//     repo, or the repo's tools_off;
//   - one with no MCP server or hook for v, which stays on but adds
//     nothing to the launch (W76-3).
//
// The index is opened once, read-only, and only for a user tool.
func sessionTools(ctx context.Context, b switchboard.Board, v adapter.Vendor, repo string, session []string,
	refuse func(tool string, code diag.Code, class, why string) string) ([]string, []string) {
	var keep, warns []string
	var s *store.Store
	var openErr error // R76-F2: every user tool is refused with it
	opened := false
	defer func() {
		if s != nil {
			_ = s.Close()
		}
	}()
	for _, n := range session {
		i := slices.IndexFunc(b.Tools, func(t tooldef.Tool) bool { return t.Name == n })
		if i < 0 {
			warns = append(warns, refuse(n, diag.CodeToolUnknown, "unknown", "no tool of that name is loaded (aggy tool list)"))
			continue
		}
		t := b.Tools[i]
		if !t.BuiltIn && !opened {
			opened = true
			var ver int
			if s, ver, openErr = openIndexReadOnly(ctx); s != nil && ver < toolAddedSchema {
				_ = s.Close()
				s = nil
			}
		}
		if !t.BuiltIn && openErr != nil {
			warns = append(warns, refuse(n, diag.CodeStore, "index", fmt.Sprintf("the index could not be read to check the file: %v", openErr)))
			continue
		}
		if err := checkAdded(ctx, s, t, true); err != nil {
			warns = append(warns, refuse(n, codeOf(err, diag.CodeToolChanged), "added", err.Error()))
			continue
		}
		keep = append(keep, n)
		b.Session = []string{n}
		st := b.State(t, v, repo)
		switch {
		case !st.On && slices.Contains(b.RepoOff, n):
			warns = append(warns, refuse(n, diag.CodeToolRepoOff, "repo_off", st.Why))
		case !st.On:
			warns = append(warns, refuse(n, diag.CodeToolScope, "off", st.Why))
		case st.Parts.MCP == nil && len(st.Parts.Hooks) == 0:
			warns = append(warns, refuse(n, diag.CodeToolScope, "no_mcp", "it has no MCP server or hook for "+string(v)+", and a session wires only those; a skill follows aggy tool on"))
		}
	}
	return keep, warns
}

// takenMCPNames maps the MCP servers vendor v already loads by name to
// where each is set, for the name-clash check. Any file that cannot be
// read or parsed is an error: the check fails closed.
//
//   - Claude: ~/.claude.json's user scope and every project's local
//     scope (W71-5), and each .mcp.json from cwd up to / (W71-5).
//     TODO(T6708): unverified whether Claude reads a .mcp.json above
//     the git root, so every one up to / counts. Also the managed
//     managed-mcp.json (claudeManagedMCP). TODO(T6708): plugin servers
//     are not read; and per Claude Code's docs a managed-mcp.json takes
//     exclusive control, so aggy's servers may then not load at all
//     (unverified).
//   - Codex: every config layer (codexConfigs: /etc/codex, the user's,
//     a selected profile, and each project .codex/config.toml from the
//     project root down to cwd), servers with enabled = false and those
//     under [profiles.*] included (W71-3).
//   - Grok: user config and Claude compatibility (parity.MCPNames), and
//     its project config (parity.ProjectMCPNames); its wiring is off
//     (grokToolsVerified).
func takenMCPNames(v adapter.Vendor, cwd, root string) (map[string]string, error) {
	out := map[string]string{}
	switch v {
	case adapter.Claude:
		m, err := parity.ClaudeMCPSources(os.Getenv)
		if err != nil {
			return nil, err
		}
		out = m
		above := false // past the repo root (or cwd outside one)
		for d := filepath.Clean(cwd); ; d = filepath.Dir(d) {
			f := filepath.Join(d, ".mcp.json")
			names, err := projectMCPNames(f)
			// A folder above the repo that this user cannot read (a
			// shared /home, a CI runner's) holds nothing aggy can see,
			// and is no clash; a file that is there but cannot be
			// parsed still refuses every tool.
			if above && errors.Is(err, fs.ErrPermission) {
				err = nil
			}
			if err != nil {
				return nil, err
			}
			above = above || d == filepath.Clean(root) || root == ""
			for _, n := range names {
				out[n] = f
			}
			if filepath.Dir(d) == d {
				break
			}
		}
		managed, err := claudeManagedNames()
		if err != nil {
			return nil, err
		}
		maps.Copy(out, managed)
	case adapter.Codex:
		return codexTakenNames(cwd, root, false)
	default:
		names, err := parity.MCPNames(v, root, os.Getenv)
		if err != nil {
			return nil, err
		}
		more, err := parity.ProjectMCPNames(v, cwd, root)
		if err != nil {
			return nil, err
		}
		names = append(names, more...)
		for _, n := range names {
			out[n] = string(v) + "'s config"
		}
	}
	return out, nil
}

// codexTakenNames is takenMCPNames for Codex. ignoreUser leaves out
// $CODEX_HOME/config.toml, which a child run with --ignore-user-config
// does not load (live-confirmed: no user MCP server, adapters.md); the
// other layers still count, a selected profile's too, since which of them
// codex exec reads then is unverified (R80-4, L78-9).
func codexTakenNames(cwd, root string, ignoreUser bool) (map[string]string, error) {
	if root == "" {
		root = cwd // Codex's project root is cwd outside git
	}
	layers, err := codexConfigs(cwd, root)
	if err != nil {
		return nil, err
	}
	user := ""
	if ignoreUser {
		if user, err = parity.CodexConfigPath(); err != nil {
			return nil, err
		}
	}
	out := map[string]string{}
	for _, l := range layers {
		if l.Data == nil || ignoreUser && l.Path == user {
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
	return out, nil
}

// claudeManagedMCP are where Claude Code reads an administrator's
// managed-mcp.json, in .mcp.json's format, by OS, per Claude Code's MCP
// docs ("Managed MCP configuration"); a test points it elsewhere.
// TODO(T6708): unverified live, and the Windows path is not read (aggy
// launches no session there yet).
var claudeManagedMCP = map[string][]string{
	"darwin": {"/Library/Application Support/ClaudeCode/managed-mcp.json"},
	"linux":  {"/etc/claude-code/managed-mcp.json"},
}[runtime.GOOS]

// claudeManagedNames maps the server names in an administrator's
// managed-mcp.json (claudeManagedMCP) to the file. A file this user
// cannot read is no clash aggy can see; one that cannot be parsed is an
// error, which refuses every tool.
func claudeManagedNames() (map[string]string, error) {
	out := map[string]string{}
	for _, f := range claudeManagedMCP {
		names, err := projectMCPNames(f)
		if errors.Is(err, fs.ErrPermission) {
			err = nil
		}
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			out[n] = f
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

// toolHookBin is the aggy path a tool's hook entry calls; a test swaps
// it, since a test binary is a go build.
var toolHookBin = aggyBin

// addHooks puts the hooks of the tools in on into req, for Claude's
// inline --settings layer: each runs aggy hook-tool, which never passes
// an "allow" on (toolwire.Hooks). A hook that approves, and any hook
// for Codex or Grok, is refused. When a wired hook declares rewrites and
// the safety policy is on req (addPolicy ran first), the policy's rules
// are added again with the hook's rewrite prefix (parity.RewriteDeny,
// H78-1).
func addHooks(v adapter.Vendor, caps []string, on []switchboard.State, req *adapter.InteractiveRequest, refuse func(tool string, code diag.Code, class, why string) string) []string {
	if !slices.ContainsFunc(on, func(st switchboard.State) bool { return len(st.Parts.Hooks) > 0 }) {
		return nil
	}
	bin := ""
	if v == adapter.Claude {
		if !slices.Contains(caps, probe.CapSettings) {
			return []string{refuse("", diag.CodeToolNoFlag, "no_settings", "claude --help does not list --settings, which carries the hooks")}
		}
		var err error
		if bin, err = toolHookBin(); err != nil {
			return []string{refuse("", diag.CodeToolWireFailed, "hook_bin", "aggy's own path cannot go in a hook command: "+err.Error())}
		}
	}
	hooks, refused := toolwire.Hooks(on, v, bin)
	var warns []string
	for _, r := range refused {
		warns = append(warns, refuse(r.Tool, r.Code, r.Code.String(), r.Why))
	}
	req.Hooks = hooks
	if len(hooks) > 0 && len(req.Deny) > 0 {
		var prefixes []string
		for _, st := range on {
			for _, h := range st.Parts.Hooks {
				if h.Rewrites && !slices.Contains(prefixes, h.RewritePrefix) {
					prefixes = append(prefixes, h.RewritePrefix)
				}
			}
		}
		for _, p := range prefixes {
			req.Deny = append(slices.Clip(req.Deny), parity.RewriteDeny(p)...)
		}
	}
	return warns
}
