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
