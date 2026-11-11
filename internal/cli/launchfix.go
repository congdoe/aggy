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
// person at the terminal is asked once whether to add them, as aggy hooks
// install --vendor <v> and aggy skill link --vendor <v> do, through the
// manifest, so aggy unwire takes them out. The question names every file a yes
// changes. Both are planned before anything is asked, and written in one run
// or not at all. A no is kept for this aggy version: the launch then says only
// the hooks' line, and nothing about the skills. Nobody to ask (no terminal, a
// borrow child, a session's own shell) gets the lines.

// fixDeclinedNote is the notes row that keeps a no, holding the aggy version
// it was given for. skillsUnlinkedNote keeps the version an aggy skill unlink
// ran under: until another version runs, the launch does not say the command
// skills are missing (#142 review).
const (
	fixDeclinedNote    = "launch_fix_declined"
	skillsUnlinkedNote = "skills_unlinked"
)

// fixAskEnv, set to 0, turns the question off and leaves the lines (the
// end-to-end tests, as AGGY_TRUST_ASK does for the trust list).
const fixAskEnv = "AGGY_FIX_ASK"

// setupAskedNow: this process asked the first-run setup question already, so
// the launch after it asks nothing more (#142 review).
var setupAskedNow bool

// launchFix is what the launch of v found missing: the lines that say so, and
// the patches a yes writes, planned now. planned is false when a part that is
// missing could not be planned: then nothing is asked or written.
type launchFix struct {
	hookLine, skillLine string
	hookNote            string // what else the hooks need (Codex's approval)
	hooks, skills       []patch.Patch
	planned             bool
}

// findLaunchFix reads and plans what is missing for v. It writes nothing.
// quietHooks: the person turned down the first-run setup and the hooks are
// missing altogether (T6966), which is then not said or asked on every launch.
// declined: a no or an aggy skill unlink was given for this aggy version, so
// the skills are not said or asked about.
func findLaunchFix(ctx context.Context, v adapter.Vendor, quietHooks, declined bool) launchFix {
	f := launchFix{planned: true}
	if w := hookWarning(v); w != "" && !quietHooks {
		f.hookLine = w
		bin, err := aggyBin()
		var p hookPlan
		if err == nil {
			p, err = planHooks(v, bin)
		}
		f.hooks, f.hookNote = p.patches, p.note
		f.planned = f.planned && err == nil && len(p.patches) > 0
	}
	if declined {
		return f
	}
	ls, names := missingSkills(ctx, v)
	if len(ls) == 0 {
		return f
	}
	f.skillLine = fmt.Sprintf("aggy's commands %s are not in %s's menu: picked there they do not exist, typed in full they still work. aggy skill link adds them", strings.Join(names, ", "), v)
	ps, err := planSkills(ls, map[string]bool{string(v): true})
	f.skills = ps
	f.planned = f.planned && err == nil && len(ps) > 0
	return f
}

// missingSkills returns the layouts of aggy's command skills (switch, borrow,
// inbox) whose link for v is not there at all, and their names as v's menu
// shows them. A folder of the person's own under one of those names, in aggy's
// store or where v looks, is theirs: aggy leaves it, as aggy skill link does,
// and does not call that skill missing (#142 review).
func missingSkills(ctx context.Context, v adapter.Vendor) ([]skillLayout, []string) {
	if !slices.Contains(skillVendors, string(v)) {
		return nil, nil
	}
	ls, err := ownLayouts()
	if err != nil {
		return nil, nil
	}
	ls, _, err = ownWritable(ctx, ls)
	if err != nil {
		return nil, nil
	}
	var miss []skillLayout
	var names []string
	for _, l := range ls {
		name := filepath.Base(l.store)
		if name == skillName {
			continue // the aggy skill costs every turn: only aggy skill link adds it
		}
		if st, err := entryState(l.links[string(v)]); err != nil || st.Hash != patch.NoFile {
			continue // linked, or something else is there
		}
		miss = append(miss, l)
		names = append(names, sessioncmd.Prefix(v)+name)
	}
	return miss, names
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

// files are the files a yes changes, each once, in the order written.
func (f launchFix) files() []string {
	var out []string
	for _, p := range append(slices.Clone(f.hooks), f.skills...) {
		if !slices.Contains(out, p.File) {
			out = append(out, p.File)
		}
	}
	return out
}

// declinedFor reports whether a no, or an aggy skill unlink, was given for this
// aggy version.
func declinedFor(ctx context.Context, s *store.Store, version string) bool {
	for _, n := range []string{fixDeclinedNote, skillsUnlinkedNote} {
		if v, ok, err := s.Note(ctx, n); err == nil && ok && v == version {
			return true
		}
	}
	return false
}

// askAtLaunch reports whether this launch may ask about what is missing: a new
// on-screen start, not a switch, a resume of the same child (a tool change's,
// while the supervisor's reader holds the terminal), --direct, or a borrow's
// child.
func askAtLaunch(t target, childKind string) bool {
	return !t.Direct && t.SwitchFrom == "" && !t.SameChild && childKind == ""
}

// offerLaunchFix asks, at most once per launch, whether to add what f found
// missing, and returns the lines the launch still says. version is this aggy's;
// a no given for it is kept in the store s. A part that cannot be planned asks
// nothing, writes nothing, and is not asked about again for this version.
func offerLaunchFix(ctx context.Context, s *store.Store, v adapter.Vendor, f launchFix, version string, getenv func(string) string, in io.Reader, out io.Writer) []string {
	if f.hookLine == "" && f.skillLine == "" {
		return nil
	}
	if getenv(fixAskEnv) == "0" || setupAskedNow || declinedFor(ctx, s, version) {
		return f.lines()
	}
	if !f.planned {
		_ = s.SetNote(ctx, fixDeclinedNote, version, time.Now())
		return f.lines()
	}
	files := f.files()
	var change []string
	change = append(change, f.lines()...)
	change = append(change, "A yes changes these files, through aggy's manifest:")
	for _, p := range files {
		change = append(change, "  "+termsafe.Field(p))
	}
	if len(f.hooks) > 0 {
		change = append(change, fmt.Sprintf("The hooks run in every %s session, also outside aggy, where they do nothing.", v))
	}
	if f.hookNote != "" {
		change = append(change, f.hookNote)
	}
	yes, asked := askInPlace(getenv, in, out, inPlace{
		change: strings.Join(change, "\n"),
		undo:   "aggy unwire undoes it. A no is kept until aggy is updated.",
		ask:    "Add them?",
	})
	switch {
	case !asked:
		return f.lines()
	case !yes:
		_ = s.SetNote(ctx, fixDeclinedNote, version, time.Now())
		return f.lines()
	}
	runID, err := applyPatchesRun(ctx, append(slices.Clone(f.hooks), f.skills...))
	if err != nil {
		fmt.Fprintf(out, "aggy did not add them: %s\n", termsafe.Line(err.Error()))
		if runID != "" {
			fmt.Fprintf(out, "Part was written: aggy unwire --run %s undoes it.\n", runID)
		}
		return f.lines()
	}
	for _, p := range files {
		fmt.Fprintf(out, "changed %s\n", termsafe.Field(p))
	}
	fmt.Fprintf(out, "Done (run %s). aggy unwire --run %s undoes it.\n", runID, runID)
	return nil
}
