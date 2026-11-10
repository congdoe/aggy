package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/policyhook"
	"github.com/congdoe/aggy/internal/safefs"
)

// The policy decision hook at launch (T6976, docs/design.md, section 10). On
// an on-screen Claude launch the policy's asks move from static ask rules,
// which no hook can lift, to aggy's PreToolUse hook, which asks for every
// policy match but an exact pre-approval. Denies stay static. Whenever the
// hook might not run, the launch keeps the static asks.

// decidePlanFile is the plan's name in the launch's private folder.
const decidePlanFile = "policy-decide.json"

// policyHookBin is the aggy path the decision hook calls; a test swaps it,
// since a test binary is a go build.
var policyHookBin = aggyBin

// usePolicyHook moves req's static asks to aggy's decision hook, writing the
// launch's plan into dir. It changes nothing, and says why in a note, when
// the hook might not run: --safe-mode or --bare, hooks turned off in a Claude
// settings file aggy can read, or an aggy path that cannot go in a hook
// command. It does nothing at all for a launch with no asks: a headless one,
// --no-policy, or every group set to deny.
func usePolicyHook(req *adapter.InteractiveRequest, plan parity.PolicyPlan, rules permissions.Set, root, version, dir string) string {
	if !plan.OnScreen || len(req.Ask) == 0 || req.PolicyHook != nil {
		return ""
	}
	const keep = "exact pre-approvals do not apply this session, and the safety policy asks as before: "
	switch {
	case claude.SkipsHooks(req.Extra):
		return keep + "--safe-mode and --bare start Claude without hooks"
	case rules.HooksOff != "":
		return keep + rules.HooksOff + " turns Claude's hooks off"
	}
	if f := repoHooksOff(root); f != "" {
		return keep + f + " turns Claude's hooks off"
	}
	bin, err := policyHookBin()
	if err != nil {
		return keep + "aggy's own path cannot go in a hook command: " + err.Error()
	}
	noPrompt := claude.NoPromptLaunch(req.Extra, req.UserSettings, rules.Mode)
	d := policyhook.NewDecidePlan(string(adapter.Claude), version, noPrompt, plan, parity.PreApprovedFrom(rules, root))
	path, err := writeDecidePlan(dir, d)
	if err != nil {
		return keep + "its plan could not be written: " + err.Error()
	}
	cmd, err := policyhook.DecideCommand(bin, path, noPrompt)
	if err != nil {
		_ = safefs.Remove(path)
		return keep + err.Error()
	}
	req.PolicyHook = &adapter.PolicyHook{
		Group: adapter.HookGroup{Matcher: policyhook.ClaudeHookMatcher(), Hooks: []adapter.HookCommand{{
			Type: "command", Command: cmd, Timeout: policyhook.DecideTimeout, OnFailure: "block",
		}}},
		Fallback: req.Ask,
	}
	req.Ask = nil
	return ""
}

// writeDecidePlan writes d into dir, which it makes when it is not there yet
// (the launch removes it either way), and returns the file's path.
func writeDecidePlan(dir string, d policyhook.DecidePlan) (string, error) {
	if _, err := safefs.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := safefs.Mkdir(dir, 0o700); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, decidePlanFile)
	return path, safefs.WriteFile(path, b, 0o600)
}

// repoHooksOff names a repo's Claude settings file that turns hooks off,
// committed or not, which Claude reads itself, or "". A file that is there but
// cannot be read or parsed counts as turning them off: the launch then only
// keeps the static asks.
func repoHooksOff(root string) string {
	if root == "" {
		return ""
	}
	for _, name := range []string{"settings.json", "settings.local.json"} {
		p := filepath.Join(root, ".claude", name)
		b, err := safefs.ReadConfig(p, claude.MaxSettings)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return p
		}
		var f struct {
			DisableAllHooks       any `json:"disableAllHooks"`
			AllowManagedHooksOnly any `json:"allowManagedHooksOnly"`
		}
		if json.Unmarshal(b, &f) != nil {
			return p
		}
		for _, v := range []any{f.DisableAllHooks, f.AllowManagedHooksOnly} {
			if v != nil && v != false {
				return p
			}
		}
	}
	return ""
}

// preApprovalCount is how many of pre's entries match a policy rule.
func preApprovalCount(pre parity.PreApproved, branches []string) int {
	n := 0
	for _, c := range pre.Commands {
		if g, _ := parity.MatchedGroups(parity.Call{Kind: parity.CallShell, Text: c}, branches); len(g) > 0 {
			n++
		}
	}
	for _, r := range pre.Reads {
		if g, _ := parity.MatchedGroups(parity.Call{Kind: parity.CallRead, Text: r}, branches); len(g) > 0 {
			n++
		}
	}
	return n
}

// policyHookNote is the launch line when the hook is on and there are
// pre-approvals of policy matches, so the person knows some run unasked.
func policyHookNote(req adapter.InteractiveRequest, rules permissions.Set, root string, branches []string) string {
	if req.PolicyHook == nil {
		return ""
	}
	n := preApprovalCount(parity.PreApprovedFrom(rules, root), branches)
	if n == 0 {
		return ""
	}
	if n == 1 {
		return "1 exact pre-approval of a safety policy match applies this session (aggy permissions list)"
	}
	return fmt.Sprintf("%d exact pre-approvals of safety policy matches apply this session (aggy permissions list)", n)
}
