package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
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

// toolProblem is one tool finding. A user tool's name is the person's
// own choice, so it and the fix naming it go in Detail, which a public
// report leaves out; a built-in's are in Message.
func toolProblem(t tooldef.Tool, v adapter.Vendor, code string, l checkup.Level, msg, fix string) checkup.Finding {
	f := checkup.Finding{Code: code, Level: l, Vendor: v}
	if fix != "" {
		fix = "; fix: " + fix
	}
	if t.BuiltIn {
		f.Message = t.Name + ": " + msg + fix
	} else {
		f.Message, f.Detail = "a user tool: "+msg, "tool "+t.Name+fix
	}
	return f
}

// boardFindings checks board b (definitions, settings, and the repo's
// tools_off) in repo; refused are the user definitions Load left out.
func boardFindings(ctx context.Context, b switchboard.Board, refused []error, cwd string, repo toolRepoAt) []checkup.Finding {
	var out []checkup.Finding
	warn := func(t tooldef.Tool, v adapter.Vendor, code diag.Code, msg, fix string) {
		out = append(out, toolProblem(t, v, code.String(), checkup.LevelWarn, msg, fix))
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
		if t.BuiltIn {
			continue
		}
		if s != nil && ver >= toolAddedSchema {
			if err := checkAdded(ctx, s, t, true); err != nil {
				warn(t, "", codeOf(err, diag.CodeToolChanged), "its file is not the one aggy tool add showed, so it stays off", "aggy tool remove "+t.Name+", then aggy tool add the file again and read what it runs")
			}
		}
	}

	// Per vendor: what is on is wireable.
	bin, _ := toolHookBin() // "" on error: a Claude hook is then refused, as at launch
	on := map[string][]adapter.Vendor{}
	for _, v := range adapter.Vendors() {
		states := b.On(v, repo.key)
		for _, st := range states {
			on[st.Tool.Name] = append(on[st.Tool.Name], v)
		}
		// R80-3, as a launch checks it first: a tool whose linked skill
		// changed since its first link gets no server or hook wired.
		changed := changedSkills(ctx, states)
		states = slices.DeleteFunc(states, func(st switchboard.State) bool {
			err, ok := changed[st.Tool.Name]
			if ok {
				// Fixed text: the error names the folder's path.
				code, why := codeOf(err, diag.CodeToolSkillChanged), "its skill folder changed since it was first linked"
				if code != diag.CodeToolSkillChanged {
					why = "its skill folder cannot be checked against its pin"
				}
				warn(st.Tool, v, code, "not wired into "+string(v)+": "+why,
					"aggy tool off "+st.Tool.Name+", read its skill folder, then aggy tool on "+st.Tool.Name)
			}
			return ok
		})
		if v == adapter.Grok && !grokToolsVerified {
			for _, st := range states {
				if st.Parts.MCP != nil || len(st.Parts.Hooks) > 0 {
					out = append(out, toolProblem(st.Tool, v, diag.CodeToolUnverified.String(), checkup.LevelInfo,
						"on for grok, not wired: unverified (what Grok's --agent changes is not verified yet)", ""))
				}
			}
			continue
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
			warn(states[i].Tool, v, r.Code, "not wired into "+string(v)+": "+r.Why, refusalFix(r, v))
		}
	}

	// Program: each on tool's programs resolve. A loaded definition has
	// passed the program checks (tooldef's validator) already.
	for _, t := range b.Tools {
		if len(on[t.Name]) == 0 {
			continue
		}
		var progs []string
		for _, v := range on[t.Name] {
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
				warn(t, "", diag.CodeToolExec, "its program "+prog+" is not an executable on PATH, so it cannot start", "install "+t.Name+" "+t.Version+", or aggy tool off "+t.Name)
			}
		}
	}

	out = append(out, skillFindings(ctx, b, s, ver)...)
	if len(out) == 0 {
		msg := "no managed tool is on"
		if len(on) > 0 {
			var names []string
			for _, t := range b.Tools {
				if len(on[t.Name]) > 0 {
					names = append(names, t.Name)
				}
			}
			msg = fmt.Sprintf("%d %s on, and wireable", len(names), plural(len(names), "tool is", "tools are"))
		}
		out = append(out, checkup.Finding{Code: codeToolsOK, Level: checkup.LevelInfo, Message: msg})
	}
	return out
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
// folder to a tool no longer loaded is stray too. s is the index opened
// read-only (nil when there is none), at schema version ver.
func skillFindings(ctx context.Context, b switchboard.Board, s *store.Store, ver int) []checkup.Finding {
	var out []checkup.Finding
	warn := func(t tooldef.Tool, v adapter.Vendor, code, msg, fix string) {
		out = append(out, toolProblem(t, v, code, checkup.LevelWarn, msg, fix))
	}
	loaded := []string{skillName}
	for _, t := range b.Tools {
		loaded = append(loaded, t.Name)
		if !hasSkill(t) {
			continue
		}
		l, err := toolSkillLayout(t)
		if err != nil {
			warn(t, "", diag.CodeToolSkillMissing.String(), "its skill cannot be linked: "+err.Error(), "")
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
				warn(t, v, diag.CodeToolFailed.String(), "its skill link cannot be read: "+err.Error(), "")
			case linked[v] && !want:
				warn(t, v, codeToolStray, "off for "+string(v)+", but its skill is still linked, so "+string(v)+" loads it", "aggy tool off "+t.Name+" --vendor "+string(v))
			case !linked[v] && want && v == adapter.Grok:
				warn(t, v, codeToolUnlinked, "on for grok, but its skill is not linked", "aggy tool on "+t.Name)
			case !linked[v] && want:
				warn(t, v, codeToolUnlinked, "on for "+string(v)+", but its skill is not linked", "aggy tool on "+t.Name+" --vendor "+string(v))
			}
		}
		if anyOn {
			if err := checkSkillStore(l.store); err != nil {
				warn(t, "", diag.CodeToolSkillMissing.String(), "its skill folder is missing or refused",
					"copy its skill folder into ~/.aggy/skills/"+t.Name+" (a plain folder holding SKILL.md, no symlinks), then aggy tool on "+t.Name)
			} else if t.SkillOnly() && s != nil && ver >= toolSkillSchema {
				// A tool with a server or hook is reported per vendor
				// above, as the launch refuses it (changedSkills).
				was, ok, err := s.ToolSkill(ctx, t.Name)
				now, derr := skillDigest(l.store)
				if err == nil && derr == nil && ok && was != now {
					warn(t, "", diag.CodeToolSkillChanged.String(), "its skill folder changed since it was first linked, and every linked CLI loads it as it is now",
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
			warn(t, adapter.Grok, codeToolGrokIgnore, "Grok's config cannot be read to check its [skills] ignore: "+err.Error(), "")
		case want && !has:
			warn(t, adapter.Grok, codeToolGrokIgnore, "off for grok, but [skills] ignore lacks its skill, so grok loads it through the Claude or Codex link", "aggy tool off "+t.Name+" --vendor grok")
		case !want && has:
			warn(t, adapter.Grok, codeToolGrokIgnore, "[skills] ignore hides its skill although no Claude or Codex link needs it", "aggy tool off "+t.Name+" --vendor grok, or aggy tool on "+t.Name)
		}
	}

	// Links of aggy's to a tool no longer loaded.
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
			if slices.Contains(loaded, n) || !tooldef.ValidName(n) {
				continue
			}
			nl, err := layoutFor(n)
			if ok, _ := nl.linked(filepath.Join(dir, n)); err == nil && ok {
				warn(tooldef.Tool{Def: tooldef.Def{Name: n}}, v, codeToolStray, "no tool of this name is loaded, but aggy's link to its skill stays, so "+string(v)+" loads it", "aggy tool off "+n)
			}
		}
	}
	return out
}
