// Package toolwire builds what a vendor CLI gets at launch for the
// managed tools that are on (switchboard.Board.On, T6708): today the
// MCP servers, with {repo} filled in. It reads and writes nothing; the
// caller writes the files it renders into the launch's private folder
// under aggy's state dir, never into the user's vendor config.
//
// Wiring is additive: a server is added next to the user's own, and a
// tool whose name a server of the user's already has is refused rather
// than wired (tool.name_clash), so aggy never replaces or merges into a
// server the person set up.
package toolwire

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/tooldef"
)

// Refusal is a tool that is on but was not wired, and why.
type Refusal struct {
	Tool string
	Code diag.Code
	Why  string
}

// repoChars are the characters a repo path may hold to be put in a
// tool's arguments: CheckBin's set (internal/hook), which no shell, JSON,
// TOML, or YAML reader treats as more than text (F-3).
const repoChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._+-@"

// MCP returns the MCP servers of the tools in on, in order, each with
// {repo} replaced by repo (the work tree the session runs in; "" outside
// one) and its working directory set to dir, a private folder outside
// the repo (S63-14), for the CLIs that take one. taken holds the names
// of the servers the vendor already loads from the user's or the repo's
// config. A tool is refused when its name is taken, or when its
// arguments hold {repo} and there is no repo or the repo's path holds a
// character outside repoChars.
func MCP(on []switchboard.State, repo, dir string, taken []string) ([]adapter.MCPServer, []Refusal) {
	var out []adapter.MCPServer
	var refused []Refusal
	for _, st := range on {
		m := st.Parts.MCP
		if m == nil {
			continue
		}
		name := st.Tool.Name
		if slices.Contains(taken, name) {
			refused = append(refused, Refusal{name, diag.CodeToolNameClash,
				fmt.Sprintf("a server named %s is already in %s's own config; aggy adds its servers next to yours and never replaces one", name, st.Vendor)})
			continue
		}
		args, err := fill(m.Argv[1:], repo)
		if err != nil {
			refused = append(refused, Refusal{name, diag.CodeToolRepoPath, err.Error()})
			continue
		}
		out = append(out, adapter.MCPServer{Name: name, Command: m.Argv[0], Args: args,
			EnvVars: slices.Clone(m.Env), Cwd: dir})
	}
	return out, refused
}

// fill replaces {repo} in each argument with repo. Arguments are argv
// words, never joined into a shell string.
func fill(args []string, repo string) ([]string, error) {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.Contains(a, tooldef.Placeholder) {
			switch {
			case repo == "":
				return nil, fmt.Errorf("its arguments hold %s, and the session is not in a repo", tooldef.Placeholder)
			case !strings.HasPrefix(repo, "/") || strings.Trim(repo, repoChars) != "":
				return nil, fmt.Errorf("its arguments hold %s, and the repo's path holds a character other than letters, digits, and / . _ + @ -", tooldef.Placeholder)
			}
		}
		out[i] = strings.ReplaceAll(a, tooldef.Placeholder, repo)
	}
	return out, nil
}

// approvingHooks are hook commands verified to answer "allow" to every
// action they touch: rtk hook codex (docs/verification.md, "Managed
// tools", S63-9). Like a hook whose definition says approves, they are
// never wired, whatever aggy hook-tool would strip.
var approvingHooks = [][]string{{"rtk", "hook", "codex"}}

// ToolHookTimeout is how long aggy hook-tool lets a tool's hook run when
// its definition sets no timeout, in seconds; the CLI's own timeout for
// the entry is two seconds longer.
const ToolHookTimeout = 8

// Hooks returns the hooks of the tools in on for vendor v, as
// Claude-format matcher groups by event, each running aggy hook-tool at
// bin: the wrapper runs the tool's argv and passes on only what
// hook.FilterToolOutput lets through, never an "allow". Only Claude
// takes hooks for one session (an inline --settings layer); for Codex
// and Grok a hook is refused (tool.no_launch_flag): their hook config
// is global, and Codex runs a hook only once trusted by hand. A hook
// known to approve is refused (tool.hook_approves).
func Hooks(on []switchboard.State, v adapter.Vendor, bin string) (map[string][]adapter.HookGroup, []Refusal) {
	var out map[string][]adapter.HookGroup
	var refused []Refusal
	for _, st := range on {
		name := st.Tool.Name
		for i, h := range st.Tool.Hooks {
			if !slices.Contains(h.Vendors, v) {
				continue
			}
			switch {
			case h.Approves || slices.ContainsFunc(approvingHooks, func(a []string) bool { return slices.Equal(a, h.Argv) }):
				refused = append(refused, Refusal{name, diag.CodeToolHookApproves,
					fmt.Sprintf("its %s hook approves actions you would be asked about, so aggy never wires it", h.Event)})
				continue
			case v != adapter.Claude:
				refused = append(refused, Refusal{name, diag.CodeToolNoFlag,
					fmt.Sprintf("%s takes no hook for one session, so its %s hook is not wired", v, h.Event)})
				continue
			}
			cmd, err := HookCommand(bin, v, name, i, st.Tool.Digest)
			if err != nil {
				refused = append(refused, Refusal{name, diag.CodeToolWireFailed, err.Error()})
				continue
			}
			timeout := h.Timeout
			if timeout == 0 {
				timeout = ToolHookTimeout
			}
			if out == nil {
				out = map[string][]adapter.HookGroup{}
			}
			out[h.Event] = append(out[h.Event], adapter.HookGroup{Matcher: h.Matcher,
				Hooks: []adapter.HookCommand{{Type: "command", Command: cmd, Timeout: timeout + 2}}})
		}
	}
	return out, refused
}

// HookCommand is the shell command a hook entry runs for hook i of tool:
//
//	test -x <aggy> && <aggy> hook-tool --vendor <v> <tool> <i> <digest> || true
//
// as aggy's own hook entries are written (hook.Command): bin passes
// hook.CheckBin, and every other word is a vendor, a tool name, a
// number, or 16 hex digits of the definition's digest, so no word needs
// quoting in any shell. aggy hook-tool runs nothing once the definition
// no longer has that digest.
func HookCommand(bin string, v adapter.Vendor, tool string, i int, digest string) (string, error) {
	if err := hook.CheckBin(bin); err != nil {
		return "", err
	}
	if !tooldef.ValidName(tool) || len(digest) < 16 || strings.Trim(digest[:16], "0123456789abcdef") != "" {
		return "", fmt.Errorf("tool %q or its digest cannot go in a hook command", tool)
	}
	return fmt.Sprintf("test -x %s && %s hook-tool --vendor %s %s %d %s || true", bin, bin, v, tool, i, digest[:16]), nil
}

// Names returns the servers' names.
func Names(servers []adapter.MCPServer) []string {
	out := make([]string, len(servers))
	for i, s := range servers {
		out[i] = s.Name
	}
	return out
}

// claudeServer is a stdio entry of Claude's MCP config: command and args
// (docs/verification.md, "MCP server config"; type is stdio when
// absent). Claude's entry has no working-directory field, so the server
// runs in the session's directory, which is why a tool's script must be
// an absolute path (internal/tooldef).
// TODO(T6708): unverified whether a server Claude starts inherits its
// environment; env_vars are not written as values, which may be secrets.
type claudeServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// ClaudeConfig renders the file for claude --mcp-config.
func ClaudeConfig(servers []adapter.MCPServer) ([]byte, error) {
	m := map[string]claudeServer{}
	for _, s := range servers {
		m[s.Name] = claudeServer{Command: s.Command, Args: nonNil(s.Args)}
	}
	return json.MarshalIndent(map[string]any{"mcpServers": m}, "", "  ")
}

// GrokAgentName is the name of the agent aggy passes Grok with --agent.
const GrokAgentName = "aggy-tools"

// grokServer is a stdio entry of a Grok agent's mcpServers: command,
// args, and cwd (McpServerTransportConfig::Stdio, untagged, so no type
// key; grok-build 2bdd1d6a xai-grok-config/src/mcp_server_config.rs:27-41,
// with cwd source-verified at 559751f). TODO(T6708): unverified whether
// the server inherits Grok's environment; env_vars are not written.
type grokServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Cwd     string   `json:"cwd,omitempty"`
}

// GrokAgent renders the agent definition for grok --agent: YAML
// frontmatter with name, description, and mcpServers as a list of
// one-key {name: config} maps, the form docs/verification.md records as
// loading ("Smoke run"), then a one-line body. Each value is written as
// JSON, which YAML reads as the same flow scalars and maps, so no
// character of a path or argument can change the structure.
func GrokAgent(servers []adapter.MCPServer) ([]byte, error) {
	var b strings.Builder
	b.WriteString("---\nname: " + GrokAgentName + "\n")
	b.WriteString("description: \"aggy: the managed tools turned on for this session\"\n")
	b.WriteString("mcpServers:\n")
	for _, s := range servers {
		e, err := json.Marshal(map[string]grokServer{s.Name: {Command: s.Command, Args: nonNil(s.Args), Cwd: s.Cwd}})
		if err != nil {
			return nil, err
		}
		b.WriteString("  - " + string(e) + "\n")
	}
	b.WriteString("---\nAnswer the user's request.\n")
	return []byte(b.String()), nil
}

func nonNil(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}
