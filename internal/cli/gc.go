package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const gcUsage = `Usage: aggy gc [--dry-run] [--yes]

Removes what aggy made and no longer needs: edit worktrees accepted
gc.accepted_worktree_days ago (unless changed or committed on since),
and pending ones left untouched for gc.pending_worktree_days; closed
borrow children in aggy's index after gc.closed_child_days (never the
vendor's own session files); inbox reports older than gc.inbox_days;
and expired research cache entries. The days (30 by default) are set in
your own config.toml only.
It lists stale leases whose pid is in use again, but kills nothing it
cannot prove it started.

It shows what it would remove and how many bytes, then asks. Without a
terminal it removes nothing unless given --yes.

  --dry-run  show what it would remove, and remove nothing
  --yes      remove without asking
`

// gcRetention is how long aggy gc keeps what it may remove.
type gcRetention struct {
	pendingWorktree, acceptedWorktree, closedChild, inbox time.Duration
}

func retention(c config.GC) gcRetention {
	day := 24 * time.Hour
	return gcRetention{
		pendingWorktree:  time.Duration(c.PendingWorktreeDays) * day,
		acceptedWorktree: time.Duration(c.AcceptedWorktreeDays) * day,
		closedChild:      time.Duration(c.ClosedChildDays) * day,
		inbox:            time.Duration(c.InboxDays) * day,
	}
}

// gcItem is one thing aggy gc would remove.
type gcItem struct {
	kind, what string
	bytes      int64
	remove     func(context.Context) error
	ids        diag.Rec // the diagnostic log's ids for it
}

// gcPlan is what a gc run would remove, and what it leaves and why.
type gcPlan struct {
	items   []gcItem
	skipped []string
}

func (p *gcPlan) skip(format string, a ...any) {
	p.skipped = append(p.skipped, fmt.Sprintf(format, a...))
}

func runGC(args []string, stdout, stderr io.Writer) int {
	return gcWith(args, defaultBorrowEnv(), os.Stdin, isTerminal(os.Stdin), stdout, stderr)
}

func gcWith(args []string, env borrowEnv, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("gc", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	yes := fl.Bool("yes", false, "")
	dry := fl.Bool("dry-run", false, "")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, gcUsage)
			return 0
		}
		refuse(context.Background(), stderr, "gc", diag.EvGCRefused, diag.CodeUsage, err.Error(), errRec(diag.Rec{}, err))
		fmt.Fprintf(stderr, "\n%s", gcUsage)
		return 2
	}
	if fl.NArg() > 0 {
		refuse(context.Background(), stderr, "gc", diag.EvGCRefused, diag.CodeUsage, "unexpected arguments", diag.Rec{})
		fmt.Fprintf(stderr, "\n%s", gcUsage)
		return 2
	}
	// A dry run has no side effects: not even a diagnostic record.
	log := dlog
	if *dry {
		log = nil
	}
	fail := func(code diag.Code, err error) int {
		refuseWith(log, context.Background(), stderr, "gc", diag.EvGCRefused, code, err.Error(), errRec(diag.Rec{}, err))
		return 1
	}
	// A borrow child never cleans up after its parent.
	if env.getenv(borrow.Delegated) != "" {
		return fail(diag.CodeOneHop, borrow.ErrDelegated)
	}
	stateDir, err := home.Dir()
	if err != nil {
		return fail(diag.CodeStore, err)
	}
	cfg, err := config.LoadDefault()
	if err != nil {
		return fail(diag.CodeConfig, err)
	}
	path, err := store.DefaultPath()
	if err != nil {
		return fail(diag.CodeStore, err)
	}
	if _, err := safefs.Stat(path); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(stdout, "Nothing to remove: aggy has no index yet.")
		return 0
	}
	ctx := context.Background()
	// A dry run never creates, migrates, or writes the index.
	open := store.OpenDefault
	if *dry {
		open = func(ctx context.Context) (*store.Store, error) { return store.OpenReadOnly(ctx, path) }
	}
	s, err := open(ctx)
	if err != nil {
		return fail(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	g := &gitWorktrees{env: env, stateDir: stateDir, out: stderr}
	plan, err := planGC(ctx, s, g, stateDir, retention(cfg.GC), env.now())
	if err != nil {
		return fail(diag.CodeGCFailed, store.ReadError(err))
	}
	// diag.db is aggy's own log: one it cannot read never stops the rest
	// of gc.
	if err := planDiag(ctx, &plan, cfg.Log, env.now()); err != nil {
		fmt.Fprintf(stderr, "aggy gc: warning: skipping the diagnostic log (diag.db), which cannot be read: %s\n", termsafe.Field(err.Error()))
	}
	showPlan(stdout, plan)
	if len(plan.items) == 0 {
		return 0
	}
	switch {
	case *dry:
		fmt.Fprintln(stdout, "Dry run: nothing removed.")
		return 0
	case *yes:
	case !tty:
		fmt.Fprintln(stdout, "Nothing removed: run again with --yes, or in a terminal, to remove these.")
		return 1
	default:
		fmt.Fprint(stdout, "Remove these? [y/N] ")
		line, _ := bufio.NewReader(io.LimitReader(in, maxAnswer)).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "Nothing removed.")
			return 1
		}
	}
	code := 0
	for _, it := range plan.items {
		rec := it.ids
		rec.Attrs = map[string]any{"kind": it.kind}
		if err := it.remove(ctx); err != nil {
			refuse(ctx, stderr, "gc", diag.EvGCRemoveFailed, diag.CodeGCRemoveFailed,
				fmt.Sprintf("not removed: %s %s: %s", it.kind, it.what, err), errRec(rec, err))
			code = 1
			continue
		}
		rec.Event, rec.Msg = diag.EvGCRemoved, it.what
		dlog.Log(ctx, rec)
		fmt.Fprintf(stdout, "Removed %s %s\n", it.kind, termsafe.Field(it.what))
	}
	return code
}

// showPlan prints what a gc would remove, with its size, and what it
// leaves.
func showPlan(w io.Writer, p gcPlan) {
	var total int64
	for _, it := range p.items {
		total += it.bytes
	}
	if len(p.items) == 0 {
		fmt.Fprintln(w, "Nothing to remove.")
	} else {
		fmt.Fprintf(w, "aggy gc would remove %d item(s), %s:\n", len(p.items), humanBytes(total))
		for _, it := range p.items {
			size := ""
			if it.bytes > 0 {
				size = "  " + humanBytes(it.bytes)
			}
			fmt.Fprintf(w, "  %-9s %s%s\n", it.kind, termsafe.Field(it.what), size)
		}
	}
	if len(p.skipped) > 0 {
		fmt.Fprintln(w, "Left alone:")
		for _, s := range p.skipped {
			fmt.Fprintf(w, "  %s\n", termsafe.Field(s))
		}
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// planGC works out what to remove. It changes nothing: every removal is
// a closure that checks its target again when it runs.
func planGC(ctx context.Context, s *store.Store, g *gitWorktrees, stateDir string, r gcRetention, now time.Time) (gcPlan, error) {
	var p gcPlan
	if err := planWorktrees(ctx, &p, s, g, r, now); err != nil {
		return p, err
	}
	children, err := s.GCChildren(ctx, now.Add(-r.closedChild))
	if err != nil {
		return p, err
	}
	for _, c := range children {
		if c.Held {
			p.skip("child %s: its lease is held by a running aggy", c.ID)
			continue
		}
		c := c
		p.items = append(p.items, gcItem{
			kind: "child", ids: diag.Rec{Child: c.ID, Vendor: c.Vendor, Trace: c.TraceID},
			what: fmt.Sprintf("%s (%s %s borrow, last active %s; index row only)", c.ID, c.Vendor, c.Kind, c.LastActive.Format("2006-01-02")),
			remove: func(ctx context.Context) error {
				ok, err := s.PruneChild(ctx, c)
				if err == nil && !ok {
					err = errors.New("it changed since (a lease taken, or a worktree kept)")
				}
				return err
			},
		})
	}
	stray, err := s.StrayLeases(ctx)
	if err != nil {
		return p, err
	}
	for _, l := range stray {
		// A lease records aggy's own pid and no start time: a live process
		// with that pid cannot be shown to be the holder, so it is listed,
		// never killed.
		p.skip("pid %d (stale lease of %s %s): its holder is gone and the pid is in use, likely by another process; not killed", l.PID, l.Table, l.ID)
	}
	if err := planInbox(ctx, &p, s, stateDir, r, now); err != nil {
		return p, err
	}
	return p, planCache(&p, stateDir, now)
}

// planWorktrees adds the edit worktrees to remove: accepted ones, a
// rejected one whose removal was cut short, pending ones whose borrow
// ended over r.pendingWorktree ago, and leftovers (a removal claimed in
// the store whose directory, branch, or git admin entry is still there,
// after a crash). Each must be one aggy made (gitWorktrees.owned), not a
// symlink, with its borrow finished and its lease free, and is checked
// again before removal.
func planWorktrees(ctx context.Context, p *gcPlan, s *store.Store, g *gitWorktrees, r gcRetention, now time.Time) error {
	wts, err := s.GCWorktrees(ctx)
	if err != nil {
		return err
	}
	for _, w := range wts {
		wt, id := w.Worktree, w.BorrowID
		leftover := !wt.Removed.IsZero()
		fi, err := safefs.Lstat(wt.Path)
		gone := errors.Is(err, fs.ErrNotExist)
		if leftover && gone {
			// The directory went; its branch or admin entry may not have.
			left, lerr := g.leftBehind(ctx, wt)
			if lerr != nil {
				p.skip("worktree of borrow %s: %v", id, lerr)
				continue
			}
			if !left {
				continue // removed
			}
		}
		switch {
		case w.Running:
			p.skip("worktree of borrow %s: the borrow is still running", id)
			continue
		case w.Held:
			p.skip("worktree of borrow %s: its lease is held by a running aggy", id)
			continue
		case leftover:
		case wt.State == store.WorktreePending && now.Sub(w.Ended) < r.pendingWorktree:
			continue // waiting for accept or reject
		case wt.State == store.WorktreeAccepted && now.Sub(acceptedAt(w)) < r.acceptedWorktree:
			continue // kept for aggy pr
		case wt.State != store.WorktreePending && wt.State != store.WorktreeAccepted && wt.State != store.WorktreeRejected:
			continue
		}
		if err := g.owned(wt); err != nil {
			p.skip("worktree of borrow %s: %v", id, err)
			continue
		}
		var size int64
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			p.skip("worktree of borrow %s: %v", id, err)
			continue
		case !fi.IsDir():
			p.skip("worktree of borrow %s: refusing: %s is not a directory (a symlink?)", id, wt.Path)
			continue
		default:
			size = dirSize(wt.Path)
		}
		accepted := !leftover && wt.State == store.WorktreeAccepted
		if accepted {
			if why, err := g.keptAccepted(ctx, wt); err != nil || why != "" {
				if err != nil {
					why = err.Error()
				}
				p.skip("worktree of borrow %s: kept: %s", id, why)
				continue
			}
		}
		// A branch with commits that hold more than the recorded edit is
		// kept: a leftover loses only its directory and admin entry.
		var commits int
		keepBranch := false
		if accepted || leftover {
			why, n, err := g.branchKept(ctx, wt)
			if err != nil {
				p.skip("worktree of borrow %s: %v", id, err)
				continue
			}
			commits = n
			if why != "" {
				p.skip("worktree of borrow %s: kept: %s", id, why)
				keepBranch = true
				if gone {
					if admin, err := g.aggyAdmin(ctx, wt); err != nil || admin == "" {
						if err != nil {
							p.skip("worktree of borrow %s: %v", id, err)
						}
						continue // nothing else is left
					}
				}
			}
		}
		what := fmt.Sprintf("%s (%s, branch %s)", wt.Path, wt.State, wt.Branch)
		switch {
		case keepBranch:
			what = fmt.Sprintf("%s (left by a removal cut short; its branch %s is kept)", wt.Path, wt.Branch)
		case leftover:
			what = fmt.Sprintf("%s (left by a removal cut short, branch %s)", wt.Path, wt.Branch)
		}
		if commits > 0 && !keepBranch {
			what += fmt.Sprintf(", deletes %d commit(s) on the branch that hold only the recorded edit", commits)
		}
		ids := diag.Rec{Borrow: id, Worktree: filepath.Base(wt.Path), Trace: w.TraceID}
		p.items = append(p.items, gcItem{kind: "worktree", what: what, bytes: size, ids: ids, remove: func(ctx context.Context) error {
			if err := unchanged(ctx, s, w); err != nil {
				return err
			}
			if leftover {
				// The removal is claimed already. The branch is checked
				// again: it may have been committed on since the plan.
				why, _, err := g.branchKept(ctx, wt)
				if err != nil {
					return err
				}
				return g.remove(ctx, wt, why == "")
			}
			if accepted {
				if why, err := g.keptAccepted(ctx, wt); err != nil || why != "" {
					if err == nil {
						err = errors.New("skipped: " + why)
					}
					return err
				}
			}
			return discardEdit(ctx, s, g, id, wt, now)
		}})
	}
	return nil
}

// planDiag adds the diagnostic log's records past [log] retention_days,
// and the oldest past max_mb, when diag.db exists. It reads the log
// read-only; the removal opens it to prune.
func planDiag(ctx context.Context, p *gcPlan, l config.Log, now time.Time) error {
	path, err := store.DiagPath()
	if err != nil {
		return err
	}
	fi, err := safefs.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	v, err := store.OpenDiagView(ctx, path, "")
	if err != nil {
		return err
	}
	before := now.Add(-time.Duration(l.RetentionDays) * 24 * time.Hour)
	n, err := v.CountBefore(ctx, before)
	_ = v.Close()
	if err != nil {
		return err
	}
	max := int64(l.MaxMB) << 20
	if n == 0 && fi.Size() <= max {
		return nil
	}
	what := fmt.Sprintf("%d diagnostic record(s) older than %d days in %s", n, l.RetentionDays, path)
	if fi.Size() > max {
		what += fmt.Sprintf(", and the oldest past %d MB", l.MaxMB)
	}
	p.items = append(p.items, gcItem{kind: "diag", what: what, remove: func(ctx context.Context) error {
		db, err := store.OpenDiag(ctx, path)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		_, err = db.Prune(ctx, before, max)
		return err
	}})
	return nil
}

// acceptedAt is when w was accepted, or, when that is not known (an
// index from before version 14), when its borrow ended.
func acceptedAt(w store.GCWorktree) time.Time {
	if w.Accepted.IsZero() {
		return w.Ended
	}
	return w.Accepted
}

// keptAccepted says why an accepted worktree must stay, "" if it may go:
// a change in it since the accept (tracked files that differ from the
// accepted tree, or new files that are not ignored), or commits on
// aggy's branch past the commit it was made from whose tip is not the
// accepted tree (commits the edit child made of its own work do not
// count). Ignored files such as node_modules are not work and do not
// count. Every git call names the worktree's admin dir and runs no
// filter or diff program.
func (g *gitWorktrees) keptAccepted(ctx context.Context, wt store.Worktree) (string, error) {
	if wt.Tree == "" {
		return "no tree was recorded for it", nil
	}
	if _, err := safefs.Lstat(wt.Path); err == nil {
		run, err := g.wtRun(ctx, wt)
		if err != nil {
			return "", err
		}
		out, _, err := run("diff", "--no-color", "--no-ext-diff", "--no-textconv", "--name-only", "-z", wt.Tree, "--")
		if err != nil {
			return "", err
		}
		if out != "" {
			return "it has changes made after the accept", nil
		}
		if out, _, err = run("ls-files", "-z", "--others", "--exclude-standard"); err != nil {
			return "", err
		}
		if out != "" {
			return "it has new files made after the accept", nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	why, _, err := g.branchKept(ctx, wt)
	return why, err
}

// branchKept says why aggy's branch for wt must stay, "" if it may be
// deleted, and how many commits deleting it drops. It may go when it is
// gone, at the base commit, or when its tip holds exactly the tree aggy
// recorded (the edit child committing its own work adds nothing the
// accept did not apply; only those commits' messages are lost, which
// the gc listing shows). Any other commit keeps it (M61-1, M61-2).
func (g *gitWorktrees) branchKept(ctx context.Context, wt store.Worktree) (string, int, error) {
	o, err := g.env.gitOutput(ctx, wt.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+wt.Branch+"^{commit}")
	if err != nil {
		return "", 0, err
	}
	head := strings.TrimSpace(string(o.Stdout))
	if o.ExitCode != 0 || head == wt.Base {
		return "", 0, nil
	}
	kept := "its branch " + wt.Branch + " has commits past the recorded edit"
	if wt.Tree == "" {
		return kept, 0, nil
	}
	o, err = g.env.gitOutput(ctx, wt.Repo, "rev-parse", "--verify", "--quiet", head+"^{tree}")
	if err != nil {
		return "", 0, err
	}
	if o.ExitCode != 0 || strings.TrimSpace(string(o.Stdout)) != wt.Tree {
		return kept, 0, nil
	}
	out, _, err := g.env.git(ctx, wt.Repo, "rev-list", "--count", wt.Base+".."+head)
	if err != nil {
		return "", 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	return "", n, err
}

// leftBehind reports whether a leftover whose directory is gone (a
// removal cut short) still has aggy's branch or git admin entry (checked
// as dropAdmin checks it) in its repo. A record not aggy's, or whose
// repo is gone or no longer a git repo, has nothing.
func (g *gitWorktrees) leftBehind(ctx context.Context, wt store.Worktree) (bool, error) {
	if g.owned(wt) != nil {
		return false, nil
	}
	if _, err := safefs.Lstat(wt.Repo); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	o, err := g.env.gitOutput(ctx, wt.Repo, "rev-parse", "--git-dir")
	if err != nil || o.ExitCode != 0 {
		return false, err
	}
	if o, err = g.env.gitOutput(ctx, wt.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+wt.Branch); err != nil || o.ExitCode == 0 {
		return err == nil, err
	}
	// An entry dropAdmin would refuse (another worktree's, or not one
	// git made) is not aggy's to clean: listing it would fail every run.
	admin, err := g.aggyAdmin(ctx, wt)
	return err == nil && admin != "", nil
}

// unchanged reads w's record again just before its removal, since the
// person may have taken a while to answer: a borrow running again, a
// lease taken (a resumed child), or a change of state or claim since the
// plan skips it.
func unchanged(ctx context.Context, s *store.Store, w store.GCWorktree) error {
	cur, err := s.GCWorktree(ctx, w.BorrowID)
	switch {
	case err != nil:
		return err
	case cur.Running:
		return errors.New("skipped: its borrow is running")
	case cur.Held:
		return errors.New("skipped: its lease was taken since (a resumed child?)")
	case cur.Worktree.State != w.Worktree.State || !cur.Worktree.Removed.Equal(w.Worktree.Removed):
		return errors.New("skipped: it changed since")
	}
	return nil
}

// dirSize adds up the sizes of the regular files under dir, following no
// symlink. It is an estimate: what it cannot read counts as nothing.
func dirSize(dir string) int64 {
	var n int64
	ents, err := safefs.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range ents {
		fi, err := e.Info() // lstat: a symlink is counted as itself
		if err != nil {
			continue
		}
		switch {
		case fi.IsDir():
			n += dirSize(filepath.Join(dir, e.Name()))
		case fi.Mode().IsRegular():
			n += fi.Size()
		}
	}
	return n
}

// stateFiles returns the regular files directly in dir (under the state
// dir) whose name ok accepts. A dir that is a symlink is refused; a
// missing one has nothing.
func stateFiles(p *gcPlan, dir string, ok func(string) bool) []fs.FileInfo {
	fi, err := safefs.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		p.skip("%s: %v", dir, err)
		return nil
	case !fi.IsDir():
		p.skip("%s: refusing: not a directory (a symlink?)", dir)
		return nil
	}
	ents, err := safefs.ReadDir(dir)
	if err != nil {
		p.skip("%s: %v", dir, err)
		return nil
	}
	var out []fs.FileInfo
	for _, e := range ents {
		if !ok(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if !fi.Mode().IsRegular() {
			p.skip("%s: refusing: not a regular file (a symlink?)", filepath.Join(dir, e.Name()))
			continue
		}
		out = append(out, fi)
	}
	return out
}

// removeFile removes path only while it is still the file fi.
func removeFile(path string, fi fs.FileInfo) func(context.Context) error {
	return func(context.Context) error { return safefs.RemoveSame(path, fi) }
}

// planInbox adds the inbox reports last written over r.inbox ago.
func planInbox(ctx context.Context, p *gcPlan, s *store.Store, stateDir string, r gcRetention, now time.Time) error {
	dir := filepath.Join(stateDir, borrow.InboxDir)
	files := stateFiles(p, dir, func(n string) bool {
		id, ok := strings.CutSuffix(n, ".md")
		return ok && borrow.ValidID(id)
	})
	for _, fi := range files {
		if now.Sub(fi.ModTime()) < r.inbox {
			continue
		}
		path, id := filepath.Join(dir, fi.Name()), strings.TrimSuffix(fi.Name(), ".md")
		rm := removeFile(path, fi)
		p.items = append(p.items, gcItem{kind: "inbox", what: path, bytes: fi.Size(), remove: func(ctx context.Context) error {
			if err := rm(ctx); err != nil {
				return err
			}
			return s.ClearReport(ctx, id)
		}})
	}
	return nil
}

// planCache adds the research cache entries past their time to live.
func planCache(p *gcPlan, stateDir string, now time.Time) error {
	dir := filepath.Join(stateDir, borrow.ResearchDir, borrow.CacheDir)
	files := stateFiles(p, dir, func(n string) bool {
		key, ok := strings.CutSuffix(n, ".json")
		return ok && len(key) == 64 && strings.Trim(key, "0123456789abcdef") == ""
	})
	for _, fi := range files {
		path := filepath.Join(dir, fi.Name())
		var data []byte // nil: dated by its modified time
		if fi.Size() <= borrow.MaxCacheEntry {
			data, _ = safefs.ReadFileNoFollow(path, fi)
		}
		if borrow.CacheExpired(data, fi.ModTime(), now) {
			p.items = append(p.items, gcItem{kind: "research", what: path, bytes: fi.Size(), remove: removeFile(path, fi)})
		}
	}
	return nil
}
