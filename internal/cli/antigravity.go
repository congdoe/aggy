package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/antigravity"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/policyhook"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/trustlist"
)

// Antigravity on screen (T6957, docs/design.md section 15, "Antigravity").
// aggy runs the real agy in its pseudo-terminal, as it runs claude, on the
// person's own Google sign-in. Its hooks are Claude-like: agy runs them as its
// own children with its environment (live), so aggy hook antigravity finds its
// session from AGGY_* and the socket's lineage. They can only live in a hooks
// file (live: --settings refuses a hooks key), so aggy writes one named entry
// into the launch folder's .agents/hooks.json through the manifest; the
// policy's static rules go in a per-run --settings file in aggy's own folder.

// trustRowAntigravity is the trust list row's id.
const trustRowAntigravity = "antigravity"

// antigravityTrustWarning is the row's warning: the terms sentence, and what
// ticking it writes.
const antigravityTrustWarning = antigravity.TermsNote + "; ticking it lets aggy add its hook entry to this folder's .agents/hooks.json"

func init() {
	registerTrustRow(trustRowDef{
		Row: trustlist.Row{ID: trustRowAntigravity, Label: "Antigravity (Gemini)", Group: trustlist.Harness,
			Warning: antigravityTrustWarning, NotInAll: true},
		bin: antigravity.Bin, order: 40,
		// Its tick is the folder's record itself, which the launch reads
		// (antigravityTicked): unticked, aggy does not start agy here.
		plan: func(trustTarget, bool) (trustRowPlan, error) { return trustRowPlan{}, nil },
	})
}

// antigravityTicked reports whether this folder's trust list ticks
// Antigravity.
func antigravityTicked(dir string) bool {
	tg, err := trustTargetFor(dir)
	if err != nil {
		return false
	}
	path, err := trustFile()
	if err != nil {
		return false
	}
	f, err := readTrustRecord(path)
	if err != nil {
		return false
	}
	return slices.Contains(f.Folders[tg.key].On, trustRowAntigravity)
}

// antigravityTickInPlace offers, at a launch in a folder whose trust list
// does not tick Antigravity, to tick it there and go on (T6979), showing the
// terms sentence: only that row changes. yes says it is ticked now.
func antigravityTickInPlace(ctx context.Context, cwd string, getenv func(string) string, in io.Reader, out io.Writer) (bool, error) {
	if in == nil || getenv == nil {
		return false, nil
	}
	tg, err := trustTargetFor(cwd)
	if err != nil {
		return false, nil
	}
	path, err := trustFile()
	if err != nil {
		return false, err
	}
	f, err := readTrustRecord(path)
	if err != nil {
		return false, nil
	}
	ans := f.Folders[tg.key]
	if !slices.Contains(ans.Shown, trustRowAntigravity) {
		ans.Shown = append(slices.Clone(ans.Shown), trustRowAntigravity)
	}
	ans.On = append(slices.Clone(ans.On), trustRowAntigravity)
	p, changes, err := trustFileChange(path, tg.key, 0, tg.key, &ans)
	if err != nil || !changes {
		return false, err
	}
	yes, asked := askInPlace(getenv, in, out, inPlace{
		change: fmt.Sprintf("Antigravity (Gemini) is not ticked in aggy's trust list for %s. aggy can tick it there, and only it. %s.", tg.root, antigravity.TermsNote),
		undo:   "aggy unwire undoes it, or aggy trust " + tg.root + " and untick it.",
		ask:    "Tick Antigravity (Gemini) here and start agy?",
	})
	if !asked || !yes {
		return false, nil
	}
	runID, err := applyPatches(ctx, []patch.Patch{p})
	if err != nil {
		return false, err
	}
	fmt.Fprintf(out, "Done (run %s). aggy unwire --run %s undoes it.\n", runID, runID)
	return true, nil
}

// agyInput is what a launch hands prepareAntigravity.
type agyInput struct {
	caps     []string
	cwd      string
	dir      string // the launch's private folder, removed when the CLI exits
	noPolicy bool
	plan     parity.PolicyPlan
	rules    permissions.Set
	version  string
	ids      parity.ReadIdentity
	// getenv, in, and out ask in place; a nil in asks nothing.
	getenv func(string) string
	in     io.Reader
	out    io.Writer
}

// agySetup is what preparing an Antigravity launch decided.
type agySetup struct {
	launch adapter.AntigravityLaunch
	warns  []string
}

// agyErr is a refused Antigravity launch, with its code.
type agyErr struct {
	code diag.Code
	err  error
}

func (e *agyErr) Error() string { return e.err.Error() }
func (e *agyErr) Unwrap() error { return e.err }

func agyFail(code diag.Code, format string, a ...any) error {
	return &agyErr{code: code, err: fmt.Errorf(format, a...)}
}

// agyCode is err's reason code, def unless it is an agyErr.
func agyCode(err error, def diag.Code) diag.Code {
	if e := (*agyErr)(nil); errors.As(err, &e) {
		return e.code
	}
	return def
}

// prepareAntigravity readies one Antigravity launch: the trust tick, the
// hooks entry in the launch folder's .agents/hooks.json, the --settings file,
// and the decision plan, all before the launch is recorded. A launch that
// cannot be set up is refused, --no-policy or not: without the hooks entry no
// turn end reaches aggy, and without the settings file agy would start
// without the policy's denies.
func prepareAntigravity(ctx context.Context, in agyInput) (agySetup, error) {
	var out agySetup
	if !antigravityTicked(in.cwd) {
		yes, err := antigravityTickInPlace(ctx, in.cwd, in.getenv, in.in, in.out)
		if err != nil {
			return out, err
		}
		if !yes {
			root, _ := repoRoot(in.cwd)
			return out, agyFail(diag.CodeAgyUntrusted, "Antigravity (Gemini) is not ticked in aggy's trust list for this folder, so aggy does not start agy here: aggy trust %s lists it, with Google's terms note; All does not tick it", root)
		}
	}
	if why := antigravity.Reachable(in.caps); why != "" {
		return out, agyFail(diag.CodeAgyUnreachable, "%s, which aggy's launch needs; update agy (aggy was checked on %s to %s)", why, antigravity.Tested.Min, antigravity.Tested.Max)
	}
	bin, err := aggyBin()
	if err != nil {
		return out, agyFail(diag.CodeAgyHooksFile, "aggy's hooks cannot run: %v; start the aggy you installed (make build), not a temporary one", err)
	}
	written, warn, err := writeAgyHooks(ctx, in, bin)
	if err != nil {
		return out, err
	}
	if warn != "" {
		out.warns = append(out.warns, warn)
	}
	// The policy's static rules, whatever --no-policy says: they only deny or
	// ask, and agy's own layer is the person's. --no-policy turns the hook's
	// decisions off (no plan), as on Claude.
	settings, err := antigravity.Settings(agyRules(in.plan.Decide()))
	if err == nil && in.noPolicy {
		settings = []byte(`{"permissions":{}}`)
	}
	if err != nil {
		return out, err
	}
	if err := ensureDir(in.dir); err != nil {
		return out, err
	}
	out.launch.SettingsFile = filepath.Join(in.dir, "antigravity-settings.json")
	if err := safefs.WriteFile(out.launch.SettingsFile, settings, 0o600); err != nil {
		return out, err
	}
	if !in.noPolicy && in.plan.OnScreen {
		root, _ := repoRoot(in.cwd)
		d := policyhook.NewDecidePlan(string(adapter.Antigravity), in.version, false, in.plan, parity.PreApprovedFrom(in.rules, root, in.ids))
		d.Root = filepath.Clean(in.cwd)
		if root != "" {
			d.Root = root
		}
		d.Protected = append(protectedPaths(), agyProtected(in.cwd, os.Getenv("HOME"))...)
		d.Words = []string{".agents", ".gemini", ".antigravity"}
		if out.launch.Plan, out.launch.PlanSum, err = writeDecidePlan(in.dir, d); err != nil {
			return out, err
		}
	}
	if written {
		out.warns = append(out.warns, "Antigravity: "+antigravity.TermsNote+".")
	}
	out.warns = append(out.warns, agyHookNote)
	if in.noPolicy {
		out.warns = append(out.warns, "aggy's hook gives no decisions this session (--no-policy); it still tells aggy of turn ends")
	}
	if w := adapter.VersionWarning(antigravity.New(), in.version, ""); w != "" {
		out.warns = append(out.warns, w+"; agy's hooks and --settings are checked only on that range, and agy updates itself in the background: "+antigravity.UpdateEnv+"=true turns that off")
	}
	return out, nil
}

// agyHookNote is the launch line on what aggy cannot see: agy's own trust
// dialog (live) gates the workspace hooks file (Google's docs), so until the
// person trusts the folder in agy, aggy's policy hook is not running, and
// aggy cannot tell before a hook arrives.
const agyHookNote = "agy asks once whether you trust this folder; its workspace hooks, aggy's safety policy and turn ends among them, load only once you do, and aggy cannot tell before its first hook arrives: until then aggy's policy is not on, and a switch waits for you to quit agy"

// ensureDir makes dir when it is not there.
func ensureDir(dir string) error {
	if _, err := safefs.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return safefs.Mkdir(dir, 0o700)
	} else if err != nil {
		return err
	}
	return nil
}

// agyProtected are Antigravity's own files whose writes, and whose naming in
// a shell command, the hook asks about: the launch folder's .agents (the
// hooks file aggy's entry is in, and agy's rules), and agy's state folders,
// whose settings could allow what aggy asks about (Google's docs: a persisted
// allow rule goes to settings.json).
func agyProtected(cwd, home string) []string {
	out := []string{filepath.Join(filepath.Clean(cwd), ".agents")}
	if filepath.IsAbs(home) {
		out = append(out, filepath.Join(home, ".gemini"), filepath.Join(home, ".antigravity"))
	}
	return out
}

// agyHooksTracked reports whether git tracks the hooks file at rel in the
// repo at root; an error says it could not tell.
var agyHooksTracked = func(ctx context.Context, root, rel string) (bool, error) {
	out, _, err := defaultBorrowEnv().git(ctx, root, "ls-files", "-z", "--", rel)
	return strings.TrimSpace(out) != "", err
}

// writeAgyHooks puts aggy's entry into the launch folder's .agents/hooks.json
// through the manifest (aggy unwire takes it out, the person's other entries
// staying), and, for a file aggy made in a git repo, the file's line in the
// repo's git exclude file. A file it cannot parse is refused, never
// rewritten. A file git tracks is changed only on a yes at the terminal
// (default no): aggy's entry would show in the person's diff. written says it
// changed the file.
func writeAgyHooks(ctx context.Context, in agyInput, bin string) (written bool, warn string, err error) {
	file := filepath.Join(filepath.Clean(in.cwd), antigravity.HooksFile)
	entry, err := antigravity.Entry(bin)
	if err != nil {
		return false, "", agyFail(diag.CodeAgyHooksFile, "aggy's hook entry cannot be made: %v", err)
	}
	before, err := safefs.ReadFileLimit(file, agyMaxHooks)
	p := patch.Patch{File: file, Mode: 0o644, Reverse: antigravity.Reverse,
		Transform: func(src []byte) ([]byte, error) { return antigravity.SetHooks(src, entry) }}
	exists := true
	switch {
	case errors.Is(err, fs.ErrNotExist):
		p.ExpectedHash, exists = patch.NoFile, false
	case err != nil:
		return false, "", agyFail(diag.CodeAgyHooksFile, "cannot read %s: %v", file, err)
	default:
		if _, err := antigravity.SetHooks(before, entry); err != nil {
			return false, "", agyFail(diag.CodeAgyHooksFile, "%s is left as it is: %v; fix it or move it away, then start again", file, err)
		}
		p.ExpectedHash = patch.Hash(before)
	}
	pl, err := patch.Plan(p)
	if err != nil {
		return false, "", agyFail(diag.CodeAgyHooksFile, "cannot plan aggy's entry in %s: %v", file, err)
	}
	if pl.Action == patch.NoOp {
		return false, "", nil
	}
	ps := []patch.Patch{p}
	if root, ok := repoRoot(in.cwd); ok {
		rel, _ := filepath.Rel(root, file)
		rel = filepath.ToSlash(rel)
		if exists {
			tracked, err := agyHooksTracked(ctx, root, rel)
			if err != nil {
				return false, "", agyFail(diag.CodeAgyHooksFile, "cannot tell whether git tracks %s: %v", file, err)
			}
			if tracked {
				yes, _ := askInPlace(in.getenv, in.in, in.out, inPlace{
					change: fmt.Sprintf("%s is tracked by git. aggy adds its one entry (%q) to it, so the change shows in your diff; your other entries stay as they are.", file, antigravity.EntryName),
					undo:   "aggy unwire takes aggy's entry out.",
					ask:    "Add aggy's entry to the tracked file and start agy?",
				})
				if !yes {
					return false, "", agyFail(diag.CodeAgyHooksFile, "%s is tracked by git, and aggy did not add its entry there, so agy was not started: answer yes at a terminal, or untrack the file", file)
				}
			}
		} else if xp, err := parity.GitExcludeLinePatch(root, rel); err == nil {
			ps = append(ps, xp)
		} else {
			warn = fmt.Sprintf("aggy could not add %s to git's exclude file (%v), so git may list it as a new file", rel, err)
		}
	}
	if _, err := applyPatches(ctx, ps); err != nil {
		return false, "", agyFail(diag.CodeAgyHooksFile, "cannot write aggy's entry into %s: %v; if something changed it while aggy wrote it, start again", file, err)
	}
	return true, warn, nil
}

// agyMaxHooks caps the hooks file aggy reads.
const agyMaxHooks = 1 << 20

// antigravityHookArgs reads aggy hook antigravity's arguments: --event <e>.
func antigravityHookArgs(args []string) (string, bool) {
	if len(args) != 2 || args[0] != "--event" || !slices.Contains(antigravity.Events, args[1]) {
		return "", false
	}
	return args[1], true
}

// antigravityHook is aggy hook antigravity, which agy runs from aggy's entry
// in a folder's .agents/hooks.json, as its own child with its environment.
// Outside an aggy Antigravity session (no AGGY_SOCKET, or AGGY_VENDOR another
// vendor's) it reads the payload, prints nothing, and exits 0 at once: a plain
// agy in the folder runs as without aggy. Inside one, SessionStart and Stop go
// to the supervisor as Claude's do, and PreToolUse is judged by the launch's
// plan (AGGY_DECIDE_PLAN, pinned by AGGY_DECIDE_SHA256). It always exits 0:
// agy blocks the tool on any hook failure (live), so where aggy cannot decide
// it answers force_ask rather than fail, as Claude's wrapper falls back to
// asking. It reads only the tool call and the conversation id; lastUserInput,
// the person's prompt, is never logged or kept.
func antigravityHook(args []string, in io.Reader, getenv func(string) string, stdout, stderr io.Writer) (code int) {
	event, ok := antigravityHookArgs(args)
	if !ok {
		drain(in)
		return cmdUsage(stderr, "hook antigravity", "want --event SessionStart|PreToolUse|Stop", hookUsage)
	}
	if !hook.Forward(adapter.Antigravity, getenv) {
		drain(in)
		return 0
	}
	if event != "PreToolUse" {
		return hookWith([]string{"--vendor", string(adapter.Antigravity), event}, in, getenv, stdout, stderr)
	}
	defer func() {
		if r := recover(); r != nil {
			_, _ = stdout.Write(policyhook.AntigravityFallback())
			code = 0
		}
	}()
	plan, sum := getenv(antigravity.PlanEnv), getenv(antigravity.PlanSumEnv)
	if plan == "" && sum == "" {
		drain(in) // --no-policy: no decision
		return 0
	}
	return agyDecide(plan, sum, in, stdout, stderr)
}

// agyDecide answers one PreToolUse from the plan at path, which must be clean,
// absolute, and hash to sum, recording a start and an end as aggy hook decide
// does. A plan that is missing, unreadable, or changed asks.
func agyDecide(path, sum string, in io.Reader, stdout, stderr io.Writer) int {
	start := time.Now()
	q := dlog.Queue(8)
	defer q.Close(decideLogWait)
	plan, planErr := policyhook.DecidePlan{}, error(policyhook.ErrPlanChanged)
	if filepath.IsAbs(path) && filepath.Clean(path) == path && policyhook.ValidSum(sum) {
		plan, planErr = readDecidePlan(path, sum)
	}
	vv := plan.VendorVersion
	if vv == "" {
		vv = "unknown"
	}
	v := string(adapter.Antigravity)
	startID := q.Log(diag.Rec{Event: diag.EvDecideStarted, Vendor: v, Attrs: map[string]any{"vendor_version": vv}})
	end := func(code diag.Code, decision, group string) {
		q.Log(diag.Rec{Event: diag.EvDecideEnded, Code: code, Vendor: v, Cause: startID, Duration: max(time.Since(start), time.Millisecond),
			Attrs: map[string]any{"vendor_version": vv, "decision": decision, "group": group}})
	}
	raw, err := io.ReadAll(io.LimitReader(in, hook.MaxPayload+1))
	if err != nil || len(raw) > hook.MaxPayload {
		raw = nil // judged as unreadable: asked, never allowed
		drain(in)
	}
	if planErr != nil {
		code := diag.CodeDecidePlan
		if errors.Is(planErr, policyhook.ErrPlanChanged) {
			code = diag.CodeDecidePlanChanged
		}
		d := policyhook.PlanFailed(policyhook.AntigravityJudged(raw), "")
		_, _ = stdout.Write(policyhook.AntigravityOutput(d))
		end(code, string(d.Verdict), "")
		fmt.Fprintf(stderr, "aggy hook antigravity: the launch's decision plan is missing, unreadable, or changed, so this call is asked about %s\n", dlog.Tag(code))
		return 0
	}
	d := policyhook.JudgeAntigravity(plan, raw)
	_, _ = stdout.Write(policyhook.AntigravityOutput(d))
	end(decideCodes[d.Reason], string(d.Verdict), d.Group)
	return 0
}

// antigravityFindings are aggy doctor's Antigravity lines.
func antigravityFindings(vr checkup.Vendor) []checkup.Finding {
	out := []checkup.Finding{{Code: codeAgyScope, Level: checkup.LevelInfo, Vendor: adapter.Antigravity,
		Message: "aggy runs agy on screen only, once Antigravity (Gemini) is ticked in a folder's trust list: " + antigravity.TermsNote}}
	if why := antigravity.Reachable(vr.Capabilities); why != "" {
		out = append(out, checkup.Finding{Code: codeAgyUnreachable, Level: checkup.LevelWarn, Vendor: adapter.Antigravity,
			Message: why + ", which aggy's launch needs"})
	}
	out = append(out, checkup.Finding{Code: codeAgyUpdates, Level: checkup.LevelInfo, Vendor: adapter.Antigravity,
		Message: "agy updates itself in the background, and a new version may change its hooks (1.5.4 changed their payload): " + antigravity.UpdateEnv + "=true in your shell turns that off"})
	return out
}

// The doctor findings' stable codes.
const (
	codeAgyScope       = "antigravity-scope"
	codeAgyUnreachable = "antigravity-unreachable"
	codeAgyUpdates     = "antigravity-updates"
)

// antigravitySetupNote is what setup says about Antigravity.
const antigravitySetupNote = `  aggy runs agy when you name it (aggy antigravity), on screen only, once you
  tick Antigravity (Gemini) in that folder's trust list (aggy trust <dir>); All
  does not tick it. ` + antigravity.TermsNote + `. aggy adds one entry to the
  folder's .agents/hooks.json (aggy unwire takes it out). Sign in with agy itself.
`

// agyExitNote is the line after an agy that exited 1 before any hook reached
// aggy, which is how agy refuses a --settings key it does not know (live), or
// "".
func agyExitNote(code int, hookSeen bool) string {
	if code != 1 || hookSeen {
		return ""
	}
	return "agy exited 1 before any of its hooks reached aggy: if it printed \"--settings: unknown setting\", this agy does not take a setting aggy passes (aggy was checked on " +
		antigravity.Tested.Min + " to " + antigravity.Tested.Max + "); aggy doctor shows its version, and " + antigravity.UpdateEnv + "=true stops its self-update"
}

// agyPrintRefusal is runPrint's line for Antigravity.
func agyPrintRefusal() string {
	return "aggy runs Antigravity on screen only: Google's FAQ calls agy wrapped in third-party applications \"not supported under consumer plans\", under a question about headless mode, so print mode and a stdin that is no terminal are refused; run agy itself for that"
}

// agyRules is the policy's decisions as antigravity.Settings takes them.
func agyRules(ds []parity.PolicyDecision) []antigravity.Rule {
	rs := make([]antigravity.Rule, 0, len(ds))
	for _, d := range ds {
		rs = append(rs, antigravity.Rule{Text: d.Text, Kind: d.Kind})
	}
	return rs
}
