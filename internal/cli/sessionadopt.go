package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/termsafe"
)

// sessionVendors are the vendors whose sessions a resume in cwd looks at: v
// when one is named, else every vendor the repo allows and the person has not
// turned off, in aggy's usual order.
func sessionVendors(cfg config.Config, cwd string, v adapter.Vendor) []adapter.Vendor {
	if v != "" {
		return []adapter.Vendor{v}
	}
	eff, err := repoEffective(cfg, cwd)
	if err != nil {
		return nil
	}
	var out []adapter.Vendor
	for _, x := range adapter.Vendors() {
		if eff.Allows(x) && !cfg.Vendors.For(x).Off {
			out = append(out, x)
		}
	}
	return out
}

// cliInstalled reports whether v's CLI is on PATH; a test replaces it.
var cliInstalled = func(v adapter.Vendor) bool {
	_, err := probe.LookPath(vendorBin(v))
	return err == nil
}

// newestNative is the session -c takes from the vendors' own stores in cwd
// (PR #127 review): the newest session of v (or of every vendor the repo
// allows, and with installed only those whose CLI is on PATH) that no child of
// aggy's holds, is not modified past the future limit (nativeFuture), and
// checks out as this folder's (checkNative), with its first prompt. It reads
// the heads of as few files as that takes, within nativeBudget; slow says the
// look ran out of time and found nothing.
func newestNative(ctx context.Context, s *store.Store, cfg config.Config, cwd string, v adapter.Vendor, files *sessionFiles, installed bool) (n nativeSession, ok, slow bool) {
	var vs []adapter.Vendor
	for _, x := range sessionVendors(cfg, cwd, v) {
		if !installed || cliInstalled(x) {
			vs = append(vs, x)
		}
	}
	type found struct {
		n  nativeSession
		ok bool
	}
	r, done := withinBudget(func() found {
		nf := &sessionFiles{getenv: files.getenv, now: files.now, readOnly: true}
		now := time.Now()
		var cands []nativeSession
		for _, x := range vs {
			cands = append(cands, scanNative(nf, x, cwd)...)
		}
		sortNative(cands, now)
		for i, n := range cands {
			if i == maxNativeChecked || nativeFuture(n, now) {
				break
			}
			if _, err := s.ChildBySession(ctx, string(n.Vendor), n.ID); !errors.Is(err, store.ErrNotFound) {
				continue
			}
			if checkNative(&n, cwd, true) {
				return found{n, true}
			}
		}
		return found{}
	})
	return r.n, r.ok, !done
}

// nativeOwner is the parent and child that already hold ns, or an empty child
// id when no child of aggy's does.
func nativeOwner(ctx context.Context, s *store.Store, ns nativeSession) (store.Parent, string, error) {
	c, err := s.ChildBySession(ctx, string(ns.Vendor), ns.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Parent{}, "", nil
	}
	if err != nil {
		return store.Parent{}, "", err
	}
	ps, err := s.ParentsByPrefix(ctx, c.ParentID)
	if err != nil {
		return store.Parent{}, "", err
	}
	for _, p := range ps {
		if p.ID == c.ParentID {
			return p, c.ID, nil
		}
	}
	return store.Parent{}, "", fmt.Errorf("%w: the parent of session %s", errNoSession, shortID(c.ID))
}

// freshParent plans a new parent in cwd, which must be a directory.
func (p *launchPlan) freshParent(cwd string, now time.Time) error {
	if fi, err := safefs.Stat(cwd); err != nil || !fi.IsDir() {
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s is not a directory", termsafe.Line(cwd))
		}
		return err
	}
	id, err := newUUID()
	if err != nil {
		return err
	}
	p.parent = store.Parent{ID: id, Dir: cwd, CreatedAt: now, LastActive: now}
	if p.parent.DirKey, err = safefs.CanonicalPath(cwd); err != nil {
		return err
	}
	p.parent.DirID = dirID(cwd)
	p.newParent = true
	return nil
}

// adopt plans the first use of a native session through aggy (T6965): a new
// parent in cwd and a child that holds ns, reopened with the CLI's resume flag.
// commit records both at once (store.InsertParentChild); when another aggy
// adopted the session first, it records neither and the launch plans again,
// through that aggy's child (errAdoptedElsewhere).
func (p *launchPlan) adopt(cfg config.Config, cwd string, ns nativeSession, now time.Time) error {
	if err := adapter.ValidSessionID(ns.Vendor, ns.ID); err != nil {
		return err
	}
	if err := p.freshParent(cwd, now); err != nil {
		return err
	}
	id, err := newUUID()
	if err != nil {
		return err
	}
	when := ns.Modified
	if when.IsZero() || when.After(now) {
		when = now
	}
	title := sessionTitle(ns.Title)
	p.parent.Title = title
	p.child = store.Child{ID: id, ParentID: p.parent.ID, Vendor: string(ns.Vendor), VendorSessionID: ns.ID,
		Model: cfg.Vendors.For(ns.Vendor).Model, TraceID: dlog.Trace(), Cwd: cwd, Status: "open",
		CreatedAt: now, LastActive: when, VendorHome: ns.Home, Title: title, FileSeen: true}
	p.newChild, p.adopted, p.resume = true, true, ns.ID
	return nil
}

// errAdoptedElsewhere: another aggy recorded the native session between this
// launch's plan and its commit, so this one recorded nothing (PR #127 review).
var errAdoptedElsewhere = errors.New("another aggy took that session on just now")

// byName plans a session the CLI resumes by name (target.ResumeName): a new
// parent and a child with no id yet. The CLI's SessionStart hook says which
// session the name found, and adoptNamed records it, or drops both rows, once
// the CLI exits.
func (p *launchPlan) byName(ctx context.Context, s *store.Store, cfg config.Config, cwd string, t target, now time.Time) error {
	if t.Vendor != adapter.Claude && t.Vendor != adapter.Grok {
		return fmt.Errorf("aggy passes a session name only to claude and grok, not %s", t.Vendor)
	}
	if err := adapter.ValidResumeName(t.ResumeName); err != nil {
		return err
	}
	if err := p.freshParent(cwd, now); err != nil {
		return err
	}
	if err := p.newChildOf(ctx, s, cfg, t.Vendor, now); err != nil {
		return err
	}
	p.child.VendorSessionID = ""
	p.resumeName = t.ResumeName
	return nil
}

// byNameNote is said at a launch by name: aggy learns the session's id only
// from the CLI's SessionStart hook.
const byNameNote = "claude or grok finds the session by that name; aggy records it as it exits, if the CLI's SessionStart hook said which session it was, so until then aggy counts no turn end there and a switch waits for you to quit it"

// namedSession is the session id the CLI's own SessionStart hook reported in
// a launch by name (PR #127 review): the first SessionStart from the session's
// own process tree, which is the one the name resumed (a later /clear starts
// another). It is all adoptNamed trusts; no file's time stands in for it.
type namedSession struct {
	vendor adapter.Vendor
	mu     sync.Mutex
	seen   bool
	id     string
}

// hook takes a hook event of the launch.
func (n *namedSession) hook(r supervisor.Request) {
	if r.Event != "SessionStart" || r.Vendor != string(n.vendor) || !r.FromSession {
		return
	}
	ev := hook.Parse(n.vendor, r.Payload)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen {
		return
	}
	n.seen = true
	if adapter.ValidSessionID(n.vendor, ev.SessionID) == nil {
		n.id = ev.SessionID
	}
}

// reported is the id the hook reported, or "".
func (n *namedSession) reported() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.id
}

// adoptNamed records, once the CLI has exited, which session a launch by name
// reopened (T6965, PR #127 review): the id the CLI's SessionStart hook reported
// for this run, when that session's file is in the child's folder and checks
// out as that folder's (findNative). Otherwise it records nothing, and drops
// the child and its parent, which were made for this launch, so no empty
// session is left; when the id is one another child of aggy's holds, it says
// which. It says what it did on out.
func adoptNamed(ctx context.Context, s *store.Store, child store.Child, reported string, files *sessionFiles, out io.Writer) {
	v, err := adapter.ParseVendor(child.Vendor)
	if err != nil {
		return
	}
	drop := func(why string) {
		if ok, _ := s.DeleteUnstartedChild(ctx, child.ID); ok {
			_, _ = s.DeleteEmptyParent(ctx, child.ParentID)
		}
		fmt.Fprintf(out, "aggy: %s\n", why)
	}
	if reported == "" {
		drop(fmt.Sprintf("%s did not say which session that name reopened (its SessionStart hook did not reach aggy), so aggy recorded none; aggy -r lists it, by its id", v))
		return
	}
	if c, err := s.ChildBySession(ctx, child.Vendor, reported); err == nil && c.ID != child.ID {
		drop(fmt.Sprintf("that name reopened %s session %s, which is aggy's session %s already; nothing new recorded", v, shortID(reported), shortID(c.ParentID)))
		return
	}
	n, ok := findNative(files, []adapter.Vendor{v}, child.Cwd, reported)
	if !ok {
		drop(fmt.Sprintf("that name reopened %s session %s, which is not this folder's, so aggy recorded none", v, shortID(reported)))
		return
	}
	if err := s.SetChildSessionID(ctx, child.ID, n.ID); err != nil {
		drop(fmt.Sprintf("aggy could not record %s session %s: %s", v, shortID(n.ID), termsafe.Line(err.Error())))
		return
	}
	_ = s.SetChildFileSeen(ctx, child.ID)
	_ = s.AppendEvent(ctx, store.Event{At: time.Now(), ParentID: child.ParentID, ChildID: child.ID, Type: store.EventSession,
		Detail: map[string]any{"vendor": child.Vendor, "session": n.ID, "via": "adopted by name"}})
	fmt.Fprintf(out, "aggy: %s session %s is aggy's session %s now\n", v, shortID(n.ID), shortID(child.ParentID))
}

// indexRO opens the index read-only, or nil when there is none yet or it
// cannot be read: then every session found is native.
func indexRO(ctx context.Context) *store.Store {
	path, err := store.DefaultPath()
	if err != nil {
		return nil
	}
	s, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return nil
	}
	return s
}

// aggyIDKnown reports whether id is a prefix of a recorded parent's or child's id.
func aggyIDKnown(ctx context.Context, s *store.Store, id string) bool {
	if ps, err := s.ParentsByPrefix(ctx, id); err == nil && len(ps) > 0 {
		return true
	}
	_, kids, err := childByPrefix(ctx, s, id)
	return err == nil && len(kids) > 0
}

// resolveResume sorts the value of -r/--resume (T6965) into t: an aggy id (a
// prefix of a recorded session's, as before), a vendor session id (one aggy
// recorded, or one found in this folder's own session store, adopted on use),
// or, for Claude and Grok, a name their own --resume takes. A value shaped like
// an id (4 to 36 hex digits and dashes, or a UUID) that matches nothing is
// refused, never passed on as a name (PR #127 review), and a name must pass
// adapter.ValidResumeName and not be one of the CLI's subcommands. The value
// is only ever that: never read as a flag, a subcommand, or a path. v is the
// vendor named, or "".
func resolveResume(t *target, v adapter.Vendor, value string) (diag.Code, error) {
	id, msg := sessionArg(value)
	uuid := adapter.IsUUID(value)
	idLike := msg == "" || uuid
	if !idLike {
		// A name, checked before anything is opened.
		if err := adapter.ValidResumeName(value); err != nil {
			return diag.CodeUsage, fmt.Errorf("%q is not a session id or a name aggy passes on: %v", termsafe.Field(value), err)
		}
	}
	ctx := context.Background()
	cfg, err := config.LoadDefault()
	if err != nil {
		return diag.CodeConfig, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return diag.CodeLaunchFailed, err
	}
	s := indexRO(ctx)
	if s != nil {
		defer func() { _ = s.Close() }()
	}
	if msg == "" && s != nil && aggyIDKnown(ctx, s, id) {
		t.ParentID = id
		return diag.Code{}, nil
	}
	if uuid {
		all := []adapter.Vendor{v}
		if v == "" {
			all = adapter.Vendors()
		}
		if s != nil {
			for _, x := range all {
				if c, err := s.ChildBySession(ctx, string(x), value); err == nil {
					t.ParentID = c.ID
					return diag.Code{}, nil
				}
			}
		}
		ns, ok := withinBudget(func() *nativeSession {
			if n, ok := findNative(&sessionFiles{readOnly: true}, sessionVendors(cfg, cwd, v), cwd, value); ok {
				return &n
			}
			return nil
		})
		if !ok {
			return diag.CodeNotFound, fmt.Errorf("%w %s: %s", errNoSession, termsafe.Field(value), nativeSlowNote)
		}
		if ns != nil {
			t.Native = ns
			return diag.Code{}, nil
		}
	}
	if idLike {
		return diag.CodeNotFound, fmt.Errorf("%w %s in this folder: aggy -r lists the sessions here, aggy's and the CLIs' own (a name shaped like an id is not passed on)", errNoSession, termsafe.Field(value))
	}
	pv := v
	if pv == "" {
		pv = launchVendor()
	}
	if pv != adapter.Claude && pv != adapter.Grok {
		return diag.CodeNotFound, fmt.Errorf("%w %s: a %s session is reopened by its id, and aggy -r lists them; only claude and grok take a session's name", errNoSession, termsafe.Field(value), pv)
	}
	// "help" too: commander and clap both add that subcommand themselves.
	if strings.EqualFold(value, "help") || slices.ContainsFunc(passFlags(pv).Commands, func(c string) bool { return strings.EqualFold(c, value) }) {
		return diag.CodeUsage, fmt.Errorf("%q is one of %s's subcommands, not a session name aggy passes on", termsafe.Field(value), pv)
	}
	t.ResumeName, t.Vendor = value, pv
	return diag.Code{}, nil
}

// pickIn is where the session picker reads the person's answer; a test sets it.
var pickIn io.Reader = os.Stdin

// maxPicked bounds the rows the session picker shows.
const maxPicked = 30

// readLine reads one line from r a byte at a time, at most limit bytes, so
// nothing past the newline is taken from a terminal: what the person typed
// ahead stays there for the CLI. It returns the line without its newline.
func readLine(r io.Reader, limit int) (string, error) {
	var b []byte
	var one [1]byte
	for len(b) < limit {
		n, err := r.Read(one[:])
		if n == 1 {
			if one[0] == '\n' {
				return string(b), nil
			}
			b = append(b, one[0])
			continue
		}
		if err != nil {
			return string(b), err
		}
	}
	return string(b), nil
}

// pickSession is -r with no value (T6965): a list on aggy's own screen, before
// any CLI starts, of this folder's sessions, aggy's and the CLIs' own, newest
// first, and the one the person picks goes into t. picked is false when the
// person picked none. v is the vendor named, or "". It needs a terminal on
// stdin, so piped input never picks (PR #127 review).
func pickSession(t *target, v adapter.Vendor, out io.Writer) (picked bool, code diag.Code, err error) {
	if !stdinTerminal() {
		return false, diag.CodeNoTerminal, errors.New("the session list needs a terminal on stdin: give aggy -r a session id instead")
	}
	ctx := context.Background()
	cfg, err := config.LoadDefault()
	if err != nil {
		return false, diag.CodeConfig, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return false, diag.CodeLaunchFailed, err
	}
	s := indexRO(ctx)
	if s != nil {
		defer func() { _ = s.Close() }()
	}
	cs, slow, err := sessionChoices(ctx, s, cwd, sessionVendors(cfg, cwd, v), &sessionFiles{readOnly: true})
	if err != nil {
		return false, diag.CodeStore, err
	}
	if slow {
		fmt.Fprintf(out, "aggy: %s\n", nativeSlowNote)
	}
	if len(cs) == 0 {
		what := "session"
		if v != "" {
			what = string(v) + " session"
		}
		return false, diag.CodeNotFound, fmt.Errorf("%w to reopen: no %s here, aggy's or the CLI's own (aggy starts a new one)", errNoSession, what)
	}
	if len(cs) > maxPicked {
		cs = cs[:maxPicked]
	}
	fmt.Fprint(out, renderChoices(cs, time.Now()))
	fmt.Fprintf(out, "Pick 1-%d (Enter to cancel): ", len(cs))
	line, _ := readLine(pickIn, maxAnswer)
	a := strings.TrimSpace(line)
	if a == "" || strings.EqualFold(a, "q") {
		fmt.Fprintln(out, "Nothing reopened.")
		return false, diag.Code{}, nil
	}
	n, err := strconv.Atoi(a)
	if err != nil || n < 1 || n > len(cs) {
		return false, diag.CodeUsage, fmt.Errorf("%q is not one of 1-%d", termsafe.Field(a), len(cs))
	}
	c := cs[n-1]
	if c.Native != nil {
		t.Native = c.Native
	} else {
		t.ParentID = c.ChildID
	}
	return true, diag.Code{}, nil
}

// renderChoices is the session picker's list: number, age, vendor, whose
// session it is, and its first prompt or title, escaped (sessionTitle).
func renderChoices(cs []sessionChoice, now time.Time) string {
	var b strings.Builder
	b.WriteString("Sessions in this folder, newest first:\n")
	w := len(strconv.Itoa(len(cs)))
	for i, c := range cs {
		whose := "aggy " + shortID(c.ChildID)
		if c.Native != nil {
			whose = string(c.Vendor) + "'s own"
		}
		title := sessionTitle(c.Title)
		if title == "" {
			title = "(no prompt yet)"
		}
		fmt.Fprintf(&b, "  %*d  %-9s %-8s %-14s %s\n", w, i+1, ago(c.When, now), c.Vendor, whose, title)
	}
	return b.String()
}
