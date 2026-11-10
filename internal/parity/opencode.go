package parity

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/permissions"
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
// the local claude, and anthropic is only for a key added with aggy key,
// which does not exist yet.
var OpenCodeAlwaysDenied = []string{"claude-agent", "anthropic"}

// OpenCodeOwnProvider is OpenCode's own provider, whose models (its free
// ones among them) send prompts to OpenCode's own service. It is denied
// last, like OpenCodeAlwaysDenied, unless the launch's Providers name it,
// which only the person's [opencode] free_models does (config.Effective).
const OpenCodeOwnProvider = "opencode"

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
	// Shared are the person's rules (OpenCodeShared), exact allows only.
	Shared OpenCodeSharedRules
	// NoPolicy leaves out the safety policy's denies (aggy run
	// --no-policy). The whitelist, providers, and guard stay.
	NoPolicy bool
	// Policy is the safety policy as this launch decided it (PolicyPlan):
	// its shell asks and denies (OpenCodePolicy). Nil gives every rule as a
	// deny.
	Policy []PolicyDecision
	// Auto tells the guard this launch runs with --auto, so it skips its scan
	// of the command's text (docs/design.md).
	Auto bool
	// Decide is the absolute path of the launch's decision plan, which the
	// plugin's permission evaluate hook hands aggy hook decide (T6976), or ""
	// for no evaluate hook.
	Decide string
}

// strictAsk says the strict shell rules ask rather than deny on this launch:
// its plugin carries the evaluate hook, the repo is trusted for OpenCode, and
// --auto, which answers every ask, is off (T6976).
func (c OpenCodeConfig) strictAsk() bool {
	return c.Decide != "" && c.Trusted && !c.Auto && !c.NoPolicy
}

// openCodeRule is a permission or policy statement.
type openCodeRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// OpenCodeShellRules are the shell allow rules a launch renders: the starter,
// and extra, the person's exact rules (OpenCodeSharedRules.Allow). The guard
// scans a command's text only if it holds one, as only then can nothing prompt.
func OpenCodeShellRules(extra []string) ([]string, error) {
	rules, err := openCodePermissions(false, OpenCodeSharedRules{Allow: extra})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rules {
		if r.Action == "shell" {
			out = append(out, r.Resource)
		}
	}
	return out, nil
}

// OpenCodeRenderedAllows reads the shell allows back from a rendered config.
func OpenCodeRenderedAllows(data []byte) []string {
	var doc struct {
		Permissions []openCodeRule `json:"permissions"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	var out []string
	for _, r := range doc.Permissions {
		if r.Action == "shell" && r.Effect == "allow" {
			out = append(out, r.Resource)
		}
	}
	return out
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

// OpenCodeSharedRules are the person's rules as OpenCode takes them.
type OpenCodeSharedRules struct {
	// Allow are exact shell commands, as a rule's resource.
	Allow []string
	// Ask are shell commands to ask about, where a * keeps its wildcard
	// meaning: a wider ask only asks more.
	Ask []string
	// Deny are tool.use statements.
	Deny []string
}

// OpenCodeShared renders a rule set for OpenCode, and names what it cannot
// take. A rule that cannot be exact is skipped, never guessed at, since * and ?
// keep their wildcard meaning in a rule and {file:} expands.
func OpenCodeShared(s permissions.Set) (OpenCodeSharedRules, []permissions.Skip) {
	var out OpenCodeSharedRules
	var skipped []permissions.Skip
	// A shell deny or ask OpenCode cannot take could have stopped a shell
	// allow, or a source could not be read: then no allow from this layer goes.
	withhold := s.Withhold
	for _, r := range s.Rules {
		why := ""
		switch r.Kind {
		case permissions.Allow:
			var cmd string
			if cmd, why = openCodeAllow(r.Text); why == "" {
				out.Allow = append(out.Allow, cmd)
			}
		case permissions.Ask:
			cmd, _, ok := permissions.BashCommand(r.Text)
			switch {
			case !ok:
				why = "OpenCode takes only shell rules from aggy so far"
			case strings.ContainsAny(cmd, "?{\n\r"):
				why = "OpenCode would read ? or { in this rule differently"
				if r.Origin != permissions.FromRepoLocal { // or a repo could turn off the person's allows
					withhold = "the rule " + r.Text + " cannot reach OpenCode, and it might have stopped this one"
				}
			default:
				out.Ask = append(out.Ask, cmd)
			}
		case permissions.Deny:
			cmd, _, ok := permissions.BashCommand(r.Text)
			switch {
			case !ok:
				why = "OpenCode takes only shell rules from aggy so far"
			case strings.ContainsAny(cmd, "?{\n\r"):
				why = "OpenCode would read ? or { in this rule differently"
				if r.Origin != permissions.FromRepoLocal { // or a repo could turn off the person's allows
					withhold = "the rule " + r.Text + " cannot reach OpenCode, and it might have stopped this one"
				}
			default:
				out.Deny = append(out.Deny, "shell:"+cmd)
			}
		}
		if why != "" {
			skipped = append(skipped, permissions.Skip{Rule: r, Why: why})
		}
	}
	if withhold != "" {
		for _, c := range out.Allow {
			skipped = append(skipped, permissions.Skip{Rule: permissions.Rule{Kind: permissions.Allow, Text: "Bash(" + c + ")"}, Why: withhold})
		}
		out.Allow = nil
	}
	return out, skipped
}

// openCodeAllow returns the exact command an allow lists, or why it lists none.
func openCodeAllow(rule string) (cmd, why string) {
	cmd, wild, ok := permissions.BashCommand(rule)
	switch {
	case !ok:
		return "", "OpenCode takes only exact shell commands from aggy"
	case wild:
		return "", "it has a wildcard, and a rule here matches exactly: Bash(git diff *) would allow git diff --output=<file>"
	}
	if err := ExactRule(cmd); err != nil {
		return "", "it holds *, ?, or {, which an OpenCode rule cannot match exactly"
	}
	// A rule matches one simple command, so one with an operator never
	// matches, and a leading assignment is a declaration the check skips.
	first, _, _ := strings.Cut(cmd, " ")
	switch {
	case strings.ContainsAny(cmd, ";|&<>`$()\\\"'"):
		return "", "it is more than one plain command"
	case strings.Contains(first, "="):
		return "", "it starts by setting a variable"
	case strings.Join(strings.Fields(cmd), " ") != cmd:
		return "", "its spacing would never match a command OpenCode sees"
	case permissions.NeverListed(cmd):
		return "", first + " runs code or reads any path, so it is never listed for OpenCode"
	}
	if class, why := permissions.CheckAllow(rule); class == permissions.Drop {
		return "", why
	}
	return cmd, ""
}

// openCodePermissions renders the whitelist: an ask on everything, then
// the listed rules, since policies only ever deny (fake model). A later
// rule wins, so the person's asks go last.
func openCodePermissions(trusted bool, shared OpenCodeSharedRules) ([]openCodeRule, error) {
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
	for _, c := range shared.Allow {
		if slices.Contains(OpenCodeStarter, c) {
			continue
		}
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
	for _, c := range shared.Ask {
		rules = append(rules, openCodeRule{"shell", c, "ask"})
	}
	return rules, nil
}

// policyDecisions are the launch's, or every rule as a deny.
func (c OpenCodeConfig) policyDecisions() []PolicyDecision {
	if c.Policy == nil {
		return PolicyPlan{}.Decide()
	}
	return c.Policy
}

// OpenCodeShellDeny is the policy's Bash rules as tool.use statements,
// which match each simple command's text, with the shell asks of ds left
// out. A rule stops the wording it names only: `shell:curl *` missed 15 of
// 26 shapes (fake model). The sign-in rules always deny.
func OpenCodeShellDeny(ds []PolicyDecision, auto bool) []string {
	return openCodeShellDeny(ds, auto, false)
}

func openCodeShellDeny(ds []PolicyDecision, auto, strictAsk bool) []string {
	_, out := OpenCodePolicyFor(ds, auto, strictAsk)
	return append(out, "shell:*opencode.db*", "shell:*opencode auth export*")
}

// openCodePathDeny is one deny per tool on each protected file name, at
// any depth. A path deny stops the tool, not `cat` in the shell.
// The .env names are the on-screen list, so a template stays readable; the
// guard refuses the rest of .env.* by its own name check.
func openCodePathDeny() []string {
	names := slices.Concat(envFilesOnScreen, privateKeyFiles, openCodeCredentials)
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
// always-denied providers last, with OpenCode's own unless it is allowed.
func openCodePolicies(c OpenCodeConfig) ([]openCodeRule, error) {
	if len(c.Providers) == 0 {
		return nil, errors.New("parity: no provider is allowed for this launch")
	}
	rules := []openCodeRule{{"provider.use", "*", "deny"}}
	for _, p := range c.Providers {
		if slices.Contains(OpenCodeAlwaysDenied, p) {
			return nil, fmt.Errorf("parity: aggy never allows provider %q on OpenCode", p)
		}
		rules = append(rules, openCodeRule{"provider.use", p, "allow"})
	}
	denied := slices.Clone(OpenCodeAlwaysDenied)
	if !slices.Contains(c.Providers, OpenCodeOwnProvider) {
		denied = append(denied, OpenCodeOwnProvider)
	}
	for _, p := range denied {
		rules = append(rules, openCodeRule{"provider.use", p, "deny"})
	}
	if !c.NoPolicy {
		for _, r := range append(openCodeShellDeny(c.policyDecisions(), c.Auto, c.strictAsk()), openCodePathDeny()...) {
			rules = append(rules, openCodeRule{"tool.use", r, "deny"})
		}
	}
	// The person's own denies hold under --no-policy too: it drops aggy's.
	for _, r := range c.Shared.Deny {
		rules = append(rules, openCodeRule{"tool.use", r, "deny"})
	}
	return rules, nil
}

// Render returns the config's JSON.
func (c OpenCodeConfig) Render() ([]byte, error) {
	// OpenCode expands these anywhere in a config file, with no escape
	// (core/src/config/variable.ts at v2.2.5), so a repo folder with one in
	// its name would read an environment value or a file into the config.
	for _, v := range append([]string{c.Root, c.Plugin, c.Aggy, c.Model, c.Decide}, c.Instructions...) {
		if strings.Contains(v, "{env:") || strings.Contains(v, "{file:") {
			return nil, fmt.Errorf("parity: OpenCode would expand %q in its config, so this folder cannot be used with it", v)
		}
	}
	perms, err := openCodePermissions(c.Trusted, c.Shared)
	if err != nil {
		return nil, err
	}
	if !c.NoPolicy {
		// Last, so a policy ask wins over any allow listed before it.
		ask, _ := OpenCodePolicyFor(c.policyDecisions(), c.Auto, c.strictAsk())
		for _, cmd := range ask {
			perms = append(perms, openCodeRule{"shell", cmd, "ask"})
		}
	}
	pol, err := openCodePolicies(c)
	if err != nil {
		return nil, err
	}
	if c.Plugin == "" || c.Aggy == "" || c.Root == "" {
		return nil, errors.New("parity: an OpenCode launch needs its guard")
	}
	options := map[string]string{"aggy": c.Aggy, "root": c.Root}
	if c.Auto {
		options["auto"] = "1"
	}
	if c.Decide != "" && !c.NoPolicy {
		options["decide"] = c.Decide
	}
	doc := openCodeDoc{
		Model:        c.Model,
		Permissions:  perms,
		Policies:     pol,
		Instructions: c.Instructions,
		Plugins: []openCodePlugin{{
			Package: "file://" + c.Plugin,
			Options: options,
		}},
	}
	if doc.Instructions == nil {
		doc.Instructions = []string{}
	}
	return json.MarshalIndent(doc, "", "  ")
}
