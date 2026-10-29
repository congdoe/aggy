package cli

import (
	"context"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/store"
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
	if prog != "" {
		return "no: " + prog + " not on PATH"
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
// when there is none or it cannot be read), at schema version ver.
func toolReady(ctx context.Context, t tooldef.Tool, s *store.Store, ver int) string {
	var parts []string
	if len(t.Install.Prebuild) > 0 {
		// TODO(T6726): read CodeGraph's index marker once PR #88 lands.
		parts = append(parts, "index n/a")
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
