package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/brief"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/limits"
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
	const usage = "usage: aggy run [--direct] [--no-policy] [--tool NAME]... <claude|codex|grok>\n"
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
					fmt.Fprint(stderr, usage)
					return 2
				}
				args, val = args[1:], args[1]
			}
			if !tooldef.ValidName(val) {
				fmt.Fprintf(stderr, "aggy run: %q is not a tool name\n", val)
				return 2
			}
			tools = append(tools, val)
		default:
			fmt.Fprint(stderr, usage)
			return 2
		}
		args = args[1:]
	}
	if len(args) != 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	v, err := adapter.ParseVendor(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "aggy run: %v\n", err)
		return 2
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
	for {
		var next switchTo
		code := launchOnce(name, t, stderr, &next, q)
		if next.vendor == "" {
			return code
		}
		c := next.choice
		t = target{ParentID: next.parentID, Vendor: next.vendor, Child: c.Child, NewChild: c.New, Model: c.Model, Brief: c.Brief}
	}
}

// switchTo is the vendor a CLI exited for, its parent, and the switch's
// options.
type switchTo struct {
	vendor   adapter.Vendor
	parentID string
	choice   supervisor.Choice
}

// diagQueue is how many records the supervisor's diagnostic queue holds;
// diagFlush bounds how long aggy waits for it to be written at the end.
const (
	diagQueue = 256
	diagFlush = 2 * time.Second
)

// launchOnce is one launch. It returns the CLI's exit code, and sets next
// when the CLI exited for a switch. q takes the supervisor's records.
func launchOnce(name string, t target, stderr io.Writer, next *switchTo, q *diag.Queue) int {
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
		return fail(diag.CodeLaunchFailed, err)
	}
	v := adapter.Vendor(pl.child.Vendor)
	ids = diag.Rec{Parent: pl.parent.ID, Child: pl.child.ID, Vendor: string(v)}
	// The child's repo decides which vendors may see it, the one on
	// screen and the ones the switch picker offers.
	eff, err := allowedIn(cfg, pl.child.Cwd, v)
	if err != nil {
		return fail(diag.CodeConfig, err)
	}
	path, err := probe.LookPath(vendorBin(v))
	if err != nil {
		return fail(diag.CodeCLIMissing, err)
	}
	// The probe finds the flags whose presence is only known from help:
	// Codex --no-alt-screen and -c, Claude --settings, Grok --rules and
	// --deny.
	caps, err := probeCaps(ctx, v, path)
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
	// The policy is checked before the launch is recorded (commit), so a
	// refused launch adds no parent, child, or lease. plan's refresh may
	// already have raised last-active times from session files (D19-9).
	var warns []string
	if t.NoPolicy {
		warns = append(warns, "starting without the safety policy (--no-policy)")
	} else {
		pw, err := addPolicy(v, caps, cfg.Policy.Required, &req)
		if err != nil {
			return fail(diag.CodePolicy, err)
		}
		warns = append(warns, pw...)
	}
	// Memory comes from the child's own directory, never the current one:
	// aggy resume <id> works from anywhere.
	// A crashed launch's folder is left to sweepMemoryDirs above.
	memDir, err := memoryDir(runDir, parentID)
	if err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	cleanup, mw := addMemory(v, caps, pl.child.Cwd, memDir, &req)
	defer cleanup()
	// The tools that are on, for this child's own vendor and repo: a
	// switch or resume is wired again for the CLI it starts. Their files
	// share the launch's private folder, removed when the CLI exits.
	tw := addTools(ctx, v, caps, pl.child.Cwd, memDir, t.Tools, &req)
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
	cmd, err := a.Interactive(req)
	if err != nil {
		return fail(diag.CodeUnsupported, err)
	}
	cmd.Path = path
	lease, err := pl.commit(ctx, s, time.Now())
	if err != nil {
		return fail(diag.CodeLaunchFailed, err)
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
		"brief": t.Brief, "model_set": t.Model != "", "direct": t.Direct}
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
		code, err := runDirect(cmd, runDir, parentID, v, assigned, cids.set, func(r diag.Rec) {
			r.Parent, r.Child, r.Vendor = parentID, pl.child.ID, string(v)
			q.LogWait(r, 100*time.Millisecond) // the socket's goroutines
		})
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
	onHook, stopIndex := startIndex(parentID, v, assigned, cids.set)
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
	// supervisor's loop never waits on SQLite.
	parent := pl.parent.ID
	children := refreshed(pctx, vendors, func(o adapter.Vendor) []supervisor.ChildChoice { return childChoices(parent, o, time.Now()) }, nil)
	quota := refreshed(pctx, vendors, hasQuota, true)
	// The near-limit share is read only at a turn end, and only when
	// warn_at is on (L60-2); the supervisor calls it off its loop.
	var used func(adapter.Vendor) (supervisor.Share, bool)
	if eff.Switch.WarnAt > 0 {
		var allowance limits.AllowanceCache
		used = func(o adapter.Vendor) (supervisor.Share, bool) { return usedShare(o, &allowance, cids.current()) }
	}
	ex, err := supervisor.Run(supervisor.Options{
		In: os.Stdin, Out: os.Stdout, Command: cmd,
		Start:      startPTY,
		SwitchKey:  key,
		Vendors:    vendors,
		RunDir:     runDir,
		ParentID:   parentID,
		OnHook:     onHook,
		Vendor:     v,
		CanSwitch:  canSwitch,
		StartsTurn: t.Brief,
		SessionID:  cids.current,
		Children:   children,
		Limits:     limitHit,
		HasQuota:   quota,
		Used:       used,
		WarnAt:     eff.Switch.WarnAt,
		Diag:       q,
		ChildID:    pl.child.ID,
	})
	dlog.Release()
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
		*next = switchTo{vendor: ex.SwitchTo, parentID: parentID, choice: ex.Next}
	}
	return ex.Code
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
				caps, err := probeCaps(ctx, next, path)
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
func hasQuota(v adapter.Vendor) bool {
	path, err := store.DefaultPath()
	if err != nil {
		return true
	}
	ctx := context.Background()
	s, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return true
	}
	defer func() { _ = s.Close() }()
	_, limited, err := limits.State{Store: s}.Limited(ctx, v, time.Now())
	return err != nil || !limited
}

// childChoices lists the parent's interactive children of v, latest
// first, for the picker: a short id, the title, and when it was last
// active, made safe for the terminal. Without an index, none.
func childChoices(parentID string, v adapter.Vendor, now time.Time) []supervisor.ChildChoice {
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

// probeCaps probes v's CLI at path for the flags whose presence is only
// known from its help.
func probeCaps(ctx context.Context, v adapter.Vendor, path string) ([]string, error) {
	probed, err := map[adapter.Vendor]adapter.Adapter{adapter.Claude: claude.Adapter{Path: path},
		adapter.Codex: codex.Adapter{Path: path}, adapter.Grok: grok.Adapter{Path: path}}[v].Probe(ctx, execrun.ExecRunner(probeTimeout))
	if err != nil {
		return nil, err
	}
	return probed.Capabilities, nil
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
	return map[adapter.Vendor]string{adapter.Claude: claude.Bin, adapter.Codex: codex.Bin, adapter.Grok: grok.Bin}[v]
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
