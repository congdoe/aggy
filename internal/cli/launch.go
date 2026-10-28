package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

// target says which parent a launch opens.
type target struct {
	// New starts another parent in the current directory, with Vendor
	// (empty means the default vendor).
	New    bool
	Vendor adapter.Vendor
	// ParentID reopens that parent (an id or a unique prefix). Empty, and
	// not New, reopens the latest parent in the current directory, or
	// starts one there.
	ParentID string
}

// launchPlan is what a launch puts on screen.
type launchPlan struct {
	parent    store.Parent
	newParent bool
	child     store.Child
	newChild  bool
	// resume is the vendor session id reopened with the CLI's resume
	// flag; empty for a new child, whose pre-assigned id (Claude and
	// Grok; Codex takes none) is child.VendorSessionID.
	resume string
}

// plan decides the parent and child a launch opens. It writes only what
// refreshChildren finds on disk; the launch itself is recorded by commit.
// Notes for the user (a session the vendor no longer has, an id never
// recorded) go to stderr.
func plan(ctx context.Context, s *store.Store, cfg config.Config, cwd string, t target, now time.Time, stderr io.Writer) (launchPlan, error) {
	var p launchPlan
	switch {
	case t.New:
		p.newParent = true
	case t.ParentID != "":
		ps, err := s.ParentsByPrefix(ctx, t.ParentID)
		if err != nil {
			return p, err
		}
		switch len(ps) {
		case 0:
			return p, fmt.Errorf("no session %s (aggy sessions lists them)", t.ParentID)
		case 1:
			p.parent = ps[0]
		default:
			return p, fmt.Errorf("%d sessions start with %s; give more of the id", len(ps), t.ParentID)
		}
	default:
		par, err := s.LatestParentInDir(ctx, cwd)
		switch {
		case errors.Is(err, store.ErrNotFound):
			p.newParent = true
		case err != nil:
			return p, err
		default:
			p.parent = par
		}
	}
	if p.newParent {
		id, err := newUUID()
		if err != nil {
			return p, err
		}
		p.parent = store.Parent{ID: id, Dir: cwd, CreatedAt: now, LastActive: now}
		return p, p.newChildOf(ctx, s, cfg, t.Vendor, now)
	}

	children, err := refreshChildren(ctx, s, p.parent.ID, &sessionFiles{})
	if err != nil {
		return p, err
	}
	// The interactive child last on screen: the most recently active one.
	var last *store.Child
	for i := range children {
		if children[i].Kind == "" {
			last = &children[i]
			break
		}
	}
	short := shortID(p.parent.ID)
	switch {
	case last == nil:
		v, _ := adapter.ParseVendor(p.parent.LastVendor)
		return p, p.newChildOf(ctx, s, cfg, v, now)
	case last.Status == "closed" || last.Status == "gone":
		why := "the vendor no longer has it"
		if last.Status == "closed" {
			why = "its directory was removed"
		}
		fmt.Fprintf(stderr, "aggy: the last %s session of %s cannot be resumed (%s); starting a new %s session. The parent's other sessions are kept.\n",
			last.Vendor, short, why, last.Vendor)
		return p, p.newChildOf(ctx, s, cfg, adapter.Vendor(last.Vendor), now)
	case last.VendorSessionID == "":
		// TODO(T6660): Codex's interactive id arrives from its hooks or
		// sessions folder (sessionIndex) once the hooks branch merges and
		// records it with SetChildSessionID.
		fmt.Fprintf(stderr, "aggy: the last %s session of %s has no recorded session id; starting a new %s session.\n",
			last.Vendor, short, last.Vendor)
		return p, p.newChildOf(ctx, s, cfg, adapter.Vendor(last.Vendor), now)
	}
	v, err := adapter.ParseVendor(last.Vendor)
	if err != nil {
		return p, err
	}
	// The id comes from the index: check it before it reaches argv.
	if err := adapter.ValidSessionID(v, last.VendorSessionID); err != nil {
		return p, fmt.Errorf("the last session of %s has an invalid id in the index: %w", short, err)
	}
	p.child, p.resume = *last, last.VendorSessionID
	p.child.Model = cfg.Vendors.For(v).Model
	return p, nil
}

// newChildOf plans a new interactive child of v (empty means the default
// vendor) in the parent's directory, which must still exist.
func (p *launchPlan) newChildOf(ctx context.Context, s *store.Store, cfg config.Config, v adapter.Vendor, now time.Time) error {
	if fi, err := safefs.Stat(p.parent.Dir); err != nil || !fi.IsDir() {
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("the directory of session %s, %s, no longer exists; run aggy new in another directory",
				shortID(p.parent.ID), termsafe.Line(p.parent.Dir))
		}
		return err
	}
	if v == "" {
		v = defaultVendor(ctx, s, cfg)
	}
	id, err := newUUID()
	if err != nil {
		return err
	}
	c := store.Child{ID: id, ParentID: p.parent.ID, Vendor: string(v), Model: cfg.Vendors.For(v).Model,
		Cwd: p.parent.Dir, Status: "open", CreatedAt: now, LastActive: now}
	if v != adapter.Codex {
		// Claude and Grok take --session-id <uuid>; Codex cannot be given
		// an id in advance (docs/adapters.md).
		if c.VendorSessionID, err = newUUID(); err != nil {
			return err
		}
	}
	p.child, p.newChild = c, true
	return nil
}

// defaultVendor is default_vendor, else the first vendor in claude,
// codex, grok order that aggy setup did not find signed out and whose CLI
// is on PATH, else claude.
func defaultVendor(ctx context.Context, s *store.Store, cfg config.Config) adapter.Vendor {
	if cfg.DefaultVendor != "" {
		return cfg.DefaultVendor
	}
	out := map[string]bool{}
	if rows, err := s.VendorStatuses(ctx); err == nil {
		for _, r := range rows {
			out[r.Vendor] = r.Connected == string(adapter.SignedOut)
		}
	}
	for _, v := range adapter.Vendors() {
		if out[string(v)] {
			continue
		}
		if _, err := probe.LookPath(vendorBin(v)); err == nil {
			return v
		}
	}
	return adapter.Claude
}

// commit records the plan in the index and takes the lease on the
// vendor session, so no other process drives it at the same time. A
// Codex child that has no id yet has nothing to lease (nil lease).
func (p *launchPlan) commit(ctx context.Context, s *store.Store, now time.Time) (*store.Lease, error) {
	if p.newParent {
		if err := s.InsertParent(ctx, p.parent); err != nil {
			return nil, err
		}
	}
	if p.newChild {
		if err := s.InsertChild(ctx, p.child); err != nil {
			return nil, err
		}
	}
	var lease *store.Lease
	if id := p.child.VendorSessionID; id != "" {
		var err error
		lease, err = s.AcquireLease(ctx, p.child.Vendor, id)
		var busy *store.BusyError
		if errors.As(err, &busy) {
			return nil, fmt.Errorf("the %s session of %s is already open in another aggy (pid %d); run aggy new to start another session here",
				p.child.Vendor, shortID(p.parent.ID), busy.PID)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := s.OnScreen(ctx, p.parent.ID, p.child.ID, p.child.Vendor, now); err != nil {
		if lease != nil {
			_ = lease.Release(ctx)
		}
		return nil, err
	}
	return lease, nil
}

// interactiveRequest is the adapter request for the plan's child.
func (p *launchPlan) interactiveRequest() adapter.InteractiveRequest {
	req := adapter.InteractiveRequest{
		Cwd:   p.child.Cwd,
		Model: p.child.Model,
		Env:   []string{"AGGY_VENDOR=" + p.child.Vendor},
	}
	if p.resume != "" {
		req.Resume = p.resume
	} else {
		req.SessionID = p.child.VendorSessionID
	}
	return req
}

// shortID is the first 8 characters of an id, as aggy sessions shows it.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
