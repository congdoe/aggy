package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/antigravity"
	"github.com/congdoe/aggy/internal/adapter/hermes"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/supervisor"
)

// maxCodexConfig caps the Codex config files read to merge
// developer_instructions.
const maxCodexConfig = 1 << 20

// memoryDelivery says how vendor v takes the shared memory, given its
// probed capabilities: ok, or why not. Claude's flag is live-verified
// but missing from --help, so it needs no capability.
func memoryDelivery(v adapter.Vendor, caps []string) (bool, string) {
	switch v {
	case adapter.Codex:
		if !slices.Contains(caps, probe.CapConfigOverride) {
			return false, "codex --help does not list -c"
		}
	case adapter.Grok:
		if !slices.Contains(caps, probe.CapRules) {
			return false, "grok --help does not list --rules"
		}
	case adapter.Antigravity:
		// TODO(T6957): agy reads AGENTS.md and GEMINI.md, and .agents/rules
		// (Google's docs), and takes no system-prompt flag aggy knows.
		return false, "agy takes no text aggy can pass at launch (it reads AGENTS.md, GEMINI.md, and .agents/rules, from Google's docs)"
	}
	return true, ""
}

// addMemory puts the shared memory of the repo holding cwd on req for
// vendor v. For
// Claude it writes the framed text to a private temp file; cleanup
// removes it after the session. Warnings say what was not delivered and
// why; none of them stops the launch.
func addMemory(v adapter.Vendor, caps []string, cwd, memDir string, req *adapter.InteractiveRequest) (cleanup func(), warns []string) {
	cleanup = func() {}
	root, _ := repoRoot(cwd)
	text, err := parity.LoadMemory(root)
	if err != nil {
		return cleanup, []string{"shared memory not delivered: " + err.Error()}
	}
	if text == "" {
		return cleanup, nil
	}
	if ok, why := memoryDelivery(v, caps); !ok {
		return cleanup, []string{"shared memory not delivered: " + why}
	}
	framed := parity.FrameMemory(text)
	switch v {
	case adapter.Claude:
		dir := memDir
		if err := safefs.Mkdir(dir, 0o700); err != nil {
			return cleanup, []string{"shared memory not delivered: " + err.Error()}
		}
		cleanup = func() { _ = safefs.RemoveAll(dir) }
		f := filepath.Join(dir, "memory.md")
		if err := safefs.WriteFile(f, []byte(framed), 0o600); err != nil {
			cleanup()
			return func() {}, []string{"shared memory not delivered: " + err.Error()}
		}
		req.MemoryFile = f
	case adapter.Codex:
		layers, err := codexConfigs(cwd, root)
		if err == nil {
			framed, err = parity.CodexInstructions(layers, framed)
		}
		if err != nil {
			return cleanup, []string{"shared memory not delivered: " + err.Error()}
		}
		req.Memory = framed
	default:
		req.Memory = framed
	}
	return cleanup, nil
}

// codexConfigs reads the config.toml of every Codex layer folder
// (parity.CodexLayerDirs), lowest precedence first, and the profile file
// the user config selects with profile = "<name>" ($CODEX_HOME/<name>.config.toml,
// config/src/loader/mod.rs), which counts as ambiguous like a project
// layer. An absent file has nil Data.
func codexConfigs(cwd, root string) ([]parity.CodexConfig, error) {
	dirs, err := parity.CodexLayerDirs(cwd, root)
	if err != nil {
		return nil, err
	}
	read := func(p string) ([]byte, error) {
		b, err := safefs.ReadFileLimit(p, maxCodexConfig)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) && strings.HasPrefix(p, parity.CodexSystemDir+"/") {
			return nil, nil
		}
		return b, err
	}
	var out []parity.CodexConfig
	for i, d := range dirs {
		b, err := read(filepath.Join(d.Dir, "config.toml"))
		if err != nil {
			return nil, err
		}
		out = append(out, parity.CodexConfig{Path: filepath.Join(d.Dir, "config.toml"), Data: b, Project: d.Project})
		if i != 1 || b == nil {
			continue
		}
		// The user layer may select a profile file.
		var doc struct{ Profile string }
		if _, err := toml.Decode(string(b), &doc); err == nil && doc.Profile != "" {
			if strings.ContainsAny(doc.Profile, `/\`) || strings.HasPrefix(doc.Profile, ".") {
				return nil, parity.ErrCodexInstructionsAmbiguous
			}
			pb, err := read(filepath.Join(d.Dir, doc.Profile+".config.toml"))
			if err != nil {
				return nil, err
			}
			out = append(out, parity.CodexConfig{Path: filepath.Join(d.Dir, doc.Profile+".config.toml"), Data: pb, Project: true})
		}
	}
	return out, nil
}

// Why the safety policy does not reach a vendor (policyDelivery).
const (
	whyNoSettings   = "claude --help does not list --settings"
	whyNoDeny       = "grok --help does not list --deny"
	whyRulesMissed  = "the Codex rules file is not written; run aggy policy wire"
	whyRulesOther   = "the Codex rules file is not aggy's current policy; undo it with aggy unwire, then run aggy policy wire"
	whyRulesError   = "cannot check the Codex rules file"
	whyNoStandalone = "opencode --help does not list --standalone"
)

// policyDelivery says whether vendor v gets the safety policy, given its
// probed capabilities: "" when it does, else a fixed reason (and the
// error behind it, which may name a path). Claude and Grok take it per
// launch; Codex reads its rules file, which aggy policy wire writes.
func policyDelivery(v adapter.Vendor, caps []string) (why string, err error) {
	switch v {
	case adapter.Claude:
		if !slices.Contains(caps, probe.CapSettings) {
			return whyNoSettings, nil
		}
	case adapter.Grok:
		if !slices.Contains(caps, probe.CapDeny) {
			return whyNoDeny, nil
		}
	case adapter.OpenCode:
		// Rendered per launch (prepareOpenCode), which needs a private
		// server: a shared service keeps the config it started with.
		if !slices.Contains(caps, probe.CapStandalone) {
			return whyNoStandalone, nil
		}
	case adapter.Hermes:
		// Rendered into aggy's profile per launch (prepareHermes).
		if why := hermes.Reachable(caps); why != "" {
			return why, nil
		}
	case adapter.Antigravity:
		// Rendered per launch (prepareAntigravity).
		if why := antigravity.Reachable(caps); why != "" {
			return why, nil
		}
	case adapter.Codex:
		path, err := parity.CodexRulesPath()
		if err != nil {
			return whyRulesError, err
		}
		switch st, err := parity.CodexRulesState(path); {
		case err != nil:
			return whyRulesError, err
		case st == parity.RulesMissing:
			return whyRulesMissed, nil
		case st == parity.RulesOther:
			return whyRulesOther, nil
		}
		// RulesOld refuses where the current file asks: stricter, so it holds.
	}
	return "", nil
}

// planFor is who answers a launch's prompts, which decides how it gets the
// safety policy (policyPlan).
type planFor int

const (
	// planOnScreen: a person sees the session and can answer the CLI's own
	// prompt, so rules ask where the CLI can say ask.
	planOnScreen planFor = iota + 1
	// planHeadless: no one can answer (a borrow child, one reopened on screen,
	// a print or other headless run), so every rule is a deny.
	planHeadless
)

// childPlan is who answers an on-screen launch of a child of kind: a person
// for the session's own child (kind ""), no one for a borrow's child reopened
// on screen. launchOnce uses it for both of its choices, the shared rules
// gateLaunch and addPermissions read and the plan policyPlan makes, so they
// cannot drift apart (PR #125 integration). Print mode and every other
// direct run is planHeadless (runPrint).
func childPlan(kind string) planFor {
	if kind == "" {
		return planOnScreen
	}
	return planHeadless
}

// policyPlan is how a launch in cwd gets the safety policy: on screen, the
// person's group settings from s apply (ask or deny per group); headless,
// every rule is a deny. No allow changes it (parity.PolicyPlan.Decide).
func policyPlan(f planFor, cwd string, s permissions.Set) parity.PolicyPlan {
	p := parity.PolicyPlan{OnScreen: f == planOnScreen}
	if !p.OnScreen {
		return p
	}
	p.Settings = s.Policy
	p.Branches, _ = defaultBranches(cwd)
	return p
}

// defaultBranches is the default branch of dir's repo, when git records one
// (parity.DefaultBranch), for the strict force-push group. known is false
// when the repo's record could not be read.
func defaultBranches(dir string) (branches []string, known bool) {
	if dir == "" {
		return nil, true
	}
	root, ok := repoRoot(dir)
	if !ok {
		return nil, true
	}
	b, known := parity.DefaultBranch(root)
	if b != "" {
		branches = []string{b}
	}
	return branches, known
}

// addPolicy puts the safety policy on req for vendor v where it goes on
// the command line, as plan decides it. When v cannot get it, it fails
// closed: an error when required (the default; only the user's own config
// or --no-policy changes that), else a warning.
func addPolicy(v adapter.Vendor, caps []string, required bool, plan parity.PolicyPlan, req *adapter.InteractiveRequest) ([]string, error) {
	if why, err := policyDelivery(v, caps); why != "" {
		if err != nil {
			why += ": " + err.Error()
		}
		if required {
			return nil, errors.New("the safety policy cannot be applied: " + why + "; fix that, pass --no-policy to start without it, or set required = false under [policy] in aggy's config")
		}
		return []string{"safety policy not applied: " + why}, nil
	}
	switch v {
	case adapter.Claude:
		req.Ask, req.Deny = parity.ClaudePolicy(plan.Decide())
	case adapter.Grok:
		req.Deny = parity.GrokPolicy(plan.Decide())
	}
	return nil, nil
}

// Finding codes of doctor's parity section.
const (
	codeMemoryRefused      = "memory-refused"       // warning: .aggy/memory.md cannot be delivered
	codeMemoryNone         = "memory-none"          // info: the repo has no shared memory
	codeMemoryDelivered    = "memory-delivered"     // info
	codeMemoryNotDelivered = "memory-not-delivered" // warning: this CLI will not get it
	codePolicyApplied      = "policy-applied"       // info
	codePolicyNotWired     = "policy-not-wired"     // info: aggy policy wire has not run
	codePolicyOld          = "policy-old"           // info: the Codex rules file is aggy's older, refusing one
	codePolicyNotApplied   = "policy-not-applied"   // warning: this CLI will not get it
	codePolicyFragile      = "policy-fragile"       // warning: another Codex rules file can switch aggy's off
	codeGrokClaudeMCP      = "grok-claude-mcp"      // warning: Grok starts Claude's stdio MCP servers in a repo that excludes it
)

// parityFindings checks what aggy run would give each installed CLI: the
// repo's shared memory (when cwd is in one) and the safety policy, and
// whether the Codex rules file still matches. It only reads. Messages are
// fixed text; a path goes in Path and an error in Detail, which
// doctor --report leaves out.
//
// Hooks have their own finding in each vendor's section (hookFinding).
// TODO(T6681): instructions (nothing wires the Codex CLAUDE.md fallback
// yet) and MCP entries (no MCP wiring exists yet) are not checked.
func parityFindings(cwd string, vendors []checkup.Vendor) []checkup.Finding {
	var out []checkup.Finding
	add := func(code string, l checkup.Level, v adapter.Vendor, path, msg string, err error) {
		f := checkup.Finding{Code: code, Level: l, Vendor: v, Path: path, Message: msg}
		if err != nil {
			f.Detail = err.Error()
		}
		out = append(out, f)
	}
	memory, root := "", ""
	if cwd != "" {
		root, _ = repoRoot(cwd)
		var err error
		switch memory, err = parity.LoadMemory(root); {
		case errors.Is(err, parity.ErrMemoryTooBig):
			add(codeMemoryRefused, checkup.LevelWarn, "", parity.MemoryPath(root), "the shared memory is over the 4 KB cap, so no CLI gets it", nil)
		case err != nil:
			add(codeMemoryRefused, checkup.LevelWarn, "", parity.MemoryPath(root), "the shared memory cannot be read safely, so no CLI gets it", err)
		case memory == "":
			add(codeMemoryNone, checkup.LevelInfo, "", "", "this repo has no shared memory (.aggy/memory.md)", nil)
		}
	}
	for _, vr := range vendors {
		if !vr.Installed {
			continue
		}
		v, name := vr.Vendor, vendorNames[vr.Vendor]
		if memory != "" {
			ok, why := memoryDelivery(v, vr.Capabilities)
			var err error
			if ok && v == adapter.Codex {
				var layers []parity.CodexConfig
				if layers, err = codexConfigs(cwd, root); err == nil {
					_, err = parity.CodexInstructions(layers, memory)
				}
				ok, why = err == nil, "cannot read its config to keep its own developer_instructions"
				if errors.Is(err, parity.ErrCodexInstructionsAmbiguous) {
					why, err = "a profile or project config sets developer_instructions, which aggy will not replace", nil
				}
			}
			if ok {
				add(codeMemoryDelivered, checkup.LevelInfo, v, "", name+" gets the shared memory at session start", nil)
			} else {
				add(codeMemoryNotDelivered, checkup.LevelWarn, v, "", name+" does not get the shared memory: "+why, err)
			}
		}
		if v == adapter.Codex {
			// Without a cwd, only the system and user folders: HOME's
			// .codex is the user folder, which the tree walk skips.
			dir, top := cwd, root
			if cwd == "" {
				dir, top = os.Getenv("HOME"), os.Getenv("HOME")
			}
			others, err := parity.OtherCodexRules(dir, top)
			if err != nil {
				add(codePolicyFragile, checkup.LevelWarn, v, "", "cannot list Codex's other rules files", err)
			}
			for _, p := range others {
				add(codePolicyFragile, checkup.LevelWarn, v, p, "Codex loads this rules file with aggy's, and a parse error in it makes Codex drop every rules file, aggy's safety policy included", nil)
			}
			if cwd != "" {
				warnTrust := func(msg string, err error) { add(codeFolderTrust, checkup.LevelWarn, v, "", msg, err) }
				switch level, err := parity.CodexTrustLevel(cwd, root); {
				case err != nil:
					warnTrust("cannot tell whether Codex trusts this folder", err)
				case level == "":
					warnTrust("Codex may ask whether to trust this folder when it opens: its config records no answer. If you decline during a switch, aggy goes back to the CLI before", nil)
				case level != "trusted":
					warnTrust("Codex records this folder as not trusted, so it loads less of the project's config", nil)
				}
			}
		}
		why, err := policyDelivery(v, vr.Capabilities)
		switch why {
		case "":
			add(codePolicyApplied, checkup.LevelInfo, v, "", name+" gets the safety policy", nil)
			if v == adapter.Codex {
				if path, err := parity.CodexRulesPath(); err == nil {
					if st, _ := parity.CodexRulesState(path); st == parity.RulesOld {
						add(codePolicyOld, checkup.LevelInfo, v, "", "Codex has aggy's older rules file, which refuses where the current one asks; aggy policy wire updates it", nil)
					}
				}
			}
		case whyRulesMissed:
			add(codePolicyNotWired, checkup.LevelInfo, v, "", name+" does not get the safety policy: "+why, nil)
		default:
			add(codePolicyNotApplied, checkup.LevelWarn, v, "", name+" does not get the safety policy: "+why, err)
		}
		if v == adapter.Grok && root != "" {
			// G35-2: allowed_vendors cannot keep Grok from starting the
			// stdio MCP servers in ~/.claude.json, which it loads itself.
			rc, err := config.LoadRepo(root)
			if err != nil || rc.AllowedVendors == nil || slices.Contains(rc.AllowedVendors, adapter.Grok) {
				continue
			}
			switch on, err := parity.GrokClaudeStdio(root, os.Getenv); {
			case err != nil:
				add(codeGrokClaudeMCP, checkup.LevelWarn, v, "", "cannot tell whether Grok Build starts Claude's stdio MCP servers in this repo, which allowed_vendors excludes it from", err)
			case on:
				add(codeGrokClaudeMCP, checkup.LevelWarn, v, "", "Grok Build starts the stdio MCP servers in ~/.claude.json itself, so this repo's allowed_vendors cannot keep them from Grok; set mcps = false under [compat.claude] in Grok's config.toml to stop that", nil)
			}
		}
	}
	return out
}

// memorySuffix names a launch's memory folder in the run dir:
// <parent-id>.<nonce>.memory, beside the supervisor's <parent-id>.lock.
// Each launch has its own (D19-8): two launches of one parent racing to
// its lock must not remove each other's folder.
const memorySuffix = ".memory"

// memoryDir is a new memory folder name for a launch of session parentID.
func memoryDir(runDir, parentID string) (string, error) {
	n, err := newUUID()
	if err != nil {
		return "", err
	}
	return filepath.Join(runDir, parentID+"."+n[:8]+memorySuffix), nil
}

// memoryParent is the parent id a memory folder name belongs to:
// <uuid>.<8 hex>.memory, or <uuid>.memory from before per-launch names.
func memoryParent(name string) (string, bool) {
	base, ok := strings.CutSuffix(name, memorySuffix)
	if !ok {
		return "", false
	}
	id, nonce, hasNonce := strings.Cut(base, ".")
	if hasNonce && (len(nonce) != 8 || strings.Trim(nonce, "0123456789abcdef") != "") {
		return "", false
	}
	return id, adapter.IsUUID(id)
}

// staleAfter is how old a memory folder must be before a sweep may take
// it: longer than any launch takes to reach its supervisor's lock.
const staleAfter = time.Minute

// sweepMemoryDirs removes the memory folders that sessions killed before
// their cleanup left in runDir (aggy's own 0700 run dir, checked by
// supervisor.PrivateDir): a <uuid>.<nonce>.memory (or <uuid>.memory)
// folder, not a symlink, older than staleAfter, whose parent's supervisor
// does not hold its lock. Errors are
// ignored; the next run tries again.
func sweepMemoryDirs(runDir string, now time.Time) {
	des, err := safefs.ReadDir(runDir)
	if err != nil {
		return
	}
	for _, de := range des {
		id, ok := memoryParent(de.Name())
		if !ok || !de.IsDir() {
			continue
		}
		dir := filepath.Join(runDir, de.Name())
		fi, err := safefs.Lstat(dir)
		if err != nil || !fi.IsDir() || now.Sub(fi.ModTime()) < staleAfter {
			continue
		}
		if live, err := supervisor.Live(runDir, id); err == nil && !live {
			_ = safefs.RemoveAll(dir)
		}
	}
}
