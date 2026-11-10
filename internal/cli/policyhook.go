package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/policyhook"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// The policy decision hook at launch (T6976, docs/design.md, section 10). On
// an on-screen Claude launch the policy's asks move from static ask rules,
// which no hook can lift, to aggy's PreToolUse hook, which asks for every
// policy match but an exact pre-approval. Denies stay static. Whenever the
// hook might not run, or its plan might be changed under it, the launch keeps
// the static asks.

// decidePlanFile is the plan's name in the launch's private folder.
const decidePlanFile = "policy-decide.json"

// policyHookBin is the aggy path the decision hook calls; a test swaps it,
// since a test binary is a go build.
var policyHookBin = aggyBin

// policyHookEnv reads aggy's own environment; a test swaps it.
var policyHookEnv = os.Getenv

// claudeSimpleVar may be what Claude Code's --bare sets for its simple mode,
// which starts without hooks (PR #129 review, MEDIUM-3). Unverified, its name
// and its meaning both: aggy treats it as hooks off, which only keeps the
// static asks.
const claudeSimpleVar = "CLAUDE_CODE_SIMPLE"

// managedSettingsPath is Claude Code's managed settings file on this OS
// (code.claude.com/docs settings.md, "Settings files": macOS
// /Library/Application Support/ClaudeCode/, Linux and WSL /etc/claude-code/,
// Windows C:\Program Files\ClaudeCode\; unverified live at 2.1.322), or ""
// where aggy does not know it. A test swaps it.
var managedSettingsPath = func() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	case "linux":
		return "/etc/claude-code/managed-settings.json"
	case "windows":
		return `C:\Program Files\ClaudeCode\managed-settings.json`
	}
	return ""
}

// policyHookIn is what usePolicyHook needs from the launch.
type policyHookIn struct {
	plan    parity.PolicyPlan
	rules   permissions.Set
	root    string // the repo root, or "" outside a repo
	cwd     string // the launch's folder
	version string // the CLI's version, for the log
	dir     string // the launch's private folder
	// settingsFile says the merged --settings layer goes in a file
	// (writeSettingsFile), which the session's user could rewrite.
	settingsFile bool
	// ids are the identities of read pre-approvals, from the index.
	ids parity.ReadIdentity
}

// usePolicyHook moves req's static asks to aggy's decision hook, writing the
// launch's plan into in.dir and pinning it by its SHA-256 in the hook's
// command. It changes nothing, and says why in a note, when the hook might not
// run or its pin could be changed: --safe-mode or --bare, Claude's simple
// mode, hooks turned off in a Claude settings file aggy can read (the person's,
// the launch folder's or any folder's up to the repo root, managed), a
// --settings file, or an aggy path that cannot go in a hook command. It does
// nothing at all for a launch with no asks: a headless one, --no-policy, or
// every group set to deny.
func usePolicyHook(req *adapter.InteractiveRequest, in policyHookIn) string {
	if !in.plan.OnScreen || len(req.Ask) == 0 || req.PolicyHook != nil {
		return ""
	}
	const keep = "exact pre-approvals do not apply this session, and the safety policy asks as before: "
	switch {
	case claude.SkipsHooks(req.Extra):
		return keep + "--safe-mode and --bare start Claude without hooks"
	case simpleMode(req.UserSettings):
		return keep + claudeSimpleVar + " is set, which may start Claude without hooks"
	case in.settingsFile:
		// PR #129 review, HIGH-1: the layer, and the hash in it, would sit in a
		// file the session's user can write, and whether Claude reads a
		// --settings file again mid-session is unverified.
		return keep + "your --settings is a file, so aggy's layer goes in a file too, which the session could change"
	case in.rules.HooksOff != "":
		return keep + in.rules.HooksOff + " turns Claude's hooks off"
	}
	if f := repoHooksOff(in.cwd, in.root); f != "" {
		return keep + f + " turns Claude's hooks off"
	}
	if f := managedSettingsPath(); f != "" && settingsHooksOff(f) {
		return keep + f + " turns Claude's hooks off"
	}
	bin, err := policyHookBin()
	if err != nil {
		return keep + "aggy's own path cannot go in a hook command: " + err.Error()
	}
	root := in.root
	if root == "" {
		root = filepath.Clean(in.cwd)
	}
	noPrompt := claude.NoPromptLaunch(req.Extra, req.UserSettings, in.rules.Mode)
	d := policyhook.NewDecidePlan(string(adapter.Claude), in.version, noPrompt, in.plan, parity.PreApprovedFrom(in.rules, in.root, in.ids))
	d.Root, d.Protected = root, protectedPaths()
	path, sum, err := writeDecidePlan(in.dir, d)
	if err != nil {
		return keep + "its plan could not be written: " + err.Error()
	}
	cmd, err := policyhook.DecideCommand(bin, path, sum, noPrompt)
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
	req.Ask = settingsGuardAsks(in.cwd, root)
	return ""
}

// claudeSettingsNames are the project settings files whose disableAllHooks
// Claude reads, and reads again mid-session (live, 2.1.322, 2026-11-10).
var claudeSettingsNames = []string{"settings.json", "settings.local.json"}

// settingsGuardAsks are static ask rules that stay beside the decision hook
// (PR #129 review, live check of 2026-11-10): a write of a Claude project
// settings file mid-session turns hooks off for the rest of the session, so
// the hook alone cannot guard it. Edit rules apply to every built-in tool
// that edits files, and a path starting with // is absolute, ** any depth
// (permissions.md, "Read and Edit"); a Bash rule's * may stand anywhere
// (permissions.md, "Wildcard patterns"). These narrow the hole and do not
// close it: a shell write that spells the path another way, or a folder whose
// case differs, gets past them.
func settingsGuardAsks(cwd, root string) []string {
	var out []string
	for _, n := range claudeSettingsNames {
		out = append(out, "Edit(//**/.claude/"+n+")")
	}
	c, stop := filepath.Clean(cwd), filepath.Clean(root)
	if rel, err := filepath.Rel(stop, c); cwd != "" && err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		for d := c; ; d = filepath.Dir(d) {
			if filepath.IsAbs(d) && !strings.ContainsAny(d, "*?[]{}()\\\n\r") {
				for _, n := range claudeSettingsNames {
					out = append(out, "Edit(/"+filepath.ToSlash(filepath.Join(d, ".claude", n))+")")
				}
			}
			if d == stop || filepath.Dir(d) == d {
				break
			}
		}
	}
	return append(out, "Bash(*.claude/settings*)")
}

// simpleMode reports claudeSimpleVar set in aggy's environment, which the
// CLI inherits, or in the person's --settings env block.
func simpleMode(user map[string]any) bool {
	if policyHookEnv(claudeSimpleVar) != "" {
		return true
	}
	env, _ := user["env"].(map[string]any)
	v, has := env[claudeSimpleVar]
	return has && v != "" && v != false
}

// protectedPaths are the paths whose writes, and whose naming in a shell
// command, the decision hook always asks about: aggy's state folder, which
// holds the plan, the run folders, and aggy's permissions file.
func protectedPaths() []string {
	if d, err := home.Dir(); err == nil {
		return []string{d}
	}
	return nil
}

// writeDecidePlan writes d into dir, which it makes when it is not there yet
// (the launch removes it either way), and returns the file's path and the
// SHA-256 of what it wrote.
func writeDecidePlan(dir string, d policyhook.DecidePlan) (path, sum string, err error) {
	if _, err := safefs.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := safefs.Mkdir(dir, 0o700); err != nil {
			return "", "", err
		}
	} else if err != nil {
		return "", "", err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, decidePlanFile)
	return path, policyhook.PlanSum(b), safefs.WriteFile(path, b, 0o600)
}

// repoHooksOff names a Claude project settings file that turns hooks off,
// committed or not, in the launch folder or any folder above it up to the repo
// root (PR #129 review, MEDIUM-1: which folder Claude reads project settings
// from is unverified), or "". Outside a repo only the launch folder is read. A
// file that is there but cannot be read or parsed counts as turning them off:
// the launch then only keeps the static asks.
func repoHooksOff(cwd, root string) string {
	if cwd == "" {
		cwd = root
	}
	if cwd == "" {
		return ""
	}
	c, stop := filepath.Clean(cwd), filepath.Clean(root)
	if root == "" {
		stop = c
	}
	var dirs []string
	if rel, err := filepath.Rel(stop, c); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		dirs = []string{c, stop} // cwd is not under root: read both
	} else {
		for d := c; ; d = filepath.Dir(d) {
			dirs = append(dirs, d)
			if d == stop || filepath.Dir(d) == d {
				break
			}
		}
	}
	for _, d := range dirs {
		for _, name := range []string{"settings.json", "settings.local.json"} {
			if p := filepath.Join(d, ".claude", name); settingsHooksOff(p) {
				return p
			}
		}
	}
	return ""
}

// settingsHooksOff reports a Claude settings file that turns hooks off
// (disableAllHooks, or allowManagedHooksOnly, set to anything but false), or
// that is there but cannot be read or parsed. Only those two keys are read.
func settingsHooksOff(p string) bool {
	b, err := safefs.ReadConfig(p, claude.MaxSettings)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	var f struct {
		DisableAllHooks       any `json:"disableAllHooks"`
		AllowManagedHooksOnly any `json:"allowManagedHooksOnly"`
	}
	if json.Unmarshal(b, &f) != nil {
		return true
	}
	for _, v := range []any{f.DisableAllHooks, f.AllowManagedHooksOnly} {
		if v != nil && v != false {
			return true
		}
	}
	return false
}

// readIdentities looks up read pre-approvals' identities in the index s; nil
// s gives none.
func readIdentities(ctx context.Context, s *store.Store) parity.ReadIdentity {
	if s == nil {
		return nil
	}
	return func(p string) (uint64, uint64, bool) {
		dev, ino, ok, err := s.ReadApprovalFor(ctx, p)
		return dev, ino, ok && err == nil
	}
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
		if g, _ := parity.MatchedGroups(parity.Call{Kind: parity.CallRead, Text: r.Path}, branches); len(g) > 0 {
			n++
		}
	}
	return n
}

// policyHookNote is the launch line when the hook is on and there are
// pre-approvals of policy matches, so the person knows some run unasked.
func policyHookNote(req adapter.InteractiveRequest, pre parity.PreApproved, branches []string) string {
	if req.PolicyHook == nil {
		return ""
	}
	n := preApprovalCount(pre, branches)
	if n == 0 {
		return ""
	}
	if n == 1 {
		return "1 exact pre-approval of a safety policy match applies this session (aggy permissions list)"
	}
	return fmt.Sprintf("%d exact pre-approvals of safety policy matches apply this session (aggy permissions list)", n)
}
