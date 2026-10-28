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
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/parity"
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
		out = append(out, parity.CodexConfig{Data: b, Project: d.Project})
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
			out = append(out, parity.CodexConfig{Data: pb, Project: true})
		}
	}
	return out, nil
}

// Why the safety policy does not reach a vendor (policyDelivery).
const (
	whyNoSettings  = "claude --help does not list --settings"
	whyNoDeny      = "grok --help does not list --deny"
	whyRulesMissed = "the Codex rules file is not written; run aggy policy wire"
	whyRulesOther  = "the Codex rules file is not aggy's current policy; undo it with aggy unwire, then run aggy policy wire"
	whyRulesError  = "cannot check the Codex rules file"
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
	}
	return "", nil
}

// addPolicy puts the safety policy on req for vendor v where it goes on
// the command line. When v cannot get it, it fails closed: an error when
// required (the default; only the user's own config or --no-policy
// changes that), else a warning.
func addPolicy(v adapter.Vendor, caps []string, required bool, req *adapter.InteractiveRequest) ([]string, error) {
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
		req.Deny = parity.ClaudeDeny
	case adapter.Grok:
		req.Deny = parity.GrokDeny
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
	codePolicyNotApplied   = "policy-not-applied"   // warning: this CLI will not get it
	codePolicyFragile      = "policy-fragile"       // warning: another Codex rules file can switch aggy's off
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
		}
		why, err := policyDelivery(v, vr.Capabilities)
		switch why {
		case "":
			add(codePolicyApplied, checkup.LevelInfo, v, "", name+" gets the safety policy", nil)
		case whyRulesMissed:
			add(codePolicyNotWired, checkup.LevelInfo, v, "", name+" does not get the safety policy: "+why, nil)
		default:
			add(codePolicyNotApplied, checkup.LevelWarn, v, "", name+" does not get the safety policy: "+why, err)
		}
	}
	return out
}

// memorySuffix names a session's memory folder in the run dir:
// <parent-id>.memory, beside the supervisor's <parent-id>.lock.
const memorySuffix = ".memory"

// memoryDir is the memory folder of session parentID.
func memoryDir(runDir, parentID string) string {
	return filepath.Join(runDir, parentID+memorySuffix)
}

// staleAfter is how old a memory folder must be before a sweep may take
// it: longer than any launch takes to reach its supervisor's lock.
const staleAfter = time.Minute

// sweepMemoryDirs removes the memory folders that sessions killed before
// their cleanup left in runDir (aggy's own 0700 run dir, checked by
// supervisor.PrivateDir): a <uuid>.memory folder, not a symlink, older
// than staleAfter, whose supervisor no longer holds its lock. Errors are
// ignored; the next run tries again.
func sweepMemoryDirs(runDir string, now time.Time) {
	des, err := safefs.ReadDir(runDir)
	if err != nil {
		return
	}
	for _, de := range des {
		id, ok := strings.CutSuffix(de.Name(), memorySuffix)
		if !ok || !adapter.IsUUID(id) || !de.IsDir() {
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
