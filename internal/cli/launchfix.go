package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/sessioncmd"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

// What a launch adds when asked (T6982, docs/design.md section 5): when aggy's
// hooks for the CLI being started, or its command skills, are not in place, a
// person at the terminal is asked once, with one key, whether to add them, as
// aggy hooks install --vendor <v> and aggy skill link --vendor <v> do, through
// the manifest, so aggy unwire takes them out. A no is kept for this aggy
// version, and the launch then only says, in one line each, what is missing
// and the command that adds it. Nobody to ask (no terminal, a borrow child, a
// session's own shell) gets those lines too.

// fixDeclinedNote is the notes row that keeps a no, holding the aggy version
// it was given for.
const fixDeclinedNote = "launch_fix_declined"

// fixAskEnv, set to 0, turns the question off and leaves the lines (the
// end-to-end tests, as AGGY_TRUST_ASK does for the trust list).
const fixAskEnv = "AGGY_FIX_ASK"

// launchFix is what the launch of v found missing: hooks, the line that says
// so ("" when none), and the hook patches; skills, the command skills'
// layouts not linked for v, and their line.
type launchFix struct {
	hookLine, skillLine string
	hooks               []patch.Patch
	skills              []skillLayout
}

// findLaunchFix reads what is missing for v. quietHooks: the person turned down
// the first-run setup and the hooks are missing altogether (T6966), which is
// then not said or asked on every launch.
func findLaunchFix(ctx context.Context, v adapter.Vendor, quietHooks bool) launchFix {
	var f launchFix
	if w := hookWarning(v); w != "" && !quietHooks {
		f.hookLine = w
		if bin, err := aggyBin(); err == nil {
			if p, err := planHooks(v, bin); err == nil {
				f.hooks = p.patches
			}
		}
	}
	f.skills, f.skillLine = missingSkills(ctx, v)
	return f
}

// missingSkills returns the layouts of aggy's command skills (switch, borrow,
// inbox) that aggy may write and that are not linked for v, and the launch's
// line about them. A folder of the person's own under one of those names is
// left alone, as aggy skill link leaves it, and not asked about.
func missingSkills(ctx context.Context, v adapter.Vendor) ([]skillLayout, string) {
	if !slices.Contains(skillVendors, string(v)) {
		return nil, ""
	}
	ls, err := ownLayouts()
	if err != nil {
		return nil, ""
	}
	ls, _, err = ownWritable(ctx, ls)
	if err != nil {
		return nil, ""
	}
	var miss []skillLayout
	var names []string
	for _, l := range ls {
		name := filepath.Base(l.store)
		if name == skillName {
			continue // the aggy skill costs every turn: only aggy skill link adds it
		}
		if ok, err := l.linked(l.links[string(v)]); err == nil && !ok {
			miss = append(miss, l)
			names = append(names, sessioncmd.Prefix(v)+name)
		}
	}
	if len(miss) == 0 {
		return nil, ""
	}
	return miss, fmt.Sprintf("aggy's commands %s are not in %s's menu: picked there they do not exist, typed in full they still work. aggy skill link adds them", strings.Join(names, ", "), v)
}

// lines are the launch's lines for what is missing.
func (f launchFix) lines() []string {
	var out []string
	for _, l := range []string{f.hookLine, f.skillLine} {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// offerLaunchFix asks, at most once per launch, whether to add what f found
// missing, and returns the lines the launch still says. version is this aggy's;
// a no given for it is kept in the store s, and not asked again until another
// version runs.
func offerLaunchFix(ctx context.Context, s *store.Store, v adapter.Vendor, f launchFix, version string, getenv func(string) string, in io.Reader, out io.Writer) []string {
	canFix := len(f.hooks) > 0 || len(f.skills) > 0
	if !canFix || getenv(fixAskEnv) == "0" {
		return f.lines()
	}
	if no, ok, err := s.Note(ctx, fixDeclinedNote); err == nil && ok && no == version {
		return f.lines()
	}
	var what []string
	if len(f.hooks) > 0 {
		what = append(what, fmt.Sprintf("aggy's hooks for %s (aggy hooks install --vendor %s)", v, v))
	}
	if len(f.skills) > 0 {
		what = append(what, fmt.Sprintf("aggy's command skills for %s (aggy skill link --vendor %s)", v, v))
	}
	yes, asked := askInPlace(getenv, in, out, inPlace{
		change: strings.Join(f.lines(), "\n"),
		undo:   "aggy can add " + strings.Join(what, " and ") + " now; aggy unwire undoes it. A no is kept until aggy is updated.",
		ask:    "Add it?",
	})
	switch {
	case !asked:
		return f.lines()
	case !yes:
		_ = s.SetNote(ctx, fixDeclinedNote, version, time.Now())
		return f.lines()
	}
	var left []string
	if len(f.hooks) > 0 {
		if runID, err := applyPatches(ctx, f.hooks); err != nil {
			fmt.Fprintf(out, "aggy's hooks were not added: %s\n", termsafe.Line(err.Error()))
			left = append(left, f.hookLine)
		} else {
			fmt.Fprintf(out, "Added aggy's hooks for %s (run %s). aggy unwire --run %s undoes it.\n", v, runID, runID)
		}
	}
	if len(f.skills) > 0 {
		if err := linkSkills(ctx, f.skills, map[string]bool{string(v): true}, out); err != nil {
			fmt.Fprintf(out, "aggy's command skills were not linked: %s\n", termsafe.Line(err.Error()))
			left = append(left, f.skillLine)
		}
	}
	return left
}
