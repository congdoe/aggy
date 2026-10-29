package cli

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/switchboard"
)

// sessionToolSet is what of the managed tools a session of vendor v in
// cwd, with the session tools (--tool) and the CLI's probed caps, gets
// from the settings now (T6729): per tool, "mcp" and "hook" where the
// launch would wire them, "proxy" where its confirmed proxy is on for v
// (T6780: a running CLI keeps the env it started with, so turning one on
// or off is offered as a resume too), and "skill" where aggy tool on|off linked it
// for v (for every repo, whatever the repo's tools_off: L78-8). A running
// session whose set differs from its launch's is offered a resume.
//
// It decides from the settings only; the resume runs the launch's wiring
// again (addTools), with every check: the digest, tools_off, the skill
// pin, the name clashes. Grok gets nothing wired while its tools are
// gated (grokToolsVerified), so its set is always empty and it is never
// offered a resume.
//
// TODO(T6729): whether a running CLI picks up a newly linked skill
// without a restart is unverified for all three, so a skill counts as
// needing a resume (docs/verification.md, "Tool reload").
func sessionToolSet(ctx context.Context, v adapter.Vendor, caps []string, cwd string, session []string) (supervisor.ToolSet, error) {
	out := supervisor.ToolSet{}
	if v == adapter.Grok && !grokToolsVerified {
		return out, nil
	}
	repo, _ := toolRepo(cwd, false)
	b, _, _, err := toolBoard(ctx)
	if err != nil {
		return nil, err
	}
	if repo.root != "" {
		// As the launch: a repo config that cannot be read wires nothing.
		if b.RepoOff, err = repoToolsOff(repo); err != nil {
			return nil, err
		}
	}
	add := func(name, part string) {
		if out[name] == "" {
			out[name] = part
		} else {
			out[name] += "+" + part
		}
	}
	skills := switchboard.Board{Tools: b.Tools, Switches: b.Switches, Unadded: b.Unadded} // as syncToolSkill links
	b.Session = session
	for _, st := range b.On(v, repo.key) {
		if st.Parts.MCP != nil && (v != adapter.Codex || slices.Contains(caps, probe.CapConfigOverride)) {
			add(st.Tool.Name, "mcp")
		}
		if len(st.Parts.Hooks) > 0 && v == adapter.Claude && slices.Contains(caps, probe.CapSettings) {
			add(st.Tool.Name, "hook")
		}
		if st.Parts.Proxy != nil { // T6780: its env is fixed at launch
			add(st.Tool.Name, "proxy")
		}
	}
	for _, t := range b.Tools {
		if t.PartsFor(v).Skill != nil && skills.State(t, v, "").On {
			add(t.Name, "skill")
		}
	}
	return out, nil
}

// notifySessions tells every running supervisor that the tool settings
// changed (T6729), through its socket, as aggy switch reaches it. Each
// works out itself whether its own tools changed and, if so, offers the
// user a resume at the turn end; nothing restarts without the user's
// key. A session of another vendor, or whose repo the change does not
// reach, is not told: vendor is the setting's ("" for every vendor), and
// repoKey its repo ("" for every repo). It returns how many were told.
// Errors are left out: a session that cannot be reached gets the tools at
// its next launch.
func notifySessions(ctx context.Context, vendor, repoKey string) int {
	dir, err := home.Dir()
	if err != nil {
		return 0
	}
	runDir := filepath.Join(dir, "run")
	des, err := safefs.ReadDir(runDir)
	if err != nil {
		return 0
	}
	s, _, err := openIndexReadOnly(ctx)
	if err != nil || s == nil {
		return 0
	}
	defer func() { _ = s.Close() }()
	told, failed := 0, 0
	for _, de := range des {
		id, ok := strings.CutSuffix(de.Name(), ".sock")
		if !ok || !adapter.IsUUID(id) {
			continue
		}
		if live, err := supervisor.Live(runDir, id); err != nil || !live {
			continue
		}
		if !sessionMatches(ctx, s, id, vendor, repoKey) {
			continue
		}
		resp, err := supervisor.Send(runDir, filepath.Join(runDir, de.Name()), supervisor.Request{Type: supervisor.ReqToolsChanged}, hookTimeout)
		if err != nil || !resp.OK {
			failed++
			continue
		}
		told++
	}
	if told+failed > 0 {
		dlog.Log(ctx, diag.Rec{Event: diag.EvToolsNotified, Attrs: map[string]any{"sessions": told, "failed": failed}})
	}
	return told
}

// sessionMatches reports whether the parent id's session may be reached
// by a setting for vendor and repoKey: its vendor on screen (the
// parent's last) is that vendor, and its directory in that repo. A
// parent the index does not have is not told.
func sessionMatches(ctx context.Context, s *store.Store, id, vendor, repoKey string) bool {
	p, err := s.GetParent(ctx, id)
	if err != nil {
		return false
	}
	if vendor != "" && p.LastVendor != vendor {
		return false
	}
	if repoKey == "" {
		return true
	}
	r, err := toolRepo(p.Dir, false)
	return err == nil && r.key == repoKey
}
