package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
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

// newestNative is the newest native session of v (or of every vendor the repo
// allows) in cwd that no child of aggy's holds, or false.
func newestNative(ctx context.Context, s *store.Store, cfg config.Config, cwd string, v adapter.Vendor, files *sessionFiles) (nativeSession, bool) {
	var best nativeSession
	found := false
	for _, x := range sessionVendors(cfg, cwd, v) {
		ns, _ := nativeSessions(files, x, cwd)
		for _, n := range ns {
			if _, err := s.ChildBySession(ctx, string(x), n.ID); !errors.Is(err, store.ErrNotFound) {
				continue
			}
			if !found || n.Modified.After(best.Modified) {
				best, found = n, true
			}
			break
		}
	}
	return best, found
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
// commit records both and logs the adoption. The index's unique (vendor,
// session id) keeps a second adoption of the same session from being recorded.
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

// byName plans a session the CLI resumes by name (target.ResumeName): a new
// parent and a child with no id yet, which adoptNamed gives one when the CLI
// exits.
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
// as the CLI exits.
const byNameNote = "claude or grok finds the session by that name; aggy records which one when it exits, so until then aggy counts no turn end there and a switch waits for you to quit it"

// adoptNamed gives a child resumed by name its session id once its CLI has
// exited (T6965): the one session of the child's directory, in the vendor's own
// store, that changed since the launch began. Two or none, or one another child
// holds, leave the row as it was; for the last, the new row is dropped, as that
// session was recorded already. A hook's word alone never sets an id
// (sessionIndex), and this needs no hook.
func adoptNamed(ctx context.Context, s *store.Store, child store.Child, began time.Time, files *sessionFiles) {
	v, err := adapter.ParseVendor(child.Vendor)
	if err != nil {
		return
	}
	since := began.Truncate(time.Second) // file times can be whole seconds
	var changed []nativeSession
	for _, n := range scanNative(files, v, child.Cwd) {
		if !n.Modified.Before(since) {
			changed = append(changed, n)
		}
	}
	if len(changed) != 1 {
		return
	}
	n := changed[0]
	if c, err := s.ChildBySession(ctx, child.Vendor, n.ID); err == nil {
		if c.ID != child.ID {
			_, _ = s.DeleteUnstartedChild(ctx, child.ID)
		}
		return
	}
	if err := s.SetChildSessionID(ctx, child.ID, n.ID); err != nil {
		return
	}
	_ = s.SetChildFileSeen(ctx, child.ID)
	_ = s.AppendEvent(ctx, store.Event{At: time.Now(), ParentID: child.ParentID, ChildID: child.ID, Type: store.EventSession,
		Detail: map[string]any{"vendor": child.Vendor, "session": n.ID, "via": "adopted by name"}})
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
// or, for Claude and Grok, a name their own --resume takes. The value is only
// ever that: never read as a flag, a subcommand, or a path
// (adapter.ValidResumeName). v is the vendor named, or "".
func resolveResume(t *target, v adapter.Vendor, value string) (diag.Code, error) {
	id, msg := sessionArg(value)
	uuid := adapter.IsUUID(value)
	if msg != "" && !uuid {
		// Neither an aggy id nor a vendor's: a name, checked before anything
		// is opened.
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
		if ns, ok := findNative(&sessionFiles{readOnly: true}, sessionVendors(cfg, cwd, v), cwd, value); ok {
			t.Native = &ns
			return diag.Code{}, nil
		}
		return diag.CodeNotFound, fmt.Errorf("%w %s in this folder: aggy -r lists the sessions here, aggy's and the CLIs' own", errNoSession, termsafe.Field(value))
	}
	pv := v
	if pv == "" {
		pv = launchVendor()
	}
	if pv != adapter.Claude && pv != adapter.Grok {
		return diag.CodeNotFound, fmt.Errorf("%w %s: aggy passes a session name only to claude and grok, whose --resume takes one; aggy -r lists the sessions here", errNoSession, termsafe.Field(value))
	}
	if err := adapter.ValidResumeName(value); err != nil {
		return diag.CodeUsage, fmt.Errorf("%q is not a session id or a name aggy passes on: %v", termsafe.Field(value), err)
	}
	t.ResumeName, t.Vendor = value, pv
	return diag.Code{}, nil
}

// pickIn is where the session picker reads the person's answer; a test sets it.
var pickIn io.Reader = os.Stdin

// maxPicked bounds the rows the session picker shows.
const maxPicked = 30

// pickSession is -r with no value (T6965): a list on aggy's own screen, before
// any CLI starts, of this folder's sessions, aggy's and the CLIs' own, newest
// first, and the one the person picks goes into t. picked is false when the
// person picked none. v is the vendor named, or "".
func pickSession(t *target, v adapter.Vendor, out io.Writer) (picked bool, code diag.Code, err error) {
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
	cs, err := sessionChoices(ctx, s, cwd, sessionVendors(cfg, cwd, v), &sessionFiles{readOnly: true})
	if err != nil {
		return false, diag.CodeStore, err
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
	line, _ := bufio.NewReader(io.LimitReader(pickIn, maxAnswer)).ReadString('\n')
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
