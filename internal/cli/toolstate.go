package cli

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
)

// T6726: aggy tool status shows, per tool, whether it is installed and
// ready, and per vendor whether it is wired. Tools stay installed and
// ready while off; on and off only rewire. aggy never installs, builds
// an index, or downloads anything here: it only reads.

// wireProblems is, per tool and vendor ("" for every vendor), the code
// of the first doctor finding that keeps it from being wired. A finding
// with no tool but a vendor (the CLI's own MCP config cannot be read)
// is under tool "". Info findings are not problems, except Grok's gate.
func wireProblems(fs []checkup.Finding) map[string]map[adapter.Vendor]string {
	out := map[string]map[adapter.Vendor]string{}
	for _, f := range fs {
		if f.Level == checkup.LevelInfo && f.Code != diag.CodeToolUnverified.String() {
			continue
		}
		if f.Tool == "" && (f.Vendor == "" || f.Code != diag.CodeToolWireFailed.String()) {
			continue
		}
		if out[f.Tool] == nil {
			out[f.Tool] = map[adapter.Vendor]string{}
		}
		if _, ok := out[f.Tool][f.Vendor]; !ok {
			out[f.Tool][f.Vendor] = f.Code
		}
	}
	return out
}

// toolInstalled is "yes" when every program t runs (its install check,
// MCP servers, and hooks) is on PATH as a regular executable, and its
// skill folder is in place and checks out; otherwise "no: " and what is
// missing.
func toolInstalled(t tooldef.Tool) string {
	prog := missingProgram(t)
	if prog == "" && len(t.Install.Check) > 0 && resolveProgram(t.Install.Check[0]) != nil {
		prog = t.Install.Check[0]
	}
	switch {
	case prog != "" && filepath.IsAbs(prog):
		return "no: " + termsafe.Field(prog) + " is not an executable file"
	case prog != "":
		return "no: " + termsafe.Field(prog) + " not on PATH"
	}
	if hasSkill(t) {
		l, err := toolSkillLayout(t)
		if err == nil {
			err = checkSkillStore(l.store)
		}
		if err != nil {
			return "no: skill folder"
		}
	}
	return "yes"
}

// toolReady says whether what t needs before it is used is in place: a
// per-repo index for a tool with a prebuild step, and a skill folder
// that still hashes to its pin. s is the index opened read-only (nil
// when there is none or it cannot be read), at schema version ver; repo
// is the repo status looks at.
func toolReady(ctx context.Context, t tooldef.Tool, s *store.Store, ver int, repo toolRepoAt) string {
	var parts []string
	if len(t.Install.Prebuild) > 0 {
		parts = append(parts, indexState(ctx, t, repo))
	}
	if hasSkill(t) {
		parts = append(parts, skillPinState(ctx, t, s, ver))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "; ")
}

func skillPinState(ctx context.Context, t tooldef.Tool, s *store.Store, ver int) string {
	if s == nil || ver < toolSkillSchema {
		return "no pin"
	}
	was, ok, err := s.ToolSkill(ctx, t.Name)
	switch {
	case err != nil:
		return "pin unknown"
	case !ok:
		return "no pin"
	}
	l, err := toolSkillLayout(t)
	if err == nil {
		err = checkSkillStore(l.store)
	}
	if err != nil {
		return "pin unknown"
	}
	now, err := skillDigest(l.store)
	switch {
	case err != nil:
		return "pin unknown"
	case now != was:
		return "pin changed"
	}
	return "pin ok"
}

// indexState is whether the main checkout of repo has t's finished index:
// its folder a real folder holding its database and aggy's marker, as
// aggy tool on --repo checks before it builds one (T6711).
func indexState(ctx context.Context, t tooldef.Tool, repo toolRepoAt) string {
	ix, ok := toolIndexes[t.Name]
	switch {
	case !ok:
		return "index n/a"
	case repo.main == "":
		return "index n/a outside a repo"
	}
	dir := filepath.Join(repo.main, ix.dir)
	if _, err := indexDir(dir); err == nil && regular(filepath.Join(dir, indexMarker)) && regular(filepath.Join(dir, ix.db)) {
		switch committed, unsure := indexOwner(ctx, t.Name, repo, ix); {
		case committed:
			return "index committed in the repo (not built by aggy)"
		case unsure:
			return "indexed (git could not check)"
		}
		return "indexed"
	}
	return "not indexed"
}
