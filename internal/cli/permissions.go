package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/transcript"
)

// permissionsFile is ~/.aggy/permissions.json.
func permissionsFile() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, permissions.FileName), nil
}

// trustFile keeps the repos the person trusted with aggy trust <dir>, in
// aggy's state folder, so a repo can never name itself.
func trustFile() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "trust.json"), nil
}

// repoTrusted reports whether the repo with this key is trusted. A file that
// cannot be read trusts nothing.
func repoTrusted(key string) bool {
	if key == "" {
		return false
	}
	path, err := trustFile()
	if err != nil {
		return false
	}
	b, err := safefs.ReadConfig(path, config.MaxFileSize)
	if err != nil {
		return false
	}
	var t openCodeTrust
	if err := json.Unmarshal(b, &t); err != nil {
		return false
	}
	return slices.Contains(t.Repos, key)
}

// repoIsTrusted is either kind of trust: aggy trust, or aggy trust --opencode,
// so the person has one step, not two.
func repoIsTrusted(repo toolRepoAt) bool {
	if repoTrusted(repo.key) {
		return true
	}
	for _, root := range []string{repo.main, repo.root} {
		if trusted, _ := openCodeTrusted(root); trusted {
			return true
		}
	}
	return false
}

// permissionInput says where the rules for dir come from.
func permissionInput(dir string) permissions.Input {
	var in permissions.Input
	in.ClaudeDir, _ = transcript.Home(adapter.Claude, os.Getenv)
	in.AggyFile, _ = permissionsFile()
	in.GrokHome, _ = transcript.Home(adapter.Grok, os.Getenv)
	in.CodexHome, _ = transcript.Home(adapter.Codex, os.Getenv)
	if repo, err := toolRepo(dir, false); err == nil && repo.main != "" {
		in.Main, in.RepoKey = repo.main, repo.key
		in.Trusted = repoIsTrusted(repo)
		if real, err := safefs.RealPath(repo.root); err == nil {
			in.Worktree = real
		}
	} else if real, err := safefs.RealPath(dir); err == nil {
		// Outside a repo Grok scopes grants to the launch folder.
		in.Worktree = real
	}
	return in
}

// settingsPathspec lists what git tracks at or under .claude, whatever the folder's
// case, so the answer stays small in a large repo. git's icase folds ASCII only, and
// a file system can also fold a long s to s, so trackedFold decides from this list.
const settingsPathspec = ":(icase).claude"

// gitTracks asks git whether it tracks path, under main, without case, since a
// file system that folds case opens .CLAUDE/settings.local.json by the lowercase
// path. git's own config cannot run anything here (gitSafe), and an error, or a
// list cut short, counts as tracked.
func gitTracks(ctx context.Context, main string) func(string) (bool, error) {
	return func(path string) (bool, error) {
		rel, err := filepath.Rel(main, path)
		if err != nil {
			return true, err
		}
		out, cut, err := defaultBorrowEnv().git(ctx, main, "ls-files", "-z", "--", settingsPathspec)
		if err != nil {
			return true, err
		}
		if cut {
			return true, nil
		}
		return trackedFold(out, rel), nil
	}
}

// trackedFold reports rel in git's -z list, or its folder as an entry of its own (a
// committed link, file, or submodule's gitlink), whatever the case.
func trackedFold(list, rel string) bool {
	dir := filepath.ToSlash(filepath.Dir(rel))
	rel = filepath.ToSlash(rel)
	for _, p := range strings.Split(list, "\x00") {
		if strings.EqualFold(p, rel) || strings.EqualFold(p, dir) {
			return true
		}
	}
	return false
}

// loadPermissions reads the rules in force for dir, and the mode the person
// set in aggy's config, which comes first.
func loadPermissions(ctx context.Context, dir string, cfg config.Config) permissions.Set {
	in := permissionInput(dir)
	if in.Main != "" {
		in.Tracked = gitTracks(ctx, in.Main)
	}
	s := permissions.Load(in)
	if mode, err := permissions.CheckMode(cfg.Permissions.Mode); err == nil {
		s.Mode, s.ModeFrom = mode, permissions.FromConfig
	}
	return s
}

// switchBlocked says why the next launch of v in dir would refuse, so a switch must
// not close the CLI on screen. TODO(T6955): a pruned session file, and a busy lease.
func switchBlocked(dir string, v adapter.Vendor) string {
	cfg, err := config.LoadDefault()
	if err != nil {
		return termsafe.Line(err.Error())
	}
	if fi, err := safefs.Stat(dir); err != nil || !fi.IsDir() {
		return termsafe.Line("the folder " + dir + " is gone")
	}
	if _, err := allowedIn(cfg, dir, v); err != nil {
		return termsafe.Line(err.Error())
	}
	path, err := permissionsFile()
	if err != nil {
		return ""
	}
	if e := permissions.AggyFileError(path, permissionInput(dir).RepoKey); e != nil {
		return termsafe.Line(e.Error())
	}
	return ""
}

// addPermissions puts the shared rules on req for vendor v, and returns the
// launch's lines for the screen: only what means a deny may be missing. A
// rule that cannot reach a CLI never stops the launch: it only means that CLI
// asks, which aggy doctor says (sharedRules' status, T7015).
func addPermissions(v adapter.Vendor, caps []string, s permissions.Set, req *adapter.InteractiveRequest) []string {
	return sharedRules(v, caps, s, req).screen
}

// ruleNotes are what putting the shared rules on a launch found: screen, the
// lines an on-screen launch prints (a deny or ask that does not reach the
// CLI, a stricter mode not applied), and status, what aggy doctor says
// instead (T7015): allows that do not reach it, which only mean it asks more.
// A status note's message holds no rule text or path; those go in its detail.
// all is every note in the words and order print mode has always printed
// (#148 review L4).
type ruleNotes struct {
	screen []string
	status []checkup.Finding
	all    []string
}

// codePermRules is doctor's finding for a rule that does not reach a CLI.
const codePermRules = "permission-rules"

// sharedRules is addPermissions with the status notes kept.
func sharedRules(v adapter.Vendor, caps []string, s permissions.Set, req *adapter.InteractiveRequest) ruleNotes {
	var n ruleNotes
	status := func(msg, detail, old string) {
		n.status = append(n.status, checkup.Finding{Code: codePermRules, Level: checkup.LevelInfo, Vendor: v, Message: msg, Detail: detail})
		n.all = append(n.all, old)
	}
	screen := func(l string) {
		n.screen = append(n.screen, l)
		n.all = append(n.all, l)
	}
	for _, p := range s.Problems {
		if p.Repo {
			screen(fmt.Sprintf("a permissions file that could have come with this repo is not used: %s: %s", p.Path, p.Why))
			continue
		}
		screen(fmt.Sprintf("a deny may be missing, from %s: %s", p.Path, p.Why))
	}
	if s.Withhold != "" {
		screen(s.Withhold)
	}
	for _, l := range s.Notes {
		screen(l)
	}
	if v != adapter.Claude && v != adapter.Grok {
		return n
	}
	r := s.Render(v)
	if r.Lost != "" {
		status("none of your allow rules from aggy reach Grok: a rule cannot reach it, and it might have stopped one", termsafe.Field(r.Lost),
			fmt.Sprintf("none of your allow rules from aggy reach Grok this session: the rule %s cannot reach it, and it might have stopped one", termsafe.Field(r.Lost)))
	}
	skipped := slices.Clone(r.Skipped)
	for _, k := range s.Skipped {
		// What a CLI reads itself is not aggy's to pass on, and Codex's file is
		// global, so a held approval from it would repeat at every launch in
		// every untrusted repo: aggy permissions show lists those.
		if !permissions.ReadsItself(v, k.Origin) && k.Origin != permissions.FromCodexGrant {
			skipped = append(skipped, k)
		}
	}
	// #148 review H2: a deny or ask that does not reach the CLI is a
	// protection missing, said on screen; an allow that does not only means
	// the CLI asks more, which aggy doctor says.
	n.screen = append(n.screen, skippedLines(skipped, vendorNames[v])...)
	if allows := skippedAllows(skipped); allows > 0 {
		n.status = append(n.status, checkup.Finding{Code: codePermRules, Level: checkup.LevelInfo, Vendor: v, Message: skippedNote(allows, vendorNames[v], !s.Trusted)})
	}
	if len(skipped) > 0 {
		n.all = append(n.all, skippedNote(len(skipped), vendorNames[v], !s.Trusted))
	}
	switch v {
	case adapter.Claude:
		if !slices.Contains(caps, probe.CapSettings) {
			if len(r.Allow)+len(r.Ask)+len(r.Deny) > 0 || r.Mode != "" {
				// Denies among them: a protection missing, on screen.
				screen("claude --help does not list --settings, so your rules from aggy do not reach this session")
			}
			return n
		}
	case adapter.Grok:
		// An allow that names or covers a safety policy rule is not passed to
		// Grok: Grok matches it as a word-boundary prefix, so Bash(rm -rf dist)
		// would also allow rm -rf dist ~. The policy's deny stays
		// (parity.GrokPolicy) and wins; leaving the allow out avoids an allow
		// that looks as if it did something.
		var kept []string
		for _, a := range r.Allow {
			if len(parity.CoveredRules(a, nil)) > 0 {
				status("an allow of yours names a safety policy rule, so it does not go to Grok", termsafe.Field(a),
					fmt.Sprintf("your allow %s names a safety policy rule, so it does not go to Grok", termsafe.Field(a)))
				continue
			}
			kept = append(kept, a)
		}
		r.Allow = kept
		if len(r.Allow) > 0 && !slices.Contains(caps, probe.CapAllow) {
			status("grok --help does not list --allow, so your allow rules from aggy do not reach it", "",
				"grok --help does not list --allow, so your allow rules from aggy do not reach this session")
			r.Allow = nil
		}
		if len(r.Deny) > 0 && !slices.Contains(caps, probe.CapDeny) {
			// A deny not passed: a protection missing, on screen.
			screen("grok --help does not list --deny, so your deny rules from aggy do not reach this session, and neither do your allows, since a deny might have stopped one")
			r.Deny, r.Allow = nil, nil
		}
		if r.Mode != "" && !slices.Contains(caps, probe.CapPermissionMode) {
			// #148 review M3: a mode stricter than Grok's default (plan, which
			// edits nothing, or dontAsk, which refuses what is not allowed)
			// not applied is a protection missing, said at launch; a looser
			// one only means Grok asks more.
			old := "grok --help does not list --permission-mode, so the mode set in aggy does not reach this session"
			if strictMode(r.Mode) {
				n.screen = append(n.screen, "grok --help does not list --permission-mode, so the "+r.Mode+" mode set in aggy does not reach this session, and Grok runs in its default mode")
				n.all = append(n.all, old)
			} else {
				status("grok --help does not list --permission-mode, so the mode set in aggy does not reach it", "", old)
			}
			r.Mode = ""
		}
		if slices.ContainsFunc(req.Extra, func(a string) bool {
			return a == "--permission-mode" || strings.HasPrefix(a, "--permission-mode=")
		}) {
			r.Mode = "" // the person's own flag, for this launch
		}
	}
	req.Rules = adapter.SharedRules{Allow: r.Allow, Ask: r.Ask, Deny: r.Deny, Mode: r.Mode}
	return n
}

// strictMode reports a permission mode stricter than the default a CLI falls
// back to: plan (no edits) or dontAsk (what is not allowed is refused).
func strictMode(mode string) bool {
	return mode == "plan" || mode == "dontAsk"
}

// skippedLines is the screen's line for the denies and asks among skipped,
// for the CLI named name: none when every skipped rule is an allow.
func skippedLines(skipped []permissions.Skip, name string) []string {
	n := len(skipped) - skippedAllows(skipped)
	if n == 0 {
		return nil
	}
	return []string{protectionsSkippedNote(n, name)}
}

// skippedAllows counts the allows among skipped.
func skippedAllows(skipped []permissions.Skip) int {
	n := 0
	for _, k := range skipped {
		if k.Kind == permissions.Allow {
			n++
		}
	}
	return n
}

// protectionsSkippedNote is the launch's line for n denies or asks of the
// person's that do not reach the CLI named name: it does not refuse, or ask
// before, what they name (#148 review H2).
func protectionsSkippedNote(n int, name string) string {
	if n == 1 {
		return "1 of your deny or ask rules does not reach " + name + ", which will not refuse or ask before what it names; aggy permissions show says why"
	}
	return fmt.Sprintf("%d of your deny or ask rules do not reach %s, which will not refuse or ask before what they name; aggy permissions show says why", n, name)
}

// skippedNote is the launch's one line about rules that do not apply. In a
// repo not trusted, it names the way to trust it (T6979): some of them wait
// for that.
func skippedNote(n int, name string, untrusted bool) string {
	s := fmt.Sprintf("%d of your permission rules do not apply on %s; aggy permissions show says why", n, name)
	if n == 1 {
		s = fmt.Sprintf("1 of your permission rules does not apply on %s; aggy permissions show says why", name)
	}
	if untrusted {
		s += "; this repo is not trusted, and an allow from it waits for that: aggy trust in a terminal, or /aggy trust in the session"
	}
	return s
}

// guardRules are the person's exact rules the OpenCode guard must count, for
// the repo in its payload. Its gate only decides when to scan, so it errs wide:
// it does not ask git or trust, and it adds what the running launch rendered, as
// a rule removed since is still in OpenCode's config. It starts nothing. When a
// file cannot be read, the rules are unknown, and scanAlways says so: every
// command is scanned, for the rules aggy ships as well as these.
func guardRules(payload []byte, getenv func(string) string) (rules []string, scanAlways bool) {
	var in struct {
		Root string `json:"root"`
	}
	if err := json.Unmarshal(payload, &in); err != nil || in.Root == "" {
		return nil, false
	}
	pi := permissionInput(in.Root)
	pi.Trusted = true
	pi.Tracked = func(string) (bool, error) { return false, nil }
	s := permissions.Load(pi)
	shared, _ := parity.OpenCodeShared(s)
	// An exact pre-approval of a policy match runs with no prompt too
	// (T6976), so a command holding one is scanned like one holding an allow.
	rules = append(shared.Allow, parity.PreApprovedFrom(s, in.Root, nil).Commands...)
	return append(rules, renderedOpenCodeAllows(getenv("OPENCODE_CONFIG"))...), s.FileErr != nil || len(s.Problems) > 0
}

// renderedOpenCodeAllows reads the shell allows of a rendered config.
func renderedOpenCodeAllows(path string) []string {
	if path == "" {
		return nil
	}
	b, err := safefs.ReadConfig(path, config.MaxFileSize)
	if err != nil {
		return nil
	}
	return parity.OpenCodeRenderedAllows(b)
}
