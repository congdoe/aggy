package parity

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// OpenCode's per-launch config (docs/design.md, section 15). It is written
// fresh for each launch, in a folder aggy owns, and passed as
// OPENCODE_CONFIG, so it is never a manifest entry. Keys and precedence are
// recorded in docs/verification.md, "OpenCode 2.2.5 as an on-screen
// harness", at the source tag v2.2.5 (schema/src/config.ts,
// schema/src/config/policy.ts, core/src/config/plugin/policy.ts). Like the
// rest of the policy, it is a guardrail against an agent's mistakes, not a
// sandbox: a model's shell runs as the person, and can rewrite this file.

// OpenCodeStarter is the shipped whitelist: read-only commands, each
// matched exactly, so `git diff --output=x` and `ls > x` still ask (live,
// fake model). The git ones can still run a repo's clean filter or
// post-index-change hook once a tracked file's time changes; there is no
// known stop (docs/design.md).
var OpenCodeStarter = []string{
	"git status",
	"git status --short",
	"git diff --no-ext-diff --no-textconv",
	"git diff --no-ext-diff --no-textconv --stat",
	"git log --oneline -20",
	"git branch --show-current",
	"ls",
	"pwd",
}

// openCodeUIActions are allowed everywhere: they reach nothing outside the
// session. Under the ask on "*", a todo update and the permission to show
// a question would each ask first (core/src/tool/plugin/todo.ts and
// question.ts at v2.2.5).
var openCodeUIActions = []string{"todowrite", "question"}

// openCodeReadTools are listed only in a repo trusted for OpenCode. In
// another repo reads ask: a cloned symlink to a key file was read and sent
// to the model with no prompt under a read allow (fake model).
var openCodeReadTools = []string{"read", "grep", "glob"}

// OpenCodeAlwaysDenied are providers no launch allows: claude-agent runs
// the local claude, opencode's free models send prompts to OpenCode's own
// service, and anthropic is only for a key added with aggy key, which
// does not exist yet.
var OpenCodeAlwaysDenied = []string{"claude-agent", "opencode", "anthropic"}

// openCodeCredentials are the file names of OpenCode's sign-ins, denied
// where a rule can see a path: sessions and credentials share opencode.db.
var openCodeCredentials = []string{"opencode.db", "opencode.db-*", "auth.json"}

// OpenCodeDeniedNames are the file names the guard also refuses in any case.
func OpenCodeDeniedNames() []string {
	return append(slices.Clone(keyFiles), openCodeCredentials...)
}

// openCodePathTools take a file path as their resource; external_directory
// a directory path; grep and glob their pattern, not a path, so a path deny
// on them only catches a pattern naming the file (core/src/tool/plugin).
var openCodePathTools = []string{"read", "edit", "glob", "external_directory"}

// OpenCodeConfig is what one launch tells OpenCode.
type OpenCodeConfig struct {
	// Trusted lists read, grep, and glob too: a repo trusted for OpenCode.
	Trusted bool
	// Providers are the providers this launch may use, after the plans,
	// keys, and the repo's allowed_vendors narrowed them. At least one.
	Providers []string
	// Model is the launch's model, or "": a resumed session keeps its own.
	Model string
	// Instructions are absolute paths of the copies aggy made.
	Instructions []string
	// Plugin is the absolute path of the plugin's folder.
	Plugin string
	// Aggy is the absolute path of the aggy binary the plugin calls.
	Aggy string
	// Root is the launch's repo root (its folder, outside a repo): the
	// guard refuses a tool call outside it.
	Root string
	// NoPolicy leaves out the safety policy's denies (aggy run
	// --no-policy). The whitelist, providers, and guard stay.
	NoPolicy bool
}

// openCodeRule is a permission or policy statement.
type openCodeRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// openCodePlugin is a plugin entry with its options
// (schema/src/config/plugin.ts at v2.2.5). Only the directory form loads;
// a file path silently never does (fake model).
type openCodePlugin struct {
	Package string            `json:"package"`
	Options map[string]string `json:"options"`
}

type openCodeDoc struct {
	Model        string           `json:"model,omitempty"`
	Permissions  []openCodeRule   `json:"permissions"`
	Policies     []openCodeRule   `json:"policies"`
	Instructions []string         `json:"instructions"`
	Plugins      []openCodePlugin `json:"plugins"`
}

// ExactRule refuses a command that cannot be listed as an exact rule:
// OpenCode gives * and ? in a rule their wildcard meaning, with no escape,
// and expands {file:} in rule text (fake model, second review).
func ExactRule(cmd string) error {
	switch {
	case strings.TrimSpace(cmd) != cmd || cmd == "":
		return fmt.Errorf("parity: %q is not an exact command", cmd)
	case strings.ContainsAny(cmd, "*?{\n\r"):
		return fmt.Errorf("parity: %q holds *, ?, {, or a newline, which an OpenCode rule cannot match exactly", cmd)
	}
	return nil
}

// openCodePermissions renders the whitelist: an ask on everything, then
// the listed rules, since policies only ever deny (fake model). A later
// rule wins.
func openCodePermissions(trusted bool) ([]openCodeRule, error) {
	rules := []openCodeRule{{"*", "*", "ask"}}
	for _, a := range openCodeUIActions {
		rules = append(rules, openCodeRule{a, "*", "allow"})
	}
	for _, c := range OpenCodeStarter {
		if err := ExactRule(c); err != nil {
			return nil, err
		}
		rules = append(rules, openCodeRule{"shell", c, "allow"})
	}
	if trusted {
		for _, t := range openCodeReadTools {
			rules = append(rules, openCodeRule{t, "*", "allow"})
		}
	}
	return rules, nil
}

// OpenCodeShellDeny is the policy's Bash rules as tool.use statements,
// which match each simple command's text. A rule stops the wording it
// names only: `shell:curl *` missed 15 of 26 shapes (fake model).
func OpenCodeShellDeny() []string {
	out := make([]string, 0, len(bashDeny)+2)
	for _, r := range bashDeny {
		out = append(out, "shell:"+strings.TrimSuffix(strings.TrimPrefix(r, "Bash("), ")"))
	}
	return append(out, "shell:*opencode.db*", "shell:*opencode auth export*")
}

// openCodePathDeny is one deny per tool on each protected file name, at
// any depth. A path deny stops the tool, not `cat` in the shell.
func openCodePathDeny() []string {
	names := OpenCodeDeniedNames()
	var out []string
	for _, tool := range openCodePathTools {
		for _, n := range names {
			out = append(out, tool+":"+n, tool+":*/"+n)
		}
	}
	// grep's resource is its search pattern: only a pattern that is the
	// name itself, or text like `process.env` and `api.key` would be
	// refused too.
	for _, n := range names {
		if !strings.Contains(n, "*") {
			out = append(out, "grep:"+n)
		}
	}
	return out
}

// openCodePolicies renders the provider allow-list and the tool denies. A
// statement's last match wins, so the deny-all goes first and the
// always-denied providers last.
func openCodePolicies(c OpenCodeConfig) ([]openCodeRule, error) {
	if len(c.Providers) == 0 {
		return nil, errors.New("parity: no provider is allowed for this launch")
	}
	rules := []openCodeRule{{"provider.use", "*", "deny"}}
	for _, p := range c.Providers {
		rules = append(rules, openCodeRule{"provider.use", p, "allow"})
	}
	for _, p := range OpenCodeAlwaysDenied {
		rules = append(rules, openCodeRule{"provider.use", p, "deny"})
	}
	if !c.NoPolicy {
		for _, r := range append(OpenCodeShellDeny(), openCodePathDeny()...) {
			rules = append(rules, openCodeRule{"tool.use", r, "deny"})
		}
	}
	return rules, nil
}

// Render returns the config's JSON.
func (c OpenCodeConfig) Render() ([]byte, error) {
	// OpenCode expands these anywhere in a config file, with no escape
	// (core/src/config/variable.ts at v2.2.5), so a repo folder with one in
	// its name would read an environment value or a file into the config.
	for _, v := range append([]string{c.Root, c.Plugin, c.Aggy, c.Model}, c.Instructions...) {
		if strings.Contains(v, "{env:") || strings.Contains(v, "{file:") {
			return nil, fmt.Errorf("parity: OpenCode would expand %q in its config, so this folder cannot be used with it", v)
		}
	}
	perms, err := openCodePermissions(c.Trusted)
	if err != nil {
		return nil, err
	}
	pol, err := openCodePolicies(c)
	if err != nil {
		return nil, err
	}
	if c.Plugin == "" || c.Aggy == "" || c.Root == "" {
		return nil, errors.New("parity: an OpenCode launch needs its guard")
	}
	doc := openCodeDoc{
		Model:        c.Model,
		Permissions:  perms,
		Policies:     pol,
		Instructions: c.Instructions,
		Plugins: []openCodePlugin{{
			Package: "file://" + c.Plugin,
			Options: map[string]string{"aggy": c.Aggy, "root": c.Root},
		}},
	}
	if doc.Instructions == nil {
		doc.Instructions = []string{}
	}
	return json.MarshalIndent(doc, "", "  ")
}
