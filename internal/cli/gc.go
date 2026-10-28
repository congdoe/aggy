package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const gcUsage = `Usage: aggy gc [--yes]

Removes what aggy made and no longer needs: edit worktrees that were
accepted (rejected ones are already gone), and pending ones left
untouched for 30 days; closed borrow children in aggy's index (never the
vendor's own session files); inbox reports older than 30 days; and
expired research cache entries. It lists stale leases whose pid is alive
again but kills nothing it cannot prove it started.

It shows what it would remove, and removes it only with --yes.
`

// gcRetention is how long aggy gc keeps what it may remove.
type gcRetention struct {
	pendingWorktree, closedChild, inbox time.Duration
}

var defaultRetention = gcRetention{
	pendingWorktree: 30 * 24 * time.Hour,
	closedChild:     30 * 24 * time.Hour,
	inbox:           30 * 24 * time.Hour,
}

// gcItem is one thing aggy gc would remove.
type gcItem struct {
	kind, what string
	bytes      int64
	remove     func(context.Context) error
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
	return gcWith(args, defaultBorrowEnv(), stdout, stderr)
}

func gcWith(args []string, env borrowEnv, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("gc", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	yes := fl.Bool("yes", false, "")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, gcUsage)
			return 0
		}
		fmt.Fprintf(stderr, "aggy gc: %v\n\n%s", err, gcUsage)
		return 2
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "aggy gc: unexpected arguments\n\n%s", gcUsage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "aggy gc: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	// A borrow child never cleans up after its parent.
	if env.getenv(borrow.Delegated) != "" {
		return fail(borrow.ErrDelegated)
	}
	stateDir, err := home.Dir()
	if err != nil {
		return fail(err)
	}
	path, err := store.DefaultPath()
	if err != nil {
		return fail(err)
	}
	if _, err := safefs.Stat(path); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(stdout, "Nothing to remove: aggy has no index yet.")
		return 0
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = s.Close() }()
	g := &gitWorktrees{env: env, stateDir: stateDir, out: stderr}
	plan, err := planGC(ctx, s, g, stateDir, defaultRetention, env.now())
	if err != nil {
		return fail(store.ReadError(err))
	}
	showPlan(stdout, plan)
	if len(plan.items) == 0 {
		return 0
	}
	if !*yes {
		fmt.Fprintln(stdout, "Nothing removed: run again with --yes to remove these.")
		return 1
	}
	code := 0
	for _, it := range plan.items {
		if err := it.remove(ctx); err != nil {
			fmt.Fprintf(stderr, "aggy gc: not removed: %s %s: %s\n", it.kind, termsafe.Field(it.what), termsafe.Field(err.Error()))
			code = 1
			continue
		}
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
			kind: "child",
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
// rejected one whose removal was cut short, and pending ones whose
// borrow ended over r.pendingWorktree ago. Each must be one aggy made
// (gitWorktrees.owned), not a symlink, with its borrow finished and its
// lease free.
func planWorktrees(ctx context.Context, p *gcPlan, s *store.Store, g *gitWorktrees, r gcRetention, now time.Time) error {
	wts, err := s.GCWorktrees(ctx)
	if err != nil {
		return err
	}
	for _, w := range wts {
		wt, id := w.Worktree, w.BorrowID
		switch {
		case w.Running:
			p.skip("worktree of borrow %s: the borrow is still running", id)
			continue
		case w.Held:
			p.skip("worktree of borrow %s: its lease is held by a running aggy", id)
			continue
		case wt.State == store.WorktreePending && now.Sub(w.Ended) < r.pendingWorktree:
			continue // waiting for accept or reject
		case wt.State != store.WorktreePending && wt.State != store.WorktreeAccepted && wt.State != store.WorktreeRejected:
			continue
		}
		if err := g.owned(wt); err != nil {
			p.skip("worktree of borrow %s: %v", id, err)
			continue
		}
		fi, err := safefs.Lstat(wt.Path)
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
		p.items = append(p.items, gcItem{
			kind:   "worktree",
			what:   fmt.Sprintf("%s (%s, branch %s)", wt.Path, wt.State, wt.Branch),
			bytes:  size,
			remove: func(ctx context.Context) error { return discardEdit(ctx, s, g, id, wt, now) },
		})
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
