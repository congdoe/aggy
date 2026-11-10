package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/adapter/opencode"
	"github.com/congdoe/aggy/internal/brief"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/limits"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
)

// runRun is `aggy run [--direct] [--no-policy] [--tool NAME]... <vendor>`:
// a new parent in the current directory with an interactive session of
// that vendor, under the supervisor, or with --direct on aggy's own
// terminal (runDirect). --no-policy starts it without the safety policy
// (launch). --tool turns a tool on for this session only (target.Tools).
func runRun(args []string, stdout, stderr io.Writer) int {
	usage := runHelpText
	var direct, noPolicy bool
	var tools []string
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		name, val, hasVal := strings.Cut(args[0], "=")
		switch {
		case args[0] == "--direct":
			direct = true
		case args[0] == "--no-policy":
			noPolicy = true
		case name == "--tool":
			if !hasVal {
				if len(args) < 2 {
					return cmdUsage(stderr, "run", "--tool needs a name", usage)
				}
				args, val = args[1:], args[1]
			}
			if !tooldef.ValidName(val) {
				return cmdUsage(stderr, "run", fmt.Sprintf("%q is not a tool name", val), "")
			}
			tools = append(tools, val)
		default:
			return cmdUsage(stderr, "run", fmt.Sprintf("unknown flag %q; aggy claude, codex, grok, or opencode take a CLI's flags", args[0]), usage)
		}
		args = args[1:]
	}
	if len(args) != 1 {
		return cmdUsage(stderr, "run", "name one vendor", usage)
	}
	v, err := adapter.ParseVendor(args[0])
	if err != nil {
		return cmdUsage(stderr, "run", err.Error(), "")
	}
	return launch("aggy run", target{New: true, Vendor: v, Direct: direct, NoPolicy: noPolicy, Tools: tools}, stderr)
}

// launch opens the parent t names and puts its child on screen under the
// supervisor: the child last on screen resumed with its CLI's resume
// flag and no brief, or a new child. name prefixes errors. It gives the
// child the shared memory of the child's own repo and the safety
// policy, and refuses to start a CLI the policy cannot reach unless
// t.NoPolicy is set or the user's config sets [policy] required = false.
//
// When the CLI exits for a switch (T6664), the next vendor's child of the
// same parent goes on screen the same way, through launchOnce, so it gets
// its own lease, the repo's allowed_vendors, the policy, and memory. Like
// /vendor it gets a brief, a new child, a chosen child, or a one-launch
// model only when the switch asked for one (T6665). --no-policy is not
// carried over (S-6):
// the next child gets the policy unless [policy] required = false, and a
// switch to a CLI the policy cannot reach is refused before the exit
// command is typed (CanSwitch).
//
// The launches share one diagnostic trace, this process's: each CLI gets
// it in AGGY_TRACE, so its hooks and borrows join it. The supervisor's
// records go through a queue (diagQueue), so the terminal never waits
// on the disk.
func launch(name string, t target, stderr io.Writer) int {
	q := dlog.Queue(diagQueue)
	defer q.Close(diagFlush)
	// before is the launch the CLI on screen was switched from, to go back to
	// when that CLI never starts (T6899).
	var before *reopen
	for {
		var next switchTo
		code := launchOnce(name, &t, stderr, &next, q)
		if next.vendor == "" {
			if next.unstarted == nil || before == nil {
				return code
			}
			u := next.unstarted
			q.Log(diag.Rec{Event: diag.EvSwitchReturned, Code: diag.CodeNeverStarted, Parent: before.next.parentID, Vendor: string(u.vendor),
				Attrs: map[string]any{"from": string(u.vendor), "to": string(before.next.vendor), "exit": code}})
			fmt.Fprintf(stderr, "%s: %s %s\n", name, goBackLine(u.vendor, before.next), dlog.Tag(diag.CodeNeverStarted))
			t = sameChildAgain(before.t, before.next)
			t.SwitchFrom, t.SwitchReason = u.vendor, "back: it never started"
			before = nil
			continue
		}
		c := next.choice
		if c.Resume {
			t = sameChildAgain(t, next)
			before = nil
			continue
		}
		// next.from is the same session, for going back to it.
		before = &reopen{t: t, next: switchTo{vendor: next.from, parentID: next.parentID, child: next.child, fromID: next.fromID}}
		t = target{ParentID: next.parentID, Vendor: next.vendor, Child: c.Child, NewChild: c.New, Model: c.Model, Brief: c.Brief, Tools: c.Tools,
			SwitchFrom: next.from, SwitchReason: next.origin}
	}
}

// screenEvent is the events row for a launch of child under t: a switch
// row (from, to, model, reason) for a switch's launch, else a screen row
// (vendor, model). A restart of the same child for a tool change (T6729)
// is neither (ok false): the same CLI is back on screen.
func screenEvent(t target, parentID string, child store.Child, at time.Time) (e store.Event, ok bool) {
	e = store.Event{At: at, ParentID: parentID, ChildID: child.ID}
	switch {
	case t.SwitchFrom != "":
		e.Type = store.EventSwitch
		e.Detail = map[string]any{"from": string(t.SwitchFrom), "to": child.Vendor, "model": child.Model, "reason": t.SwitchReason}
	case t.SameChild:
		return store.Event{}, false
	default:
		e.Type = store.EventScreen
		e.Detail = map[string]any{"vendor": child.Vendor, "model": child.Model}
	}
	return e, true
}

// goBackLine says what follows a CLI of vendor from that never started. A child
// with no session id cannot be reopened, so plan makes a new one there, and the
// line must not call that going back.
func goBackLine(from adapter.Vendor, to switchTo) string {
	const closed = "%s closed before it started (a dialog declined, or quit before your first message), "
	if to.fromID == "" {
		return fmt.Sprintf(closed+"so aggy opens a new %s session: it never learned the id of the one before. Quit %s to leave aggy.", from, to.vendor, to.vendor)
	}
	return fmt.Sprintf(closed+"so aggy goes back to %s. Quit %s to leave aggy.", from, to.vendor, to.vendor)
}

// sameChildAgain is the target of T6729's resume: the same child again, so it
// is wired with the tools as now set. It is the same session, so its --tool
// names, one-launch model, and CLI flags carry over, unlike a switch;
// --no-policy does not (S-6), so neither do the flags only it let through, and
// it gets no brief.
func sameChildAgain(t target, next switchTo) target {
	return target{ParentID: next.parentID, Vendor: next.vendor, Child: next.child, SameChild: true,
		Model: t.Model, Tools: t.Tools, Extra: t.SafeExtra, SafeExtra: t.SafeExtra, Dirs: t.Dirs}
}

// switchTo is the vendor a CLI exited for, its parent, and the switch's
// options. unstarted, with no vendor, says the CLI never started (T6899).
type switchTo struct {
	vendor    adapter.Vendor
	parentID  string
	choice    supervisor.Choice
	child     string         // the child that was on screen, for a resume (T6729)
	from      adapter.Vendor // the vendor that was on screen
	fromID    string         // its session id, "" if aggy never learned it
	origin    string         // who asked for the switch (supervisor.RunFacts.SwitchOrigin)
	unstarted *unstarted
}

// reopen is a launch to go back to: its target, and that child as a switchTo.
type reopen struct {
	t    target
	next switchTo
}

// unstarted is a CLI that closed before it started.
type unstarted struct{ vendor adapter.Vendor }

// startEvidence is what the CLI just run left behind about getting past its
// launch dialogs.
type startEvidence int

const (
	startUnknown startEvidence = iota // aggy cannot tell: keep the row, go back nowhere
	startYes
	startNever
)

// childStarted looks for that evidence: a counted turn end, a session file (for
// a new child, any: its id is new, so a file behind aggy's clock is still its
// own; for a resumed one, changed since the launch), or, for a new Codex child
// with no id yet, a rollout new since then (another Codex's counts too: the safe
// side, which just ends aggy as before). A SessionStart hook is no evidence:
// whether a CLI sends it before its trust dialog is unverified. Quitting before
// the first message looks the same as a declined dialog, and is treated the same.
//
// startNever takes a folder aggy can read, of a recorded layout, that lacks the
// file. One that is not there at all (an unmounted disk, a vendor whose layout
// aggy cannot read) says nothing, and going back from it could delete a row with
// a session behind it. So does a miss in Claude's project folders, whose layout
// is unverified (errNotInScan): a file somewhere aggy does not look is possible.
// TODO(T6895): a live check of that layout can move Claude to startNever.
func childStarted(facts supervisor.RunFacts, child store.Child, id string, newChild bool, began time.Time, sawRollout func() (seen, known bool), files *sessionFiles) startEvidence {
	if facts.TurnRan {
		return startYes
	}
	v, err := adapter.ParseVendor(child.Vendor)
	if err != nil {
		return startUnknown
	}
	if id == "" {
		if sawRollout == nil {
			return startUnknown
		}
		switch seen, known := sawRollout(); {
		case seen:
			return startYes
		case known:
			return startNever
		}
		return startUnknown
	}
	t, err := files.LastChanged(v, id, child.VendorHome, child.Cwd, child.CreatedAt)
	switch {
	case errors.Is(err, errNoSessionFile):
		return startNever
	case err != nil:
		return startUnknown
	case newChild || !t.Before(began.Truncate(time.Second)): // file times can be whole seconds
		return startYes
	}
	return startNever
}

// diagQueue is how many records the supervisor's diagnostic queue holds;
// diagFlush bounds how long aggy waits for it to be written at the end.
const (
	diagQueue = 256
	diagFlush = 2 * time.Second
)

// launchOnce is one launch. It returns the CLI's exit code, and sets next
// when the CLI exited for a switch. q takes the supervisor's records. It
// changes *tp only through gateDirFlags, so a later launch of the session does
// not carry a flag only --no-policy let through.
func launchOnce(name string, tp *target, stderr io.Writer, next *switchTo, q *diag.Queue) int {
	t := *tp
	// ids are the entity ids known so far, for the records.
	var ids diag.Rec
	// The record's message is the code's registry text: the error can
	// quote a path, PATH, AGGY_HOME, or a CLI's own text (G67-1).
	fail := func(code diag.Code, err error) int {
		r := ids
		info, _ := diag.LookupCode(code)
		r.Event, r.Code, r.Msg = diag.EvLaunchRefused, code, info.Meaning
		dlog.Log(context.Background(), r)
		fmt.Fprintf(stderr, "%s: %s %s\n", name, termsafe.Line(err.Error()), dlog.Tag(code))
		return 1
	}
	// Refused before anything is written to the index.
	if !isTerminal(os.Stdin) {
		return fail(diag.CodeNoTerminal, errors.New("needs a terminal on stdin"))
	}
	cfg, err := config.LoadDefault()
	if err != nil {
		return fail(diag.CodeConfig, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return fail(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	pl, err := plan(ctx, s, cfg, cwd, t, time.Now(), stderr)
	if err != nil {
		return fail(launchCode(err, diag.CodeLaunchFailed), err)
	}
	v := adapter.Vendor(pl.child.Vendor)
	ids = diag.Rec{Parent: pl.parent.ID, Child: pl.child.ID, Vendor: string(v)}
	if err := gateDirFlags(tp, pl.child.Cwd); err != nil {
		return fail(diag.CodeFlagLoosens, err)
	}
	t = *tp
	if pl.remakeCwd {
		remove, err := remakeBorrowDir(pl.child.Cwd)
		if err != nil {
			return fail(diag.CodeLaunchFailed, err)
		}
		defer remove()
	}
	// The child's repo decides which vendors may see it, the one on
	// screen and the ones the switch picker offers.
	eff, err := allowedIn(cfg, pl.child.Cwd, v)
	if err != nil {
		return fail(launchCode(err, diag.CodeConfig), err)
	}
	path, err := probe.LookPath(vendorBin(v))
	if err != nil {
		return fail(diag.CodeCLIMissing, err)
	}
	// The probe finds the flags whose presence is only known from help:
	// Codex --no-alt-screen and -c, Claude --settings, Grok --rules and
	// --deny.
	caps, version, err := probeCaps(ctx, v, path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: probing %s: %s; starting it without the flags help must list\n", name, v, termsafe.Line(err.Error()))
	}
	var a adapter.Adapter
	switch v {
	case adapter.Claude:
		a = claude.Adapter{Caps: caps}
	case adapter.Codex:
		a = codex.Adapter{Caps: caps}
	case adapter.Grok:
		a = grok.New()
	case adapter.OpenCode:
		a = opencode.New()
	}
	key, err := config.ParseKey(cfg.Switch.Key)
	if err != nil {
		return fail(diag.CodeConfig, err)
	}
	stateDir, err := home.Dir()
	if err != nil {
		return fail(diag.CodeStore, err)
	}
	parentID := pl.parent.ID
	runDir := filepath.Join(stateDir, "run")
	if err := supervisor.PrivateDir(runDir); err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	sweepMemoryDirs(runDir, time.Now())
	req := pl.interactiveRequest()
	req.Extra = t.Extra
	// The policy is checked before the launch is recorded (commit), so a
	// refused launch adds no parent, child, or lease. plan's refresh may
	// already have raised last-active times from session files (D19-9).
	var warns []string
	if pl.child.Kind != "" {
		warns = append(warns, borrowNote(pl))
	}
	if w := hookWarning(v); w != "" {
		warns = append(warns, w)
	}
	if len(t.Loosens) > 0 {
		warns = append(warns, "your flags "+strings.Join(t.Loosens, ", ")+" loosen approvals, the sandbox, or hook trust for this launch; --no-policy lets them through")
	}
	warns = append(warns, t.FlagNotes...)
	if t.NoPolicy {
		warns = append(warns, "starting without the safety policy (--no-policy)")
	} else {
		pw, err := addPolicy(v, caps, cfg.Policy.Required, &req)
		if err != nil {
			return fail(diag.CodePolicy, err)
		}
		warns = append(warns, pw...)
	}
	// The person's shared rules, read now, so an approval given in another
	// CLI since the last launch or switch holds here. A borrow's child, even
	// reopened on screen, gets none.
	var rules permissions.Set
	if pl.child.Kind == "" {
		rules = loadPermissions(ctx, pl.child.Cwd, cfg)
		if rules.FileErr != nil {
			return fail(diag.CodePermBadFile, rules.FileErr)
		}
		warns = append(warns, addPermissions(v, caps, rules, &req)...)
	}
	// Memory comes from the child's own directory, never the current one:
	// aggy resume <id> works from anywhere.
	// A crashed launch's folder is left to sweepMemoryDirs above.
	memDir, err := memoryDir(runDir, parentID)
	if err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	// An edit borrow's child could write its worktree's memory file, and no
	// one has reviewed that worktree yet, so it is not taken as instructions.
	cleanup, mw := func() {}, []string(nil)
	if adapter.Kind(pl.child.Kind) != adapter.Edit {
		cleanup, mw = addMemory(v, caps, pl.child.Cwd, memDir, &req)
	}
	defer cleanup()
	// The tools that are on, for this child's own vendor and repo: a
	// switch or resume is wired again for the CLI it starts. Their files
	// share the launch's private folder, removed when the CLI exits.
	var tw []string
	if v == adapter.OpenCode && len(t.Tools) > 0 {
		tw = []string{"managed tools are not wired to OpenCode yet, so --tool is ignored here"}
	} else if v != adapter.OpenCode {
		tw = addTools(ctx, v, caps, pl.child.Cwd, memDir, t.Tools, &req)
	}
	defer func() { _ = safefs.RemoveAll(memDir) }()
	for _, w := range append(append(warns, mw...), tw...) {
		fmt.Fprintf(stderr, "%s: %s\n", name, termsafe.Line(w))
	}
	// The brief is built now and written only once the lease is held, so
	// a refused launch leaves no file and never rewrites the brief of a
	// child another aggy has on screen (G34-5). It is a file the child is
	// told to read, never an argument (ARG_MAX).
	var briefText string
	if t.Brief {
		if briefText, err = childBrief(ctx, s, defaultBorrowEnv(), pl); err != nil {
			return fail(diag.CodeLaunchFailed, err)
		}
		if req.BriefFile, err = brief.Path(pl.child.Cwd, pl.child.ID); err != nil {
			return fail(diag.CodeLaunchFailed, err)
		}
	}
	if v == adapter.OpenCode {
		oc, err := prepareOpenCode(openCodeInput{cfg: eff, caps: caps, cwd: pl.child.Cwd, dir: memDir,
			model: pl.child.Model, memory: req.Memory, brief: briefText, noPolicy: t.NoPolicy, rules: rules})
		if err != nil {
			return fail(diag.CodePolicy, err)
		}
		req.OpenCode = &oc.launch
		for _, w := range oc.warns {
			fmt.Fprintf(stderr, "%s: %s\n", name, termsafe.Line(w))
		}
		if version != "" {
			if w := openCodeNewVersionNote(ctx, s, version); w != "" {
				fmt.Fprintf(stderr, "%s: %s\n", name, termsafe.Line(w))
			}
		}
		if oc.launch.Mini {
			fmt.Fprintf(stderr, "%s: %s\n", name, termsafe.Line(openCodeUntrustedNote(oc.root, rules.HeldLocal)))
		}
	}
	cmd, err := a.Interactive(req)
	if err != nil {
		return fail(diag.CodeUnsupported, err)
	}
	cmd.Path = path
	lease, err := pl.commit(ctx, s, time.Now())
	if err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	// onScreen records, once the CLI's process has started, which CLI came on
	// screen or the switch that put it there (T6925). Not at the commit: a
	// brief that cannot be written, an interrupted tool proxy, or a CLI that
	// cannot be started ends the launch after it, and the log must not show a
	// CLI that never ran. The log is a record, so a failed write never stops
	// the launch.
	// TODO(T6955): the write waits up to the store's busy timeout on a locked
	// index, as the commit above can.
	onScreen := func() {
		if e, ok := screenEvent(t, pl.parent.ID, pl.child, time.Now()); ok {
			_ = s.AppendEvent(ctx, e)
		}
	}
	if pl.child.Kind == "" {
		stopScan, note := suggestAtLaunch(ctx, s, pl.child.Cwd)
		defer stopScan()
		if note != "" {
			fmt.Fprintf(stderr, "%s: %s\n", name, termsafe.Line(note))
		}
	}
	defer func() {
		if lease != nil {
			_ = lease.Release(ctx)
		}
		// Record the session file's time now the CLI has exited.
		_, _ = refreshChildren(ctx, s, pl.parent.ID, &sessionFiles{})
	}()
	if t.Brief {
		// G34-7: a failure here leaves the committed child row with no
		// session behind it. That is benign: the next plan marks it gone.
		_, warn, err := brief.WriteFile(pl.child.Cwd, pl.child.ID, briefText)
		if err != nil {
			return fail(diag.CodeLaunchFailed, err)
		}
		if warn != "" {
			fmt.Fprintf(stderr, "%s: %s\n", name, termsafe.Line(warn))
		}
	}
	// T6780: a tool's proxy, once the launch is recorded, so a refused
	// launch never starts one. It is stopped when this CLI exits, before
	// a switch or resume launches the next. Any failure is a warning and
	// the CLI starts without it.
	px := startToolProxy(ctx, v, pl.child.Cwd, memDir, proxyTools(v, t.Tools), stderr, name)
	defer px.stop()
	if px.interrupted {
		fmt.Fprintf(stderr, "%s: interrupted; nothing started %s\n", name, dlog.Tag(diag.CodeProxyFailed))
		return 130
	}
	if px.env != "" {
		cmd.Env = append(cmd.Env, px.env)
	}
	// HIGH-2: a proxy that dies mid-session is recorded at once (its
	// address is already held, execrun.Proxy.Died), the supervisor reads
	// the session's tools again and offers a resume, and aggy says so
	// once the terminal is its own again. When aggy could not hold the
	// address, execrun has already killed the CLI (px.guard), so the
	// session ends and the warning shows at once.
	proxyNudge := make(chan struct{}, 1)
	watch, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	if px.died != nil {
		go func() {
			select {
			case <-px.died:
				px.onDied()
				select {
				case proxyNudge <- struct{}{}:
				default:
				}
			case <-watch.Done():
			}
		}()
	}
	defer func() {
		if n := px.diedNote(name); n != "" {
			fmt.Fprintln(stderr, n)
		}
	}()
	// The child's id when known at launch: Claude's and Grok's
	// pre-assigned or resumed one (the only id the index records for
	// them), or a resumed Codex thread. A new Codex child's id comes from
	// the session index, which ids hands to the child row.
	assigned := pl.child.VendorSessionID
	cids := &childIDs{s: s, child: pl.child}
	defer cids.release(ctx)
	// The launch, and the CLI's exit (with the vendor launched next), as
	// records; the vendor session id only where the index has it already.
	launched := ids
	launched.Event, launched.VendorSession = diag.EvLaunched, assigned
	launched.Attrs = map[string]any{"new_child": pl.newChild, "new_parent": pl.newParent, "resumed": pl.resume != "",
		"brief": t.Brief, "model_set": t.Model != "", "direct": t.Direct, "cli_flags": len(t.Extra)}
	if len(t.Loosens) > 0 {
		launched.Attrs["loosening_flags"] = strings.Join(t.Loosens, ",")
	}
	// Queued, like the rest of the session's records, so the launch
	// never waits on the disk (G67-6).
	cause := q.Log(launched)
	// The CLI owns the terminal from here: a warning about the log waits
	// until it is back (G67-5).
	dlog.Hold()
	defer dlog.Release()
	began := time.Now()
	exited := func(code int, to adapter.Vendor) {
		r := ids
		r.Event, r.Cause, r.Exit, r.Duration = diag.EvSessionExited, cause, diag.Int(code), time.Since(began)
		if to != "" {
			r.Attrs = map[string]any{"switch_to": string(to)}
		}
		q.Log(r)
	}
	if t.Direct {
		// --direct's CLI shares aggy's process group: only it is killed.
		code, err := runDirect(cmd, runDir, parentID, v, assigned, cids.set, func(r diag.Rec) {
			r.Parent, r.Child, r.Vendor = parentID, pl.child.ID, string(v)
			q.LogWait(r, 100*time.Millisecond) // the socket's goroutines
		}, func(pid int) {
			px.guard(pid, false)
			onScreen()
		})
		px.guard(0, false)
		dlog.Release()
		if err != nil {
			r := ids
			r.Event, r.Code, r.Cause, r.Msg = diag.EvLaunchRefused, diag.CodeLaunchFailed, cause, "the supervisor failed"
			q.Log(r)
			fmt.Fprintf(stderr, "%s: %s %s\n", name, termsafe.Line(err.Error()), dlog.Tag(diag.CodeLaunchFailed))
			if code == 0 {
				code = 1
			}
		} else {
			exited(code, "")
		}
		return code
	}
	onHook, stopIndex, index := startIndex(parentID, v, assigned, cids.set)
	defer stopIndex()
	// A limit reaches the picker (T6667) without ever blocking the watch.
	limitHit := make(chan supervisor.LimitHit, 1)
	onHook, stopLimits := startLimits(parentID, v, onHook, func(lv supervisor.LimitHit) {
		select {
		case limitHit <- lv:
		default:
		}
	})
	defer stopLimits()
	// A switch is checked when picked or asked for, and again before the
	// CLI is told to exit; launch checks everything once more.
	// The probes switchCheck starts end with this launch.
	pctx, cancelProbes := context.WithCancel(ctx)
	defer cancelProbes()
	canSwitch := switchCheck(pctx, cfg, pl.child.Cwd)
	vendors := pickerVendors(eff.Switch.Order, v)
	// The policy probes start now, so a switch later is not kept waiting
	// on them.
	go func() {
		for _, o := range vendors {
			_ = canSwitch(o)
		}
	}()
	// The picker's index reads run in the background too (O56-6), so the
	// supervisor's loop never waits on SQLite, all on one read-only handle
	// closed when the CLI exits (T6770).
	parent := pl.parent.ID
	ro := &roIndex{}
	defer ro.close()
	children := refreshed(pctx, vendors, func(o adapter.Vendor) []supervisor.ChildChoice { return childChoices(ro.get(), parent, o, time.Now()) }, nil)
	quota := refreshed(pctx, vendors, func(o adapter.Vendor) bool { return hasQuota(ro.get(), o) }, true)
	hasChild := func(o adapter.Vendor, id string) bool { return childRecorded(ro.get(), parent, o, id) }
	// The near-limit share is read only at a turn end, and only when
	// warn_at is on (L60-2); the supervisor calls it off its loop.
	var used func(adapter.Vendor) (supervisor.Share, bool)
	if eff.Switch.WarnAt > 0 {
		var allowance limits.AllowanceCache
		used = func(o adapter.Vendor) (supervisor.Share, bool) { return usedShare(o, &allowance, cids.current()) }
	}
	// The tools this launch got, from the settings, so a tool switched on
	// or off while it runs is offered as a resume (T6729). Without them
	// (a repo config or index aggy cannot read) none is offered.
	var tools func() (supervisor.ToolSet, error)
	toolBase, err := sessionToolSet(ctx, v, caps, pl.child.Cwd, t.Tools)
	if err == nil && v != adapter.OpenCode {
		tools = func() (supervisor.ToolSet, error) {
			set, err := sessionToolSet(pctx, v, caps, pl.child.Cwd, t.Tools)
			px.withoutProxy(set)
			return set, err
		}
	}
	var facts supervisor.RunFacts
	ex, err := supervisor.Run(supervisor.Options{
		Facts:         &facts,
		Tools:         tools,
		ToolsAtLaunch: toolBase,
		ToolsNudge:    proxyNudge,
		In:            os.Stdin, Out: os.Stdout, Command: cmd,
		// The CLI leads its own session in the pseudo-terminal.
		Start: func(c adapter.Command, rows, cols int) (supervisor.Child, error) {
			ch, err := startPTY(c, rows, cols)
			if err == nil {
				px.guard(ch.Pid(), true)
				onScreen()
			}
			return ch, err
		},
		SwitchKey: key,
		Vendors:   vendors,
		RunDir:    runDir,
		ParentID:  parentID,
		OnHook:    onHook,
		Vendor:    v,
		CanSwitch: canSwitch,
		// OpenCode takes a brief in its instructions, so none starts a turn.
		StartsTurn: t.Brief && v != adapter.OpenCode,
		SessionID:  cids.current,
		OnFollow:   cids.follow,
		// T6848: only when aggy can say so for this binary; one that cannot
		// be checked refuses nothing.
		HooksMissing: hooksMissing(v),
		Children:     children,
		HasChild:     hasChild,
		SwitchBlocked: func(o adapter.Vendor, id string) string {
			return switchBlocked(childCwd(ro.get(), parent, o, id, pl.child.Cwd), o)
		},
		Limits:   limitHit,
		HasQuota: quota,
		Used:     used,
		WarnAt:   eff.Switch.WarnAt,
		Diag:     q,
		ChildID:  pl.child.ID,
	})
	px.guard(0, false) // the CLI was reaped: never signal its pid again
	dlog.Release()
	// Before the next launch, whose brief reads this child's transcript.
	cids.settle(ctx, appendEvent, &sessionFiles{})
	if err != nil {
		r := ids
		r.Event, r.Code, r.Cause, r.Msg = diag.EvLaunchRefused, diag.CodeLaunchFailed, cause, "the supervisor failed"
		q.Log(r)
		fmt.Fprintf(stderr, "%s: %s %s\n", name, termsafe.Line(err.Error()), dlog.Tag(diag.CodeLaunchFailed))
		if ex.Code == 0 {
			ex.Code = 1
		}
		return ex.Code
	}
	exited(ex.Code, ex.SwitchTo)
	if ex.SwitchTo != "" {
		*next = switchTo{vendor: ex.SwitchTo, parentID: parentID, choice: ex.Next, child: pl.child.ID, from: v, fromID: cids.current(), origin: facts.SwitchOrigin}
		return ex.Code
	}
	// A signal to aggy or a signal that killed the CLI never leads to another
	// launch; only launch knows whether there is a CLI to go back to.
	// TODO(T6895): a CLI's own exit code 130 after Ctrl+C reads as a signal here,
	// so it ends aggy: the safe side.
	if facts.Signalled || ex.Code >= 128 || !t.fromSwitch() || cids.didMove() ||
		childStarted(facts, pl.child, cids.current(), pl.newChild, began, index.sawRollout, &sessionFiles{}) != startNever {
		return ex.Code
	}
	next.unstarted = &unstarted{vendor: v}
	if pl.newChild {
		// It has nothing behind it, and it must not stay the parent's latest.
		cids.release(ctx)
		if lease != nil {
			_ = lease.Release(ctx)
			lease = nil
		}
		_, _ = s.DeleteUnstartedChild(ctx, pl.child.ID)
	}
	return ex.Code
}

// hooksMissing reports whether aggy's turn-end hook for v is not installed, for
// any aggy binary: then aggy sees no turn end. Only that makes an agent's switch
// wait forever. It is only a hint: the supervisor lets an arriving hook overrule
// it. Codex's also need the person's approval, which aggy cannot check; that
// stays the list's words.
func hooksMissing(v adapter.Vendor) bool {
	pr, err := hookPresence(v)
	return err == nil && slices.Contains(pr.Absent, "Stop")
}

// hookWarning is the launch's line about v's hooks, "" when they are in place.
func hookWarning(v adapter.Vendor) string {
	if hooksMissing(v) {
		return fmt.Sprintf("aggy's hooks for %s are not installed, so aggy sees no turn end and no limit in it: a switch means quitting %s yourself. aggy hooks install sets them up", v, v)
	}
	pr, err := hookPresence(v)
	switch {
	case err != nil:
		return ""
	case len(pr.Absent) > 0:
		return fmt.Sprintf("some of aggy's hooks for %s are not installed (%s), so aggy may miss what they report. aggy hooks install adds them", v, strings.Join(pr.Absent, ", "))
	case len(pr.Stale) > 0:
		return fmt.Sprintf("aggy's hooks for %s call another aggy binary; they report while it is there. aggy hooks install points them at this one", v)
	}
	return ""
}

// refreshEvery is how often refreshed reads again.
var refreshEvery = 2 * time.Second

// refreshed returns fn's latest answer for each of vendors, read again
// in the background every refreshEvery until ctx ends; def until the
// first read is done.
func refreshed[T any](ctx context.Context, vendors []adapter.Vendor, fn func(adapter.Vendor) T, def T) func(adapter.Vendor) T {
	var mu sync.Mutex
	got := map[adapter.Vendor]T{}
	go func() {
		for {
			for _, v := range vendors {
				r := fn(v)
				mu.Lock()
				got[v] = r
				mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(refreshEvery):
			}
		}
	}()
	return func(v adapter.Vendor) T {
		mu.Lock()
		defer mu.Unlock()
		if r, ok := got[v]; ok {
			return r
		}
		return def
	}
}

// switchCheck says why the session in cwd may not switch to a vendor:
// the repo's allowed_vendors, its CLI not on PATH, or, unless [policy]
// required = false, a safety policy that cannot reach it (S-6; a switch
// never carries --no-policy over). The policy needs a probe of the CLI's
// help, which can take up to probeTimeout, so it runs in the background
// (S-10): until it is done the check says supervisor.ErrChecking. A
// probe that succeeded is kept for this launch; a failed one, or a policy
// it cannot reach, is reported once and forgotten, so a later pick probes
// again.
func switchCheck(ctx context.Context, cfg config.Config, cwd string) func(adapter.Vendor) error {
	type result struct {
		done bool
		caps []string
		err  error
	}
	var mu sync.Mutex
	probed := map[adapter.Vendor]*result{}
	return func(next adapter.Vendor) error {
		if _, err := allowedIn(cfg, cwd, next); err != nil {
			return err
		}
		path, err := probe.LookPath(vendorBin(next))
		if err != nil {
			return fmt.Errorf("%s is not on PATH", next)
		}
		if !cfg.Policy.Required {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		r := probed[next]
		if r == nil {
			r = &result{}
			probed[next] = r
			go func() {
				caps, _, err := probeCaps(ctx, next, path)
				mu.Lock()
				defer mu.Unlock()
				r.done, r.caps, r.err = true, caps, err
			}()
		}
		if !r.done {
			return supervisor.ErrChecking
		}
		if r.err != nil {
			delete(probed, next)
			return fmt.Errorf("probing %s for the safety policy failed (%s); pick it again to retry", next, termsafe.Line(r.err.Error()))
		}
		if why, perr := policyDelivery(next, r.caps); why != "" {
			delete(probed, next)
			if perr != nil {
				why += ": " + perr.Error()
			}
			return fmt.Errorf("the safety policy cannot be applied to %s: %s", next, why)
		}
		return nil
	}
}

// hasQuota reports whether v has no plan limit recorded now
// (limits.State, the router's skip rule). An index aggy cannot read
// counts as quota: the user still confirms the switch.
func hasQuota(s *store.Store, v adapter.Vendor) bool {
	if s == nil {
		return true
	}
	ctx := context.Background()
	_, limited, err := limits.State{Store: s}.Limited(ctx, v, time.Now())
	return err != nil || !limited
}

// childChoices lists the parent's interactive children of v, latest
// first, for the picker: a short id, the title, and when it was last
// active, made safe for the terminal. Without an index, none.
func childChoices(s *store.Store, parentID string, v adapter.Vendor, now time.Time) []supervisor.ChildChoice {
	if s == nil {
		return nil
	}
	ctx := context.Background()
	cs, err := s.Children(ctx, parentID)
	if err != nil {
		return nil
	}
	var out []supervisor.ChildChoice
	for _, c := range cs {
		if c.Kind != "" || c.Vendor != string(v) {
			continue
		}
		label := shortID(c.ID) + "  " + now.Sub(c.LastActive).Round(time.Minute).String() + " ago"
		if c.Title != "" {
			label += "  " + termsafe.Field(c.Title)
		}
		if r := []rune(label); len(r) > 70 {
			label = string(r[:70])
		}
		out = append(out, supervisor.ChildChoice{ID: c.ID, Label: label})
	}
	return out
}

// childRecorded reports whether the parent's interactive child id of v
// is still in the index, as plan will look for it after a switch
// (T6770). Without an index, or when the read fails, it says yes: the
// switch goes on, as before the check, and plan reports a child gone.
func childRecorded(s *store.Store, parentID string, v adapter.Vendor, id string) bool {
	if s == nil {
		return true
	}
	cs, err := s.Children(context.Background(), parentID)
	if err != nil {
		return true
	}
	for _, c := range cs {
		if c.ID == id && c.Kind == "" && c.Vendor == string(v) {
			return true
		}
	}
	return false
}

// childCwd is the folder a switch to child id launches in, else fallback.
func childCwd(s *store.Store, parentID string, v adapter.Vendor, id, fallback string) string {
	if s == nil || id == "" {
		return fallback
	}
	cs, err := s.Children(context.Background(), parentID)
	if err != nil {
		return fallback
	}
	for _, c := range cs {
		if c.ID == id && c.Vendor == string(v) && c.Cwd != "" {
			return c.Cwd
		}
	}
	return fallback
}

// openIndexRO opens the index read-only; a test counts the opens.
var openIndexRO = store.OpenReadOnly

// roIndex is the one read-only index handle a launch's picker reads go
// through (T6770), opened on first use (again after a failed open) and
// closed by close, after which get gives nil.
type roIndex struct {
	mu     sync.Mutex
	s      *store.Store
	closed bool
}

// get returns the handle, or nil while the index cannot be opened.
func (r *roIndex) get() *store.Store {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.s == nil && !r.closed {
		if path, err := store.DefaultPath(); err == nil {
			if s, err := openIndexRO(context.Background(), path); err == nil {
				r.s = s
			}
		}
	}
	return r.s
}

// close closes the handle for good. A read already running on it ends
// first (database/sql's Close waits for it) or fails.
func (r *roIndex) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.s != nil {
		_ = r.s.Close()
		r.s = nil
	}
}

// probeCaps probes v's CLI at path for the flags whose presence is only
// known from its help, and its version.
func probeCaps(ctx context.Context, v adapter.Vendor, path string) ([]string, string, error) {
	probed, err := map[adapter.Vendor]adapter.Adapter{adapter.Claude: claude.Adapter{Path: path},
		adapter.Codex: codex.Adapter{Path: path}, adapter.Grok: grok.Adapter{Path: path},
		adapter.OpenCode: opencode.Adapter{Path: path}}[v].Probe(ctx, execrun.ExecRunner(probeTimeout))
	if err != nil {
		return nil, "", err
	}
	return probed.Capabilities, probed.Version, nil
}

// pickerVendors lists the vendors in order, except on, that aggy setup
// recorded and did not find signed out. Codex and Grok have no verified
// sign-in check, so "unknown" counts. Without an index, the list is empty.
func pickerVendors(order []adapter.Vendor, on adapter.Vendor) []adapter.Vendor {
	path, err := store.DefaultPath()
	if err != nil {
		return nil
	}
	ctx := context.Background()
	s, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return nil
	}
	defer func() { _ = s.Close() }()
	rows, err := s.VendorStatuses(ctx)
	if err != nil {
		return nil
	}
	ok := map[adapter.Vendor]bool{}
	for _, r := range rows {
		ok[adapter.Vendor(r.Vendor)] = r.Connected != string(adapter.SignedOut)
	}
	var out []adapter.Vendor
	for _, v := range order {
		if v != on && ok[v] {
			out = append(out, v)
		}
	}
	return out
}

// vendorBin is the vendor CLI's command name.
func vendorBin(v adapter.Vendor) string {
	return map[adapter.Vendor]string{adapter.Claude: claude.Bin, adapter.Codex: codex.Bin, adapter.Grok: grok.Bin, adapter.OpenCode: opencode.Bin}[v]
}

// proxyTools are the --tool names a tool proxy may start for: none on
// OpenCode, which has no managed tools yet.
func proxyTools(v adapter.Vendor, tools []string) []string {
	if v == adapter.OpenCode {
		return nil
	}
	return tools
}

// newUUID returns a random (version 4) UUID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
