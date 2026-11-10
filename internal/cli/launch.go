package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/opencode"
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
	// Continue reopens the session last active in the current directory
	// that can still be reopened (of Vendor, when set), and is an error
	// when there is none (aggy --continue, aggy <vendor> --continue).
	Continue bool
	// ParentID reopens that parent (an id or a unique prefix), or, when
	// no parent's id has that prefix, the parent of the one child, a borrow
	// child included, whose id does, with that child on screen. Empty, and not New,
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
	// Extra are the CLI's own flags from aggy <vendor>, for this launch.
	// SafeExtra is Extra without Loosens, the flags --no-policy let through:
	// a resume of the same session keeps SafeExtra, since it drops
	// --no-policy (S-6), and a switch keeps neither.
	Extra, SafeExtra, Loosens []string
	// Flags are the CLI's own words as given, not yet sorted: the launch
	// sorts them into Extra and the rest once it knows the vendor
	// (parseLaunchFlags). nil means Extra is already sorted.
	Flags []string
	// Prompt says Extra holds a prompt, so the CLI starts with a turn
	// running. A later launch of the session never sends it again
	// (SafeExtra leaves it out).
	Prompt bool
	// FlagNotes are the launch lines of the flags given (PassFlags.Noted).
	FlagNotes []string
	// Dirs are the folders the flags name, for gateDirFlags.
	Dirs []adapter.DirFlag
	// SameChild says Child is the child that was on screen, reopened for a
	// tool change, so it may be a borrow's. A switch's Child never may.
	SameChild bool
	// Preview: nothing launches (--brief --show), so a note says what a launch
	// would do.
	Preview bool
	// Native reopens a session found in the vendor's own store for the
	// current directory (T6965): the child that already holds it, or, on first
	// use, a new parent and child adopted for it. A picked session and -c's
	// newest are native when aggy has not recorded them.
	Native *nativeSession
	// ResumeName reopens the session the CLI finds by this name, passed as its
	// --resume <name> (Claude and Grok, T6965). The new child has no id until
	// the CLI exits, when the one session file of this directory that changed
	// since the launch names it (adoptNamed).
	ResumeName string
	// SwitchFrom is the vendor this launch was switched from, and
	// SwitchReason why, for the switch row of aggy log (T6925); "" for a
	// launch that is not a switch's, which writes a screen row instead.
	SwitchFrom   adapter.Vendor
	SwitchReason string
}

// starting is what a note says a launch does.
func (t target) starting() string {
	if t.Preview {
		return "a launch would start"
	}
	return "starting"
}

// fromSwitch reports whether t is a switch's launch, which has a CLI to go back
// to: a vendor in a parent, and not the same child again.
func (t target) fromSwitch() bool { return t.ParentID != "" && t.Vendor != "" && !t.SameChild }

// Errors launchCode gives their own reason code.
var (
	// errNoSession: no session has the id, or none here can be reopened.
	errNoSession = errors.New("no session")
	// errAmbiguousID: more than one session has the prefix.
	errAmbiguousID = errors.New("give more of the id")
	// errNotAllowed: the repo's allowed_vendors leaves the vendor out.
	errNotAllowed = errors.New("this repo's allowed_vendors does not list")
	// errHarnessOff: the person turned the vendor off in aggy's config.
	errHarnessOff = errors.New("this harness is turned off")
	// errBorrowNoScreen: research and news children never go on screen.
	errBorrowNoScreen = errors.New("does not go on screen: by rule it never gets file-reading or MCP tools, an on-screen session has them, and its transcript may hold text from the web; aggy inbox shows its report")
)

// launchCode is err's reason code, def unless it is one of those above.
func launchCode(err error, def diag.Code) diag.Code {
	switch {
	case errors.Is(err, errNoSession):
		return diag.CodeNotFound
	case errors.Is(err, errAmbiguousID):
		return diag.CodeAmbiguousID
	case errors.Is(err, errNotAllowed), errors.Is(err, errHarnessOff):
		return diag.CodeNotAllowed
	case errors.Is(err, errBorrowNoScreen):
		return diag.CodeBorrowNoScreen
	}
	return def
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
	// remakeCwd says the child's working dir is a finished borrow's empty
	// temp dir, which launchOnce makes again for the session.
	remakeCwd bool
	// preview: nothing launches (target.Preview), so nothing is written.
	preview bool
	// adopted: the child is a native session recorded for the first time,
	// which commit logs (T6965).
	adopted bool
	// resumeName is target.ResumeName, for the CLI's own --resume.
	resumeName string
}

// plan decides the parent and child a launch opens. It writes only what
// refreshChildren finds on disk, and for a preview nothing; the launch itself is
// recorded by commit.
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
	p.preview = t.Preview
	return p, err
}

// planChild is plan without the one-launch model.
func planChild(ctx context.Context, s *store.Store, cfg config.Config, cwd string, t target, now time.Time, stderr io.Writer) (launchPlan, error) {
	var p launchPlan
	pick := "" // a child t.ParentID named, put on screen instead of the latest
	switch {
	case t.New:
		p.newParent = true
	case t.ResumeName != "":
		return p, p.byName(ctx, s, cfg, cwd, t, now)
	case t.Native != nil:
		par, id, err := nativeOwner(ctx, s, *t.Native)
		if err != nil {
			return p, err
		}
		if id == "" {
			return p, p.adopt(cfg, cwd, *t.Native, now)
		}
		p.parent, pick = par, id
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
			return p, fmt.Errorf("%w %s (aggy sessions and aggy session list them)", errNoSession, t.ParentID)
		case n > 1:
			return p, fmt.Errorf("%d sessions start with %s; %w", n, t.ParentID, errAmbiguousID)
		case len(ps) == 1:
			p.parent = ps[0]
		default:
			p.parent, pick = par, kids[0]
		}
	case t.Continue:
		par, last, err := latestResumable(ctx, s, cwd, t.Vendor, t.Preview)
		// T6965: a session the vendor CLI started here by itself counts too,
		// when it is newer than the one aggy recorded.
		if ns, ok := newestNative(ctx, s, cfg, cwd, t.Vendor, &sessionFiles{readOnly: t.Preview}); ok &&
			(err != nil && errors.Is(err, errNoSession) || err == nil && ns.Modified.After(last.LastActive)) {
			return p, p.adopt(cfg, cwd, ns, now)
		}
		if err != nil {
			return p, err
		}
		p.parent, pick = par, last.ID
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
			fmt.Fprintf(stderr, "aggy: this directory was replaced since session %s; %s a new session. aggy resume %s reopens the old one.\n",
				shortID(par.ID), t.starting(), shortID(par.ID))
			p.newParent = true
		default:
			if p.parent, err = latestStarted(ctx, s, key, cwd, par, t, stderr); err != nil {
				return p, err
			}
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
	// Only an id on the command line may name a borrow child: a switch
	// picks among interactive children, and its id comes over the socket.
	borrowOK := pick != "" && t.Child == "" || t.SameChild
	if t.Child != "" {
		pick = t.Child
	}
	files := &sessionFiles{readOnly: t.Preview}
	children, err := planRefresh(ctx, s, p.parent.ID, files)
	if err != nil {
		return p, err
	}
	// The interactive child last on screen: the most recently active one that
	// can be reopened, or the one picked by id. Any other counts only when no
	// other does (T6899).
	var last, unstarted, noID *store.Child
	for i := range children {
		if (children[i].Kind == "" || borrowOK && children[i].ID == pick) && (pick == "" || children[i].ID == pick) &&
			(t.Vendor == "" || children[i].Vendor == string(t.Vendor)) {
			if pick == "" && !reopenable(children[i]) {
				if unstarted == nil {
					unstarted = &children[i]
				}
				if noID == nil && children[i].VendorSessionID == "" && !neverStarted(children[i]) {
					noID = &children[i]
				}
				continue
			}
			last = &children[i]
			break
		}
	}
	if last == nil {
		last = unstarted
	} else if noID != nil && last.VendorSessionID != "" {
		noteHidden(stderr, *noID, *last, t)
	}
	if last != nil && last.Kind != "" {
		return p, p.borrowChild(cfg, t, *last, files)
	}
	short := shortID(p.parent.ID)
	switch {
	case last == nil && pick != "" && t.Child == "" && t.Vendor != "":
		// aggy claude -r <id of a Codex child>.
		return p, fmt.Errorf("session %s is not a %s session", shortID(pick), t.Vendor)
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
		fmt.Fprintf(stderr, "aggy: the last %s session of %s cannot be resumed (%s); %s a new %s session. The parent's other sessions are kept.\n",
			last.Vendor, short, why, t.starting(), last.Vendor)
		return p, p.newChildOf(ctx, s, cfg, adapter.Vendor(last.Vendor), now)
	case last.VendorSessionID == "":
		// A Codex child whose id the session index never confirmed (its
		// hooks not installed or not approved, or no turn saved).
		fmt.Fprintf(stderr, "aggy: the last %s session of %s has no recorded session id; %s a new %s session.\n",
			last.Vendor, short, t.starting(), last.Vendor)
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

// neverStarted reports whether c, as refreshChildren left it, is an interactive
// child with no session file now and none ever seen: its CLI closed at a launch
// dialog, or before the first message. There is nothing to reopen.
func neverStarted(c store.Child) bool {
	return c.Kind == "" && c.Status == stateGone && !c.FileSeen
}

// reopenable reports whether a resume could put c back on screen. A child with
// no session id cannot: its turns, if it had any, are out of aggy's reach, which
// is why brief's reading of neverStarted stays apart from this.
func reopenable(c store.Child) bool {
	return !neverStarted(c) && (c.Kind != "" || c.VendorSessionID != "")
}

// noteHidden says that a resume opens opened because the newer session hidden
// cannot be reopened: it may well have had turns.
func noteHidden(stderr io.Writer, hidden, opened store.Child, t target) {
	verb := "opening"
	if t.Preview {
		verb = "a launch would open"
	}
	fmt.Fprintf(stderr, "aggy: the newer %s session here cannot be reopened, since aggy never learned its id; %s the %s session %s.\n",
		hidden.Vendor, verb, opened.Vendor, shortID(opened.ID))
}

// latestStarted is the parent aggy resume reopens in dir: the most recently
// active one with a child that can be reopened, else latest. A parent left only
// with a child that never started, or with no id, must not hide the session
// before it (T6899).
func latestStarted(ctx context.Context, s *store.Store, key, dir string, latest store.Parent, t target, stderr io.Writer) (store.Parent, error) {
	ps, err := s.ParentsInDir(ctx, key, dir)
	if err != nil {
		return latest, err
	}
	var hidden *store.Child
	for _, par := range ps {
		if replaced(par) {
			continue
		}
		children, err := planRefresh(ctx, s, par.ID, &sessionFiles{readOnly: t.Preview})
		if err != nil {
			return latest, err
		}
		for i, c := range children {
			if c.Kind == "" && reopenable(c) {
				// The chosen parent's own children say theirs in planChild.
				if hidden != nil {
					noteHidden(stderr, *hidden, c, t)
				}
				return par, nil
			}
			if c.Kind == "" && hidden == nil && c.VendorSessionID == "" && !neverStarted(c) {
				hidden = &children[i]
			}
		}
	}
	return latest, nil
}

// borrowChild plans a finished borrow's child on screen (aggy resume <child
// id>), or says why it cannot be: never a new child in its place. Research
// and news are refused (errBorrowNoScreen). The rest resume in their own
// directory, since Claude and Grok keep sessions per directory: review's repo
// or edit's worktree, which must still be there, or the empty temp dir of
// image and video, which ended with the borrow and is made new for the session
// (remakeCwd), so a dir already there is refused, never used. It is the
// person's session now: the borrow's lockdown flags do not apply, and launchOnce
// says so.
func (p *launchPlan) borrowChild(cfg config.Config, t target, c store.Child, files *sessionFiles) error {
	what := c.Kind + " borrow " + shortID(c.ID)
	if k := adapter.Kind(c.Kind); k == adapter.Research || k == adapter.News {
		return fmt.Errorf("%s %w", what, errBorrowNoScreen)
	}
	if t.Brief {
		return fmt.Errorf("%s takes no brief: it is its own session, and other CLIs' turns are not part of it", what)
	}
	if c.Status != stateClosed {
		return fmt.Errorf("%s is still running; aggy inbox shows its report when it ends", what)
	}
	if c.VendorSessionID == "" {
		return fmt.Errorf("%s has no recorded session id: its CLI reported none", what)
	}
	v, err := adapter.ParseVendor(c.Vendor)
	if err != nil {
		return err
	}
	if err := adapter.ValidSessionID(v, c.VendorSessionID); err != nil {
		return fmt.Errorf("%s has an invalid id in the index: %w", what, err)
	}
	switch adapter.Kind(c.Kind) {
	case adapter.Image, adapter.Video:
		if !borrowTempDir(c) {
			return fmt.Errorf("%s cannot be resumed: %s is not a borrow's temp dir", what, termsafe.Line(c.Cwd))
		}
		p.remakeCwd = true
	default:
		fi, err := safefs.Stat(c.Cwd)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("%s cannot be resumed: its directory, %s, was removed", what, termsafe.Line(c.Cwd))
		case err != nil:
			return err
		case !fi.IsDir():
			return fmt.Errorf("%s cannot be resumed: %s is not a directory", what, termsafe.Line(c.Cwd))
		}
	}
	if _, err := files.LastChanged(v, c.VendorSessionID, c.VendorHome, c.Cwd, c.CreatedAt); errors.Is(err, errNoSessionFile) || errors.Is(err, errNotInScan) {
		return fmt.Errorf("%s cannot be resumed: the vendor has no saved session for it", what)
	}
	p.child, p.resume = c, c.VendorSessionID
	p.child.Model = cfg.Vendors.For(v).Model
	return nil
}

// borrowNote says at launch what a borrow's child on screen has become.
func borrowNote(pl launchPlan) string {
	note := "this " + pl.child.Kind + " borrow's session is on screen now: the borrow's lockdown no longer applies, and what it may do is up to the CLI's own permission settings"
	if pl.remakeCwd {
		note += "; its temp directory is made new, and files in it are removed when the session ends"
	}
	if adapter.Kind(pl.child.Kind) == adapter.Edit {
		note += "; aggy accept refuses a worktree changed since the borrow ended, so review and commit such changes by hand, or aggy reject them; .aggy/memory.md is not loaded from an unreviewed worktree, but the CLI still loads its own project files, which the child may have written: read CLAUDE.md, AGENTS.md, and .claude/settings.json there (its hooks run code) before you go on; which of them load for this CLI in a worktree is unverified"
	}
	return note
}

// borrowTempDir reports whether c's working dir is the work/ of an
// aggy-borrow-* temp dir, as internal/borrow makes for every kind but review
// and edit.
func borrowTempDir(c store.Child) bool {
	return filepath.IsAbs(c.Cwd) && filepath.Base(c.Cwd) == "work" && strings.HasPrefix(filepath.Base(filepath.Dir(c.Cwd)), "aggy-borrow-")
}

// remakeBorrowDir makes a finished borrow's temp dir again, for a session
// that resumes in it, and returns what removes it. Each level must be new:
// one already there may not be empty or the user's (a temp folder can be
// shared), and a child must never start among files aggy did not vouch for.
func remakeBorrowDir(cwd string) (func(), error) {
	root := filepath.Dir(cwd)
	if err := safefs.Mkdir(root, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%s is already there (another aggy has this borrow's session open, or one crashed); remove it to resume the session", termsafe.Line(root))
		}
		return nil, err
	}
	remove := func() { _ = safefs.RemoveAll(root) }
	if err := safefs.Mkdir(cwd, 0o700); err != nil {
		remove()
		return nil, err
	}
	return remove, nil
}

// latestResumable finds the session --continue reopens: the interactive
// child last active in dir (of v, when set) that can still be reopened, in
// whichever of dir's parents holds it, since every bare aggy starts a
// parent of its own and one quit before its first message saves nothing.
func latestResumable(ctx context.Context, s *store.Store, dir string, v adapter.Vendor, readOnly bool) (store.Parent, store.Child, error) {
	key, err := safefs.CanonicalPath(dir)
	if err != nil {
		return store.Parent{}, store.Child{}, err
	}
	ps, err := s.ParentsInDir(ctx, key, dir)
	if err != nil {
		return store.Parent{}, store.Child{}, err
	}
	for _, par := range ps {
		if replaced(par) {
			continue
		}
		children, err := planRefresh(ctx, s, par.ID, &sessionFiles{readOnly: readOnly})
		if err != nil {
			return store.Parent{}, store.Child{}, err
		}
		for _, c := range children {
			if c.Kind == "" && (v == "" || c.Vendor == string(v)) && c.VendorSessionID != "" &&
				c.Status != stateClosed && c.Status != stateGone {
				return par, c, nil
			}
		}
	}
	what := "session"
	if v != "" {
		what = string(v) + " session"
	}
	return store.Parent{}, store.Child{}, fmt.Errorf("%w to continue: no %s here can be reopened (aggy sessions lists them; aggy starts a new one)", errNoSession, what)
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
		// an id in advance (docs/adapters.md). OpenCode's --session takes a
		// new id of its own shape.
		if c.VendorSessionID, err = newUUID(); err != nil {
			return err
		}
		if v == adapter.OpenCode {
			c.VendorSessionID = opencode.NewSessionID(c.VendorSessionID)
		}
	}
	p.child, p.newChild = c, true
	return nil
}

// defaultVendor is default_vendor, else the first vendor of allowed (in
// claude, codex, grok order) that aggy setup did not find signed out and
// whose CLI is on PATH, else the first allowed one, else claude, which
// launch then refuses. A nil s counts none signed out. cfg is narrowed by the repo (config.Merge), so
// default_vendor is empty when the repo does not allow it.
func defaultVendor(ctx context.Context, s *store.Store, cfg config.Config, allowed []adapter.Vendor) adapter.Vendor {
	if cfg.DefaultVendor != "" {
		return cfg.DefaultVendor
	}
	out := map[string]bool{}
	if s != nil {
		if rows, err := s.VendorStatuses(ctx); err == nil {
			for _, r := range rows {
				out[r.Vendor] = r.Connected == string(adapter.SignedOut)
			}
		}
	}
	for _, v := range allowed {
		// An open harness opens only when the person named it.
		if out[string(v)] || v == adapter.OpenCode {
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
	if cfg.Vendors.For(v).Off {
		return eff, fmt.Errorf("%w: aggy config set harness.%s on", errHarnessOff, v)
	}
	if !eff.Allows(v) {
		return eff, fmt.Errorf("%w %s", errNotAllowed, v)
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
	if p.adopted {
		// The index's record that aggy took this session on (T6965); a failed
		// write loses only the line.
		_ = s.AppendEvent(ctx, store.Event{At: now, ParentID: p.parent.ID, ChildID: p.child.ID, Type: store.EventSession,
			Detail: map[string]any{"vendor": p.child.Vendor, "session": p.child.VendorSessionID, "via": "adopted"}})
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
	switch {
	case p.resume != "":
		req.Resume = p.resume
	case p.resumeName != "":
		req.ResumeName = p.resumeName
	default:
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
// session index knows it. A Codex child gets no id at launch, so its lease is
// taken here, when the id is confirmed, and held until the CLI exits. When the
// CLI moves to another session (/clear, /resume; the supervisor follows it on
// a hook from where the CLI's own hooks run, session.followSession), follow
// makes that the id, and settle records it on the row as the CLI exits, once a
// session file of this child's directory backs it: a resume starts a CLI on that
// id, so it takes more than a hook's word, which a tool in the CLI's tree can
// forge, and a session of another repo must not become this child's. That
// guards against a mistake, not against a tool that can write to the vendor's
// folder: it can as well rewrite the session aggy would resume. No file (a /clear
// and no message after it) leaves the row on the session that has turns, as
// claude --continue does.
type childIDs struct {
	s     *store.Store
	child store.Child

	mu    sync.Mutex
	lease *store.Lease
	done  bool
	id    string // a new Codex child's id, once recorded and leased
	moved string // the session the CLI moved to, once followed
	via   string // the evidence for it
}

// current is the child's vendor session id: the one given at launch, or
// a new Codex child's once the index recorded it; "" until then. The
// supervisor counts only turn ends that name it (S-9).
func (c *childIDs) current() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.moved != "" {
		return c.moved
	}
	if c.child.VendorSessionID != "" {
		return c.child.VendorSessionID
	}
	return c.id
}

// follow is supervisor.Options.OnFollow.
func (c *childIDs) follow(id, via string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.moved, c.via = id, via
}

// didMove reports whether the CLI moved to another session while it ran.
func (c *childIDs) didMove() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.moved != ""
}

// settle records the session the CLI moved to on the child's row, if its session
// file backs it: found where this child's directory keeps its sessions, and for
// Codex the interactive TUI's own rollout, of this directory (rolloutInDir), so a
// nested codex exec's never is. An id another child holds stays off this one.
// Errors are dropped: the row stays as it was.
func (c *childIDs) settle(ctx context.Context, record func(store.Event) error, files *sessionFiles) {
	c.mu.Lock()
	from, to, via := c.child.VendorSessionID, c.moved, c.via
	if from == "" {
		from = c.id
	}
	c.mu.Unlock()
	v, err := adapter.ParseVendor(c.child.Vendor)
	if to == "" || from == "" || err != nil {
		return
	}
	if !files.inDir(v, to, c.child.VendorHome, c.child.Cwd, c.child.CreatedAt) {
		return
	}
	if moved, err := c.s.MoveChildSession(ctx, c.child.ID, from, to); err != nil || !moved {
		return
	}
	if record != nil {
		_ = record(store.Event{At: time.Now(), ParentID: c.child.ParentID, Type: store.EventSession,
			Detail: map[string]any{"vendor": string(v), "session": to, "via": "followed " + via}})
	}
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

// childByPrefix returns the ids of the children, borrow children included,
// whose id starts with prefix, and the parent of the last one.
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
			if strings.HasPrefix(c.ID, prefix) {
				par, found = p, append(found, c.ID)
			}
		}
	}
	return par, found, nil
}
