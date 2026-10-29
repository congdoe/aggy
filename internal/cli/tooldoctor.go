package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/tooldef"
	"github.com/congdoe/aggy/internal/toolwire"
)

// Doctor codes for tool findings no reason code names; every other tool
// finding carries the reason code a launch or aggy tool would refuse with.
const (
	codeToolsOK        = "tools-ok"            // info: on, and wireable
	codeToolCost       = "tool-cost"           // info: an on tool's estimated tokens per vendor (toolCost)
	codeToolStray      = "tool-skill-stray"    // warning: an off tool's skill is still linked
	codeToolUnlinked   = "tool-skill-unlinked" // warning: an on tool's skill is not linked
	codeToolGrokIgnore = "tool-grok-ignore"    // warning: Grok's [skills] ignore does not match the links
)

// toolFindings is aggy doctor's check of the managed tools against the
// switches, per tool and vendor, in the repo holding cwd ("" is outside
// a repo): what is on is wireable, and what is off leaves no trace. It
// only reads: the definitions, the index (read-only), the CLIs' MCP
// config, and the skill folders. It fixes nothing; each problem is one
// line with its reason code and the command that fixes it.
func toolFindings(ctx context.Context, cwd string) []checkup.Finding {
	b, _, refused, err := toolBoard(ctx)
	if err != nil {
		return []checkup.Finding{{Code: diag.CodeToolFailed.String(), Level: checkup.LevelWarn,
			Message: "cannot read the tool definitions or settings", Detail: err.Error()}}
	}
	var repo toolRepoAt
	if cwd != "" {
		repo, _ = toolRepo(cwd, false)
	}
	var out []checkup.Finding
	if repo.root != "" {
		if b.RepoOff, err = repoToolsOff(repo); err != nil {
			out = append(out, checkup.Finding{Code: diag.CodeConfig.String(), Level: checkup.LevelWarn,
				Message: "the repo's .aggy/config.toml cannot be read, so every tool is off here; fix the file", Detail: err.Error()})
			for _, t := range b.Tools {
				b.RepoOff = append(b.RepoOff, t.Name)
			}
		}
	}
	return append(out, boardFindings(ctx, b, refused, cwd, repo)...)
}

// toolProblem is one tool finding. msg is aggy's fixed text; detail is
// anything else (an error, a path, a name from a config), and fix the
// command that clears it. A public report prints Message only (H83-1):
// a built-in's Message is its name, msg, and fix; a user tool's name is
// the person's own choice, so its Message is msg alone, and its name,
// the detail, and the fix go in Detail.
func toolProblem(t tooldef.Tool, v adapter.Vendor, code string, l checkup.Level, msg, detail, fix string) checkup.Finding {
	f := checkup.Finding{Code: code, Level: l, Vendor: v, Tool: t.Name}
	if fix != "" {
		fix = "fix: " + fix
	}
	if t.BuiltIn {
		f.Message, f.Detail = joinNonEmpty(t.Name+": "+msg, fix), detail
	} else {
		f.Message, f.Detail = "a user tool: "+msg, joinNonEmpty("tool "+t.Name, detail, fix)
	}
	return f
}

func joinNonEmpty(parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), "; ")
}

// refusalText is the fixed text for a launch's refusal code.
var refusalText = map[diag.Code]string{
	diag.CodeToolNameClash:    "a server of its name is already set",
	diag.CodeToolRepoPath:     "the repo's path cannot go in its arguments",
	diag.CodeToolHookApproves: "its hook approves actions you would be asked about",
	diag.CodeToolNoFlag:       "the CLI takes no hook for one session",
	diag.CodeToolWireFailed:   "its hook command cannot be built",
}

// boardFindings checks board b (definitions, settings, and the repo's
// tools_off) in repo; refused are the user definitions Load left out.
func boardFindings(ctx context.Context, b switchboard.Board, refused []error, cwd string, repo toolRepoAt) []checkup.Finding {
	var out []checkup.Finding
	warn := func(t tooldef.Tool, v adapter.Vendor, code diag.Code, msg, detail, fix string) {
		out = append(out, toolProblem(t, v, code.String(), checkup.LevelWarn, msg, detail, fix))
	}

	// Definition: a refused file is off; an added one is still the file
	// aggy tool add showed (S63-16).
	for _, e := range refused {
		name := "<name>"
		var te *tooldef.Error
		if errors.As(e, &te) {
			name = strings.TrimSuffix(filepath.Base(te.File), ".toml")
		}
		out = append(out, checkup.Finding{Code: toolCode(e).String(), Level: checkup.LevelWarn,
			Message: "a user definition is refused, so its tool is off", Detail: e.Error() + "; fix: the key it names, or aggy tool remove " + name})
	}
	s, ver, err := openIndexReadOnly(ctx)
	if err != nil {
		out = append(out, checkup.Finding{Code: diag.CodeStore.String(), Level: checkup.LevelWarn,
			Message: "cannot read the index to check the tools' records", Detail: err.Error()})
	}
	if s != nil {
		defer func() { _ = s.Close() }()
	}
	for _, t := range b.Tools {
		if t.BuiltIn || s == nil || ver < toolAddedSchema {
			continue
		}
		// M83-3: aggy tool on takes a file aggy tool add never recorded,
		// and a session's --tool does not; either is fixed by adding the
		// file again, once the tool is off (remove refuses one that is on).
		readd := "aggy tool off " + t.Name + ", aggy tool remove " + t.Name + ", then aggy tool add the file again and read what it runs"
		added, ok, err := s.ToolAdded(ctx, t.Name)
		switch {
		case err != nil:
			warn(t, "", diag.CodeStore, "its aggy tool add record cannot be read", err.Error(), "")
		case !ok:
			warn(t, "", diag.CodeToolChanged, "aggy tool add has no record of its file, so a session's --tool refuses it", "", readd)
		case added != t.Digest:
			warn(t, "", diag.CodeToolChanged, "its file is not the one aggy tool add showed, so aggy tool on and --tool refuse it", "", readd)
		}
	}

	// Per vendor: what is on is wireable. Servers and hooks follow the
	// switches in this repo.
	bin, _ := toolHookBin() // "" on error: a Claude hook is then refused, as at launch
	wired := map[string][]adapter.Vendor{}
	for _, v := range adapter.Vendors() {
		states := b.On(v, repo.key)
		// R80-3, as a launch checks it first: a tool whose linked skill
		// changed since its first link gets no server or hook wired.
		changed := changedSkills(ctx, states)
		states = slices.DeleteFunc(states, func(st switchboard.State) bool {
			err, ok := changed[st.Tool.Name]
			if ok {
				code, why := codeOf(err, diag.CodeToolSkillChanged), "its skill folder changed since it was first linked"
				if code != diag.CodeToolSkillChanged {
					why = "its skill folder cannot be checked against its pin"
				}
				warn(st.Tool, v, code, "not wired into "+string(v)+": "+why, err.Error(),
					"aggy tool off "+st.Tool.Name+", read its skill folder, then aggy tool on "+st.Tool.Name)
			}
			return ok
		})
		if v == adapter.Grok && !grokToolsVerified {
			for _, st := range states {
				if st.Parts.MCP != nil || len(st.Parts.Hooks) > 0 {
					out = append(out, toolProblem(st.Tool, v, diag.CodeToolUnverified.String(), checkup.LevelInfo,
						"on for grok, not wired: unverified (what Grok's --agent changes is not verified yet)", "", ""))
				}
			}
			continue // L83-10: nothing is wired, so no program runs
		}
		for _, st := range states {
			wired[st.Tool.Name] = append(wired[st.Tool.Name], v)
		}
		var taken map[string]string
		if cwd != "" && slices.ContainsFunc(states, func(st switchboard.State) bool { return st.Parts.MCP != nil }) {
			if taken, err = takenMCPNames(v, cwd, repo.root); err != nil {
				out = append(out, checkup.Finding{Code: diag.CodeToolWireFailed.String(), Level: checkup.LevelWarn, Vendor: v,
					Message: "the CLI's own MCP servers cannot be listed, so no tool is wired into " + string(v) + "; fix the file named", Detail: err.Error()})
				continue
			}
		}
		_, mcpNo := toolwire.MCP(states, repo.root, "/", "aggy", taken)
		_, hookNo := toolwire.Hooks(states, v, bin)
		for _, r := range append(mcpNo, hookNo...) {
			i := slices.IndexFunc(states, func(st switchboard.State) bool { return st.Tool.Name == r.Tool })
			why, ok := refusalText[r.Code]
			if !ok {
				why = "refused"
			}
			warn(states[i].Tool, v, r.Code, "not wired into "+string(v)+": "+why, r.Why, refusalFix(r, v))
		}
	}

	// Program: each wired tool's programs resolve. A loaded definition
	// has passed the program checks (tooldef's validator) already.
	for _, t := range b.Tools {
		var progs []string
		for _, v := range wired[t.Name] {
			p := t.PartsFor(v)
			if p.MCP != nil {
				progs = append(progs, p.MCP.Argv[0])
			}
			for _, h := range p.Hooks {
				progs = append(progs, h.Argv[0])
			}
		}
		slices.Sort(progs)
		for _, prog := range slices.Compact(progs) {
			if err := resolveProgram(prog); err != nil {
				warn(t, "", diag.CodeToolExec, "a program it runs is not an executable on PATH, so it cannot start", prog+": "+err.Error(),
					installHowTo(t)+", or aggy tool off "+t.Name)
			}
		}
	}

	out = append(out, skillFindings(ctx, b, s, ver)...)
	problems := slices.ContainsFunc(out, func(f checkup.Finding) bool { return f.Level != checkup.LevelInfo })
	n := 0
	blocked := skillBlocked(ctx, b, repo.key)
	for _, t := range b.Tools {
		var costs []string
		anyOn := false
		for _, v := range adapter.Vendors() {
			skillOn, partsOn := toolOnFor(b, t, v, repo.key, blocked)
			anyOn = anyOn || skillOn || partsOn
			costs = append(costs, string(v)+" "+toolCost(t, v, skillOn, partsOn))
		}
		if !anyOn {
			continue
		}
		n++
		out = append(out, toolProblem(t, "", codeToolCost, checkup.LevelInfo, "estimated tokens every turn (+ more on invoke): "+strings.Join(costs, ", "), "", ""))
	}
	if !problems {
		msg := "no managed tool is on"
		if n > 0 {
			msg = fmt.Sprintf("%d %s on, and wireable", n, plural(n, "tool is", "tools are"))
		}
		out = append(out, checkup.Finding{Code: codeToolsOK, Level: checkup.LevelInfo, Message: msg})
	}
	return out
}

// missingProgram is the first program among t's MCP servers and hooks
// that does not resolve as a launch would find it, or "".
func missingProgram(t tooldef.Tool) string {
	for _, v := range adapter.Vendors() {
		p := t.PartsFor(v)
		var progs []string
		if p.MCP != nil {
			progs = append(progs, p.MCP.Argv[0])
		}
		for _, h := range p.Hooks {
			progs = append(progs, h.Argv[0])
		}
		for _, prog := range progs {
			if resolveProgram(prog) != nil {
				return prog
			}
		}
	}
	return ""
}

// installHowTo says how to install t's pinned version by hand: its
// definition's install.hint when it has one. aggy never installs. A
// user tool's hint is the file author's, not aggy's advice, so it is
// quoted and attributed (#86).
func installHowTo(t tooldef.Tool) string {
	s := "install " + t.Name + " " + t.Version + " by hand"
	switch {
	case t.Install.Hint == "":
		return s
	case t.BuiltIn:
		return s + " (" + t.Install.Hint + ")"
	}
	return s + "; " + userHint(t.Install.Hint)
}

// userHint attributes a user definition's install.hint to the file.
func userHint(h string) string {
	return "the tool's definition, not aggy, says: " + strconv.Quote(h)
}

// refusalFix is the command that clears a launch's refusal r for v.
func refusalFix(r toolwire.Refusal, v adapter.Vendor) string {
	switch r.Code {
	case diag.CodeToolNameClash:
		return "rename that server in the CLI's config, or aggy tool off " + r.Tool + " --vendor " + string(v)
	case diag.CodeToolRepoPath:
		return "run the session in a repo whose path has only letters, digits, and / . _ + @ -"
	}
	return "aggy tool off " + r.Tool + " --vendor " + string(v)
}

// resolveProgram finds prog as a launch does: an absolute path, or a
// bare name on PATH (probe.LookPath); either must be a regular,
// executable file.
func resolveProgram(prog string) error {
	if !filepath.IsAbs(prog) {
		_, err := probe.LookPath(prog)
		return err
	}
	fi, err := safefs.Stat(prog)
	if err == nil && (!fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0) {
		err = errors.New("not an executable file")
	}
	return err
}

// skillFindings checks every tool's skill links against its switches
// for every repo, as aggy tool on and off set them (syncToolSkill): a
// vendor's link is there exactly when the tool is on for it, the folder
// is in place and still the one first linked (L78-4), and Grok's
// [skills] ignore holds the folder exactly when Grok is unlinked and a
// Claude or Codex link stays. A link of aggy's in a vendor's skill
// folder to a tool no longer loaded, or to one whose definition has no
// skill now (H83-2), is stray too. s is the index opened read-only (nil
// when there is none), at schema version ver.
func skillFindings(ctx context.Context, b switchboard.Board, s *store.Store, ver int) []checkup.Finding {
	var out []checkup.Finding
	warn := func(t tooldef.Tool, v adapter.Vendor, code, msg, detail, fix string) {
		out = append(out, toolProblem(t, v, code, checkup.LevelWarn, msg, detail, fix))
	}
	withSkill := []string{skillName}
	for _, t := range b.Tools {
		if !hasSkill(t) {
			continue
		}
		withSkill = append(withSkill, t.Name)
		l, err := toolSkillLayout(t)
		if err != nil {
			warn(t, "", diag.CodeToolSkillMissing.String(), "its skill cannot be linked", err.Error(), "")
			continue
		}
		linked := map[adapter.Vendor]bool{}
		anyOn := false
		for _, v := range adapter.Vendors() {
			want := t.PartsFor(v).Skill != nil && b.State(t, v, "").On
			anyOn = anyOn || want
			linked[v], err = l.linked(l.links[string(v)])
			switch {
			case err != nil:
				warn(t, v, diag.CodeToolFailed.String(), "its skill link cannot be read", err.Error(), "")
			case linked[v] && !want:
				warn(t, v, codeToolStray, "off for "+string(v)+", but its skill is still linked, so "+string(v)+" loads it", "", "aggy tool off "+t.Name+" --vendor "+string(v))
			case !linked[v] && want && v == adapter.Grok:
				warn(t, v, codeToolUnlinked, "on for grok, but its skill is not linked", "", "aggy tool on "+t.Name)
			case !linked[v] && want:
				warn(t, v, codeToolUnlinked, "on for "+string(v)+", but its skill is not linked", "", "aggy tool on "+t.Name+" --vendor "+string(v))
			}
		}
		if anyOn {
			if err := checkSkillStore(l.store); err != nil {
				warn(t, "", diag.CodeToolSkillMissing.String(), "its skill folder is missing or refused", err.Error(),
					"copy its skill folder into ~/.aggy/skills/"+t.Name+" (a plain folder holding SKILL.md, no symlinks), then aggy tool on "+t.Name)
			} else if t.SkillOnly() && s != nil && ver >= toolSkillSchema {
				// A tool with a server or hook is reported per vendor
				// above, as the launch refuses it (changedSkills).
				was, ok, err := s.ToolSkill(ctx, t.Name)
				now, derr := skillDigest(l.store)
				if err == nil && derr == nil && ok && was != now {
					warn(t, "", diag.CodeToolSkillChanged.String(), "its skill folder changed since it was first linked, and every linked CLI loads it as it is now", "",
						"aggy tool off "+t.Name+", read the folder, then aggy tool on "+t.Name)
				}
			}
		}
		if _, err := safefs.Lstat(l.grokHome); err != nil {
			continue // no Grok home: aggy writes no ignore entry
		}
		cfg, _, err := readMaybe(l.grokConfig)
		var has bool
		if err == nil {
			has, err = grokIgnores(cfg, l.store)
		}
		want := !linked[adapter.Grok] && (linked[adapter.Claude] || linked[adapter.Codex])
		switch {
		case err != nil:
			warn(t, adapter.Grok, codeToolGrokIgnore, "Grok's config cannot be read to check its [skills] ignore", err.Error(), "")
		case want && !has:
			warn(t, adapter.Grok, codeToolGrokIgnore, "off for grok, but [skills] ignore lacks its skill, so grok loads it through the Claude or Codex link", "", "aggy tool off "+t.Name+" --vendor grok")
		case !want && has:
			warn(t, adapter.Grok, codeToolGrokIgnore, "[skills] ignore hides its skill although no Claude or Codex link needs it", "", "aggy tool off "+t.Name+" --vendor grok, or aggy tool on "+t.Name)
		}
	}

	// Links of aggy's to a tool not loaded, or loaded without a skill.
	l, err := layoutFor(skillName)
	if err != nil {
		return out
	}
	for _, v := range adapter.Vendors() {
		dir := filepath.Dir(l.links[string(v)])
		des, err := safefs.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			out = append(out, checkup.Finding{Code: diag.CodeToolFailed.String(), Level: checkup.LevelWarn, Vendor: v, Path: dir,
				Message: "cannot read the skill folder to check for stray links", Detail: err.Error()})
			continue
		}
		for _, de := range des {
			n := de.Name()
			if slices.Contains(withSkill, n) || !tooldef.ValidName(n) {
				continue
			}
			nl, err := layoutFor(n)
			if ok, _ := nl.linked(filepath.Join(dir, n)); err == nil && ok {
				t := tooldef.Tool{Def: tooldef.Def{Name: n}}
				if i := slices.IndexFunc(b.Tools, func(t tooldef.Tool) bool { return t.Name == n }); i >= 0 {
					t = b.Tools[i]
				}
				warn(t, v, codeToolStray, "no loaded tool of its name has a skill, but aggy's link to its skill stays, so "+string(v)+" loads it", "", "aggy tool off "+n)
			}
		}
	}
	return out
}
