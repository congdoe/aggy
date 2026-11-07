package permissions

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Approvals a person gave in Grok and Codex, read at each launch and switch.
// Nothing watches these files, so a running session picks a new one up at its
// next launch, and removing one in its own CLI removes it here too. Both files
// are source-read, not run (docs/verification.md, "Approvals across CLIs").
// OpenCode's Always allow lives in opencode.db beside its sign-ins, which aggy
// never opens, so those stay invisible here (docs/design.md, section 15).

// grokGrants is the part of Grok's permission.toml aggy reads
// (PermissionState, xai-grok-workspace/src/permission/state.rs at grok-build
// 2bdd1d6a; unknown fields are ignored, as an older Grok does).
type grokGrants struct {
	AllowBashExecute bool     `toml:"allow_bash_execute"`
	AllowedCommands  []string `toml:"allowed_bash_commands"`
	DeniedCommands   []string `toml:"disallowed_bash_commands"`
	AllowedGlobs     []string `toml:"allowed_bash_globs"`
	AllowedDomains   []string `toml:"allowed_web_fetch_domains"`
	DeniedDomains    []string `toml:"disallowed_web_fetch_domains"`
	AllowedMCPTools  []string `toml:"allowed_mcp_tools"`
	AllowedServers   []string `toml:"allowed_mcp_servers"`
	DeniedMCPTools   []string `toml:"disallowed_mcp_tools"`
}

// grokMaxDir is the longest folder name Grok URL-encodes; longer ones get a
// blake3 hash (encode_cwd_dirname, xai-grok-config/src/paths.rs at 2bdd1d6a),
// which aggy does not compute.
const grokMaxDir = 255

// grokScopeDir is Grok's folder name for a project: the root, URL-encoded as
// the urlencoding crate does, leaving only letters, digits, and - _ . ~.
func grokScopeDir(root string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(root); i++ {
		c := root[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String(), b.Len() <= grokMaxDir
}

// loadGrok reads this work tree's grants. Grok keeps them per project, each
// worktree its own, so they stay scoped to it.
func (s *Set) loadGrok(in Input) {
	if in.GrokHome == "" || in.Worktree == "" {
		return
	}
	dir, ok := grokScopeDir(in.Worktree)
	if !ok {
		s.Problems = append(s.Problems, Problem{in.Worktree, "this folder's path is too long for aggy to find Grok's approvals"})
		return
	}
	path := filepath.Join(in.GrokHome, "sessions", dir, "permission.toml")
	b := s.read(path)
	if b == nil {
		return
	}
	var g grokGrants
	if err := toml.Unmarshal(b, &g); err != nil {
		s.Problems = append(s.Problems, Problem{path, err.Error()})
		return
	}
	s.addGrok(g, path, in.Trusted)
}

func (s *Set) addGrok(g grokGrants, path string, trusted bool) {
	rule := func(k Kind, text string) Rule { return Rule{k, text, FromGrokGrant, path} }
	name := func(k Kind, text, why string) { s.Skipped = append(s.Skipped, Skip{rule(k, text), why}) }
	if g.AllowBashExecute {
		name(Allow, "allow_bash_execute", "this Grok approval allows every shell command, which aggy never carries")
	}
	// An approved command is a prefix there, but never for a command on Grok's
	// dangerous list, and that list is not read here. So it carries as the exact
	// command: narrower than Grok's, never wider.
	for _, c := range sortedClean(g.AllowedCommands) {
		if why := badCommand(c); why != "" {
			name(Allow, c, why)
			continue
		}
		s.AddAllow(rule(Allow, "Bash("+c+")"), trusted)
	}
	// A denied one is a prefix, and wider is the safe way for a deny.
	for _, c := range sortedClean(g.DeniedCommands) {
		if why := badCommand(c); why != "" {
			name(Deny, c, why)
			continue
		}
		s.add(rule(Deny, "Bash("+c+" *)"))
	}
	for _, c := range sortedClean(g.AllowedGlobs) {
		switch why := badCommand(c); {
		case why != "":
			name(Allow, c, why)
		case strings.ContainsAny(c, "?["):
			name(Allow, c, "Grok's ? and [...] have no match in Claude's rules")
		default:
			s.AddAllow(rule(Allow, "Bash("+c+")"), trusted)
		}
	}
	for _, d := range sortedClean(g.AllowedDomains) {
		s.AddAllow(rule(Allow, "WebFetch(domain:"+d+")"), trusted)
	}
	for _, d := range sortedClean(g.DeniedDomains) {
		// A deny there also covers subdomains (state.rs), so both forms.
		s.add(rule(Deny, "WebFetch(domain:"+d+")"))
		s.add(rule(Deny, "WebFetch(domain:*."+d+")"))
	}
	const mcp = "how Grok's MCP tool names map to Claude's is unverified"
	for _, list := range [][]string{g.AllowedMCPTools, g.AllowedServers} {
		for _, m := range sortedClean(list) {
			name(Allow, m, mcp)
		}
	}
	for _, m := range sortedClean(g.DeniedMCPTools) {
		name(Deny, m, mcp)
	}
}

// sortedClean sorts a list: Grok writes a set, in any order.
func sortedClean(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// badCommand says why a command cannot be written as a Bash rule.
func badCommand(c string) string {
	if c == "" || strings.TrimSpace(c) != c || strings.ContainsAny(c, "\n\r\x00") {
		return "it is not one line of text"
	}
	return ""
}

// codexRule is the line Codex appends when a person approves a command
// (blocking_append_allow_prefix_rule, execpolicy/src/amend.rs at rust-v0.164.0:
// prefix_rule(pattern=[...], decision="allow"), its tokens JSON strings).
var codexRule = regexp.MustCompile(`^prefix_rule\(pattern=(\[.*\]), decision="(allow|prompt|forbidden)"\)$`)

// plainToken can stand in a Bash rule as the same word Codex matched.
var plainToken = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

// loadCodex reads rules/default.rules, the file Codex's approvals go to
// (DEFAULT_POLICY_FILE, core/src/exec_policy.rs at rust-v0.164.0). It is parsed
// as text and never run. The file is global and says nothing of the repo a rule
// was approved in, so its allows apply only in a repo the person trusted, never
// everywhere: Codex's own reach is a limit of its rule files, not a choice to
// copy (docs/design.md, section 10). Its asks and denies only narrow, so they
// apply everywhere.
func (s *Set) loadCodex(in Input) {
	if in.CodexHome == "" {
		return
	}
	path := filepath.Join(in.CodexHome, "rules", "default.rules")
	b := s.read(path)
	if b == nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s.addCodexLine(line, path, in.Trusted)
	}
}

func (s *Set) addCodexLine(line, path string, trusted bool) {
	named := func(why string) {
		s.Skipped = append(s.Skipped, Skip{Rule{Allow, line, FromCodexGrant, path}, why})
	}
	m := codexRule.FindStringSubmatch(line)
	if m == nil {
		named("aggy reads only the one-line rules Codex writes itself, so this one is not carried")
		return
	}
	var tokens []string
	if err := json.Unmarshal([]byte(m[1]), &tokens); err != nil || len(tokens) == 0 {
		named("this rule lists alternatives or something other than plain words, so it is not carried")
		return
	}
	for _, t := range tokens {
		if !plainToken.MatchString(t) {
			named("a word in this rule needs quoting, which a Bash rule cannot match the same way")
			return
		}
	}
	// Codex matches a prefix by words, as Bash(a b *) does: the space keeps
	// the word boundary, and it also matches a b alone (permissions.md).
	text := "Bash(" + strings.Join(tokens, " ") + " *)"
	r := Rule{Allow, text, FromCodexGrant, path}
	switch m[2] {
	case "prompt":
		r.Kind = Ask
		s.add(r)
	case "forbidden":
		r.Kind = Deny
		s.add(r)
	default:
		if !trusted {
			s.Skipped = append(s.Skipped, Skip{r, "Codex's file does not say which repo it was approved in, so it applies only in a trusted repo (aggy trust <dir>)"})
			return
		}
		s.AddAllow(r, trusted)
	}
}
