package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
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
	// ParentID reopens that parent (an id or a unique prefix), or, when
	// no parent's id has that prefix, the parent of the one interactive
	// child whose id does, with that child on screen. Empty, and not New,
	// reopens the latest parent in the current directory, or starts one
	// there. With Vendor set (a switch), the parent's latest child of
	// that vendor goes on screen, or a new one.
	ParentID string
	// Direct runs the CLI on aggy's own terminal (aggy run --direct).
	Direct bool
	// NoPolicy starts the CLI without aggy's safety policy (--no-policy).
	NoPolicy bool
	// Brief writes the child's brief to .aggy/briefs/<child-id>.md and
	// tells the child to read it (--brief).
	Brief bool
	// For a switch (ParentID and Vendor set, T6665): Child puts that
	// child of the parent on screen instead of the vendor's latest,
	// NewChild starts another child of the vendor, and Model is the model
	// for this one launch, never written to config.
	Child    string
	NewChild bool
	Model    string
	// Tools are turned on for this one launch only (aggy run --tool),
	// never written to the settings; a repo's tools_off and a per-repo
	// tool's scope still win (switchboard.Board.Session). A switch from
	// this session does not carry them; a switch gets its own, from aggy
	// switch --tool, which the user confirmed with the switch.
	Tools []string
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
// recorded) go to stderr. A switch's one-launch model (T6665) is set on
// the child here, so a new child's index row records it (O56-7); it is
// checked again, as it may have come over the socket, and never written
// to config.
func plan(ctx context.Context, s *store.Store, cfg config.Config, cwd string, t target, now time.Time, stderr io.Writer) (launchPlan, error) {
	if err := adapter.ValidModel(t.Model); err != nil {
		return launchPlan{}, err
	}
	p, err := planChild(ctx, s, cfg, cwd, t, now, stderr)
	if err == nil && t.Model != "" {
		p.child.Model = t.Model
	}
	return p, err
}

// planChild is plan without the one-launch model.
func planChild(ctx context.Context, s *store.Store, cfg config.Config, cwd string, t target, now time.Time, stderr io.Writer) (launchPlan, error) {
	var p launchPlan
	pick := "" // a child t.ParentID named, put on screen instead of the latest
	switch {
	case t.New:
		p.newParent = true
	case t.ParentID != "":
		ps, err := s.ParentsByPrefix(ctx, t.ParentID)
		if err != nil {
			return p, err
		}
		par, kids, err := childByPrefix(ctx, s, t.ParentID)
		if err != nil {
			return p, err
		}
		// A prefix of both a parent and a child is ambiguous (G34-8).
		switch n := len(ps) + len(kids); {
		case n == 0:
			return p, fmt.Errorf("no session %s (aggy sessions and aggy session list them)", t.ParentID)
		case n > 1:
			return p, fmt.Errorf("%d sessions start with %s; give more of the id", n, t.ParentID)
		case len(ps) == 1:
			p.parent = ps[0]
		default:
			p.parent, pick = par, kids[0]
		}
	default:
		key, err := safefs.CanonicalPath(cwd)
		if err != nil {
			return p, err
		}
		par, err := s.LatestParentInDir(ctx, key, cwd)
		switch {
		case errors.Is(err, store.ErrNotFound):
			p.newParent = true
		case err != nil:
			return p, err
		case replaced(par):
			// A directory removed and made again (a re-clone) is another
			// project: its old sessions are not resumed by default.
			fmt.Fprintf(stderr, "aggy: this directory was replaced since session %s; starting a new session. aggy resume %s reopens the old one.\n",
				shortID(par.ID), shortID(par.ID))
			p.newParent = true
		default:
			p.parent = par
		}
	}
	if !p.newParent && replaced(p.parent) {
		fmt.Fprintf(stderr, "aggy: warning: the directory of session %s was replaced since it started (a re-clone?)\n", shortID(p.parent.ID))
	}
	if p.newParent {
		id, err := newUUID()
		if err != nil {
			return p, err
		}
		p.parent = store.Parent{ID: id, Dir: cwd, CreatedAt: now, LastActive: now}
		if p.parent.DirKey, err = safefs.CanonicalPath(cwd); err != nil {
			return p, err
		}
		p.parent.DirID = dirID(cwd)
		return p, p.newChildOf(ctx, s, cfg, t.Vendor, now)
	}

	if t.Vendor != "" && t.NewChild {
		return p, p.newChildOf(ctx, s, cfg, t.Vendor, now)
	}
	if t.Child != "" {
		pick = t.Child
	}
	files := &sessionFiles{}
	children, err := planRefresh(ctx, s, p.parent.ID, files)
	if err != nil {
		return p, err
	}
	// The interactive child last on screen: the most recently active one,
	// or the one picked by id.
	var last *store.Child
	for i := range children {
		if children[i].Kind == "" && (pick == "" || children[i].ID == pick) &&
			(t.Vendor == "" || children[i].Vendor == string(t.Vendor)) {
			last = &children[i]
			break
		}
	}
	short := shortID(p.parent.ID)
	switch {
	case last == nil && pick != "":
		// The child named went from the index while it was refreshed
		// (G34-9): starting another one instead would surprise.
		return p, fmt.Errorf("session %s is no longer recorded (aggy sessions lists the sessions)", shortID(pick))
	case last == nil:
		v, _ := adapter.ParseVendor(p.parent.LastVendor)
		if t.Vendor != "" {
			v = t.Vendor
		}
		return p, p.newChildOf(ctx, s, cfg, v, now)
	case last.Status == stateClosed || last.Status == stateGone:
		why := "the vendor has no saved session for it"
		if last.Status == stateClosed {
			why = "its directory was removed"
		}
		fmt.Fprintf(stderr, "aggy: the last %s session of %s cannot be resumed (%s); starting a new %s session. The parent's other sessions are kept.\n",
			last.Vendor, short, why, last.Vendor)
		return p, p.newChildOf(ctx, s, cfg, adapter.Vendor(last.Vendor), now)
	case last.VendorSessionID == "":
		// A Codex child whose id the session index never confirmed (its
		// hooks not installed or not approved, or no turn saved).
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
		eff, err := repoEffective(cfg, p.parent.Dir)
		if err != nil {
			return err
		}
		v = defaultVendor(ctx, s, eff.Config, eff.Allowed)
	}
	id, err := newUUID()
	if err != nil {
		return err
	}
	c := store.Child{ID: id, ParentID: p.parent.ID, Vendor: string(v), Model: cfg.Vendors.For(v).Model, TraceID: dlog.Trace(),
		Cwd: p.parent.Dir, Status: "open", CreatedAt: now, LastActive: now}
	// Where its session files are looked for later, whatever that
	// shell's CLAUDE_CONFIG_DIR, CODEX_HOME, or GROK_HOME.
	c.VendorHome, _ = (&sessionFiles{}).home(v)
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

// defaultVendor is default_vendor, else the first vendor of allowed (in
// claude, codex, grok order) that aggy setup did not find signed out and
// whose CLI is on PATH, else the first allowed one, else claude, which
// launch then refuses. cfg is narrowed by the repo (config.Merge), so
// default_vendor is empty when the repo does not allow it.
func defaultVendor(ctx context.Context, s *store.Store, cfg config.Config, allowed []adapter.Vendor) adapter.Vendor {
	if cfg.DefaultVendor != "" {
		return cfg.DefaultVendor
	}
	out := map[string]bool{}
	if rows, err := s.VendorStatuses(ctx); err == nil {
		for _, r := range rows {
			out[r.Vendor] = r.Connected == string(adapter.SignedOut)
		}
	}
	for _, v := range allowed {
		if out[string(v)] {
			continue
		}
		if _, err := probe.LookPath(vendorBin(v)); err == nil {
			return v
		}
	}
	if len(allowed) > 0 {
		return allowed[0]
	}
	return adapter.Claude
}

// allowedIn is repoEffective, and an error when that repo does not allow
// v: a session aggy starts or resumes never puts a repo on a vendor the
// repo leaves out.
func allowedIn(cfg config.Config, dir string, v adapter.Vendor) (config.Effective, error) {
	eff, err := repoEffective(cfg, dir)
	if err != nil {
		return eff, err
	}
	if !eff.Allows(v) {
		return eff, fmt.Errorf("this repo's allowed_vendors does not list %s", v)
	}
	return eff, nil
}

// repoEffective is cfg narrowed by the config of the repo dir is in (or
// of dir itself outside a repo). A repo config that cannot be read is an
// error: aggy never guesses that a repo allows a vendor.
func repoEffective(cfg config.Config, dir string) (config.Effective, error) {
	root, _ := repoRoot(dir)
	rc, err := config.LoadRepo(root)
	if err != nil {
		return config.Effective{}, fmt.Errorf("read repo config: %w", err)
	}
	return config.Merge(cfg, rc), nil
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
		Env:   childEnv(p.child.Vendor, p.child.Model),
	}
	if p.resume != "" {
		req.Resume = p.resume
	} else {
		req.SessionID = p.child.VendorSessionID
	}
	return req
}

// childEnv is AGGY_VENDOR, and AGGY_MODEL when a model is set, for a
// child: the linked skill and the attribution hook read them. AGGY_TRACE
// carries this run's diagnostic trace (docs/logging.md) to the session.
func childEnv(vendor, model string) []string {
	env := []string{"AGGY_VENDOR=" + vendor}
	if model != "" {
		env = append(env, "AGGY_MODEL="+model)
	}
	if t := dlog.Trace(); t != "" {
		env = append(env, diag.TraceEnv+"="+t)
	}
	return env
}

// dirID is dir's device and inode as "dev:ino", or "" when unknown.
func dirID(dir string) string {
	fi, err := safefs.Stat(dir)
	if err != nil {
		return ""
	}
	dev, ino, ok := safefs.FileID(fi)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", dev, ino)
}

// replaced reports whether p's directory is now another one than when p
// was made: same path, other device and inode.
func replaced(p store.Parent) bool {
	if p.DirID == "" {
		return false
	}
	now := dirID(p.Dir)
	return now != "" && now != p.DirID
}

// shortID is the first 8 characters of an id, as aggy sessions shows it.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// childIDs records the on-screen child's vendor session id once the
// session index knows it. The row keeps the first id (a /clear does not
// move it). A Codex child gets no id at launch, so its lease is taken
// here, when the id is confirmed, and held until the CLI exits.
type childIDs struct {
	s     *store.Store
	child store.Child

	mu    sync.Mutex
	lease *store.Lease
	done  bool
	id    string // a new Codex child's id, once recorded and leased
}

// current is the child's vendor session id: the one given at launch, or
// a new Codex child's once the index recorded it; "" until then. The
// supervisor counts only turn ends that name it (S-9).
func (c *childIDs) current() string {
	if c.child.VendorSessionID != "" {
		return c.child.VendorSessionID
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.id
}

// set is sessionIndex.onID. Errors are dropped: a failed write must not
// disturb the CLI on screen, and an id another child holds stays off
// this one. A new id is recorded and leased in one update (D23-11), so a
// concurrent aggy resume never finds it recorded and unleased.
func (c *childIDs) set(id string) {
	ctx := context.Background()
	if c.child.VendorSessionID != "" {
		_ = c.s.SetChildSessionID(ctx, c.child.ID, id) // the launch already leased it
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done || c.lease != nil {
		return
	}
	if l, err := c.s.SetChildSessionIDLeased(ctx, c.child.ID, id); err == nil {
		c.lease, c.id = l, id
	}
}

// release gives up a lease set took; later ids are not leased.
func (c *childIDs) release(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done = true
	if c.lease != nil {
		_ = c.lease.Release(ctx)
		c.lease = nil
	}
}

// planRefresh is refreshChildren; a test swaps it to drop a child.
var planRefresh = refreshChildren

// childByPrefix returns the ids of the interactive children whose id
// starts with prefix, and the parent of the last one.
func childByPrefix(ctx context.Context, s *store.Store, prefix string) (store.Parent, []string, error) {
	ps, err := s.Parents(ctx)
	if err != nil {
		return store.Parent{}, nil, err
	}
	var par store.Parent
	var found []string
	for _, p := range ps {
		cs, err := s.Children(ctx, p.ID)
		if err != nil {
			return store.Parent{}, nil, err
		}
		for _, c := range cs {
			if c.Kind == "" && strings.HasPrefix(c.ID, prefix) {
				par, found = p, append(found, c.ID)
			}
		}
	}
	return par, found, nil
}
