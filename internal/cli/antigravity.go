package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/policyhook"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
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
// terms sentence: only that row changes. yes says it is ticked now. On a
// terminal that can take the trust select it is that select with the one row
// (agyTickSelect), so its typing guard holds and no Space or Enter typed for
// the CLI ticks it; otherwise the typed y/N question, default no.
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
	q := inPlace{
		change: fmt.Sprintf("Antigravity (Gemini) is not ticked in aggy's trust list for %s. aggy can tick it there, and only it. %s.", tg.root, antigravity.TermsNote),
		undo:   "aggy unwire undoes it, or aggy trust " + tg.root + " and untick it.",
		ask:    "Tick Antigravity (Gemini) here and start agy?",
	}
	if !trustPerson(getenv) {
		return false, nil
	}
	yes, asked := agyTickSelect(q, in, out)
	if !asked {
		yes, asked = askInPlace(getenv, in, out, q)
	}
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

// agyTickSelect asks q as the trust select with Antigravity's row alone: the
// row carries a warning, so the cursor starts on none and one Space ticks
// nothing; the row ticked and Enter is yes, and any other way out is no and
// records nothing. asked is false when the terminal cannot take the select.
func agyTickSelect(q inPlace, in io.Reader, out io.Writer) (yes, asked bool) {
	i := slices.IndexFunc(trustRowDefs, func(d trustRowDef) bool { return d.ID == trustRowAntigravity })
	if i < 0 || !selectTerminal(in, out) {
		return false, false
	}
	st := trustlist.NewState([]trustlist.Row{trustRowDefs[i].Row}, trustlist.Folder{}, false, nil)
	header := termsafe.Lines(q.change) + "\n" + termsafe.Lines(q.undo) + "\n" +
		termsafe.Line(q.ask) + " Tick its row (Down to it, then Space) and press Enter; Enter alone, n, Esc, or x starts nothing and records nothing.\n\n"
	c, ok := selectTrustList(&trustListAt{st: st}, header, in, out)
	if !ok {
		return false, false
	}
	return c == trustlist.Confirm && slices.Contains(st.Ticked(), trustRowAntigravity), true
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
	// The policy's static rules go in --settings as asks and denies, never an
	// allow. --no-policy leaves the file with no rules and writes no plan, so
	// the hook gives no decisions either (#141 review, L3).
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
		// hooks.json too (#141 review, M3): a command that names the hooks
		// file by a spelling that misses .agents (rm .ag*/hooks.json,
		// find -name hooks.json -delete) asks.
		d.Words = []string{".agents", "hooks.json", ".gemini", ".antigravity"}
		if out.launch.Plan, out.launch.PlanSum, err = writeDecidePlan(in.dir, d); err != nil {
			return out, err
		}
	}
	if written {
		out.warns = append(out.warns, "Antigravity: "+antigravity.TermsNote+".", agyEntryNote)
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
	if err := antigravity.CheckHooksPath(file); err != nil {
		return false, "", agyFail(diag.CodeAgyHooksLink, "%v; aggy writes its hook entry only into this folder's own .agents/hooks.json, so agy was not started: replace the link with a real folder or file", err)
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
		if foreign, _ := antigravity.ForeignEntry(before); foreign {
			return false, "", agyFail(diag.CodeAgyHooksFile, "%s already has an entry named %q that aggy did not write, so aggy left it as it is and did not start agy: rename that entry, then start again", file, antigravity.EntryName)
		}
		if why, others := agyOthersChanged(ctx, file, before); why != "" {
			dlog.Log(ctx, diag.Rec{Event: diag.EvAgyHooksChanged, Code: diag.CodeAgyHooksForeign, Vendor: string(adapter.Antigravity)})
			yes := agyAsk(in, inPlace{
				change: fmt.Sprintf("%s %s: %q. Every entry there runs with agy's tools in this session and can allow or auto-approve them past aggy's hook.", file, why, others),
				undo:   "Nothing is changed by answering; look at the file first.",
				ask:    "Start agy with those entries?",
			})
			if !yes {
				return false, "", agyFail(diag.CodeAgyHooksForeign, "%s %s (%q), so agy was not started: look at them, then start again at a terminal and answer yes, or remove them", file, why, others)
			}
		}
		p.ExpectedHash = patch.Hash(before)
	}
	pl, err := patch.Plan(p)
	if err != nil {
		return false, "", agyFail(diag.CodeAgyHooksFile, "cannot plan aggy's entry in %s: %v", file, err)
	}
	if pl.Action == patch.NoOp {
		return false, "", recordAgyHooks(file)
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
				yes := agyAsk(in, inPlace{
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
	return true, warn, recordAgyHooks(file)
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
		agyHookCheck(stderr)
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
		drain(in) // --no-policy: no decision; one of the two alone asks (agyDecide)
		return 0
	}
	return agyDecideFn(plan, sum, in, stdout, stderr)
}

// agyDecideFn is agyDecide; tests replace it to check the panic fallback.
var agyDecideFn = agyDecide

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
	out = append(out, checkup.Finding{Code: codeAgyEntry, Level: checkup.LevelInfo, Vendor: adapter.Antigravity, Message: agyEntryNote})
	out = append(out, checkup.Finding{Code: codeAgyUpdates, Level: checkup.LevelInfo, Vendor: adapter.Antigravity,
		Message: "agy updates itself in the background, and a new version may change its hooks (1.5.4 changed their payload): " + antigravity.UpdateEnv + "=true in your shell turns that off"})
	return out
}

// The doctor findings' stable codes.
const (
	codeAgyScope       = "antigravity-scope"
	codeAgyUnreachable = "antigravity-unreachable"
	codeAgyUpdates     = "antigravity-updates"
	codeAgyEntry       = "antigravity-entry"
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

// agyEntryNote is the launch line and the doctor line on aggy's entry (#141
// review, M4). It runs aggy by its absolute path, so once aggy is removed or
// moved a plain agy in that folder cannot start the hook, which blocks every
// tool (LIVE). A command that skips a missing aggy would need a shell, and
// whether agy runs a hook command through one is unverified: a word-split
// command with no shell would fail on such a form in every session. So the
// entry stays the plain path, and the person is told.
const agyEntryNote = "aggy's entry in a folder's .agents/hooks.json runs aggy by its absolute path: if you remove or move aggy, plain agy blocks every tool in that folder until you run aggy unwire (or delete the entry named aggy)"

// agyRecord is what aggy keeps of a hooks file right after it last wrote it
// or found its entry in place (#141 review, M3): the file's SHA-256, the
// SHA-256 of the file with aggy's entry taken out (Others: every other entry,
// byte for byte), and the other entries' names, in aggy's own state folder,
// keyed by the file's path. The person's own processes can edit or delete
// it, as they can the hooks file, so it catches mistakes, not a determined
// process (#144 re-review, MEDIUM-B).
type agyRecord struct {
	Hash   string   `json:"hash"`
	Others string   `json:"others"`
	Names  []string `json:"names"`
}

// agyRecordPath is where aggy keeps file's record.
func agyRecordPath(file string) (string, error) {
	h, err := home.Dir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(file)))
	return filepath.Join(h, "antigravity", hex.EncodeToString(sum[:12])+".json"), nil
}

// readAgyRecord reads file's record; ok is false when there is none or it
// cannot be read.
func readAgyRecord(file string) (agyRecord, bool) {
	var r agyRecord
	p, err := agyRecordPath(file)
	if err != nil {
		return r, false
	}
	b, err := safefs.ReadFileLimit(p, 1<<16)
	if err != nil || json.Unmarshal(b, &r) != nil || r.Hash == "" || r.Others == "" {
		return agyRecord{}, false
	}
	return r, true
}

// agyOthers is the hooks file without aggy's entry: its SHA-256 and the
// names of the entries left.
func agyOthers(src []byte) (hash string, names []string, err error) {
	rest, err := antigravity.StripHooks(src)
	if err != nil {
		return "", nil, err
	}
	all, err := antigravity.Names(rest)
	if err != nil {
		return "", nil, err
	}
	return patch.Hash(rest), all, nil
}

// recordAgyHooks records the hooks file as it is now.
func recordAgyHooks(file string) error {
	src, err := safefs.ReadFileLimit(file, agyMaxHooks)
	if err != nil {
		return agyFail(diag.CodeAgyHooksFile, "cannot read %s back: %v", file, err)
	}
	others, names, err := agyOthers(src)
	if err != nil {
		return agyFail(diag.CodeAgyHooksFile, "%s: %v", file, err)
	}
	p, err := agyRecordPath(file)
	if err != nil {
		return err
	}
	if err := ensureDir(filepath.Dir(p)); err != nil {
		return err
	}
	b, _ := json.Marshal(agyRecord{Hash: patch.Hash(src), Others: others, Names: names})
	return safefs.WriteFile(p, b, 0o600)
}

// agyWroteHooks reports whether aggy's manifest holds a change it applied to
// file with aggy's entry as its undo: aggy wrote the file before.
var agyWroteHooks = func(ctx context.Context, file string) bool {
	st, err := store.OpenDefault(ctx)
	if err != nil {
		return true // cannot tell: count it as written, so a missing record asks
	}
	defer func() { _ = st.Close() }()
	cs, err := st.Changes(ctx, "")
	if err != nil {
		return true
	}
	return slices.ContainsFunc(cs, func(c store.Change) bool {
		return c.File == file && c.Reverse == antigravity.Reverse && c.Status == store.ChangeApplied
	})
}

// agyOthersChanged compares the entries other than aggy's with the record
// (#144 re-review, MEDIUM-B). why is "" when they are as aggy last saw them;
// otherwise it says how, with names the entries now there. A missing record
// counts as a change when aggy's manifest says it wrote the file and other
// entries are there; a first write into a file that already holds entries
// says so too.
func agyOthersChanged(ctx context.Context, file string, src []byte) (why string, names []string) {
	others, names, err := agyOthers(src)
	if err != nil {
		return "cannot be read as aggy wrote it", nil
	}
	rec, ok := readAgyRecord(file)
	switch {
	case ok && rec.Others != others:
		return "has entries other than aggy's that changed or appeared since aggy last wrote it", names
	case ok || len(names) == 0:
		return "", names
	case agyWroteHooks(ctx, file):
		return "has entries other than aggy's, and aggy's record of the file is missing", names
	}
	return "already holds entries other than aggy's", names
}

// agyHooksChanged says how the hooks file differs from what aggy last
// recorded: "" when it holds aggy's entry exactly and the other entries are
// as aggy last saw them.
func agyHooksChanged(file string, entry []byte) string {
	src, err := safefs.ReadFileLimit(file, agyMaxHooks)
	if err != nil {
		return "it cannot be read, so aggy's entry may be gone"
	}
	var why []string
	if !antigravity.HasEntry(src, entry) {
		why = append(why, "aggy's entry in it is gone or changed")
	}
	if w, names := agyOthersChanged(context.Background(), file, src); w != "" && !strings.HasPrefix(w, "already") {
		why = append(why, fmt.Sprintf("it %s (%q)", w, names))
	}
	return strings.Join(why, ", and ")
}

// agyHookCheck is the SessionStart and Stop hook's look at its own hooks file
// (#141 review, M3): agy runs hooks in <workspace>/.agents (LIVE). A change is
// logged and said on stderr; the event still goes on to the supervisor.
// Whether agy reads the file again during a session is unverified.
func agyHookCheck(stderr io.Writer) {
	wd, err := os.Getwd()
	if err != nil || filepath.Base(wd) != ".agents" {
		return
	}
	bin, err := aggyBin()
	if err != nil {
		return
	}
	entry, err := antigravity.Entry(bin)
	if err != nil {
		return
	}
	if msg := agyHooksChanged(filepath.Join(wd, "hooks.json"), entry); msg != "" {
		dlog.Log(context.Background(), diag.Rec{Event: diag.EvAgyHooksChanged, Code: diag.CodeAgyHooksChanged, Vendor: string(adapter.Antigravity)})
		fmt.Fprintf(stderr, "aggy: this folder's .agents/hooks.json changed during the session: %s %s\n", msg, dlog.Tag(diag.CodeAgyHooksChanged))
	}
}

// agyExitCheck is the line after agy exits when its hooks file changed during
// the session, or "".
func agyExitCheck(cwd string) string {
	bin, err := aggyBin()
	if err != nil {
		return ""
	}
	entry, err := antigravity.Entry(bin)
	if err != nil {
		return ""
	}
	msg := agyHooksChanged(filepath.Join(filepath.Clean(cwd), antigravity.HooksFile), entry)
	if msg == "" {
		return ""
	}
	return "this folder's .agents/hooks.json changed during the session: " + msg + "; the next aggy antigravity here puts aggy's entry back and asks about new entries"
}

// agyAsk asks in place at the launch's terminal; with nothing to ask at (no
// reader or getenv given), the answer is no.
func agyAsk(in agyInput, q inPlace) bool {
	if in.getenv == nil || in.in == nil {
		return false
	}
	yes, _ := askInPlace(in.getenv, in.in, in.out, q)
	return yes
}
