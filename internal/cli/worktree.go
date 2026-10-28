package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// worktreesDir is where edit worktrees live, under the state dir: never
// inside the repo, so the main checkout's status and tools never see them.
const worktreesDir = "worktrees"

// branchPrefix is the prefix of aggy's own edit branches. aggy deletes a
// branch only when its name is this prefix and the borrow id.
const branchPrefix = "aggy/edit-"

// gitWorktrees makes and removes edit worktrees with git (borrow.Worktrees).
// Every git call takes gitSafe and, where git may convert file content,
// the filter drivers switched off (filterOff): no hook, fsmonitor, filter,
// or diff program from the repo's config runs.
type gitWorktrees struct {
	env      borrowEnv
	stateDir string
	out      io.Writer // progress lines for the person
}

// validBorrowID reports whether id is a borrow id: 16 lowercase hex digits.
func validBorrowID(id string) bool {
	return len(id) == 16 && strings.Trim(id, "0123456789abcdef") == ""
}

// worktreePath is where borrow id's worktree goes.
func (g *gitWorktrees) worktreePath(id string) string {
	return filepath.Join(resolvePath(filepath.Join(g.stateDir, worktreesDir)), id)
}

// Create makes a worktree for borrow id at <state>/worktrees/<id> on a new
// branch aggy/edit-<id>, from the main checkout's HEAD commit.
func (g *gitWorktrees) Create(ctx context.Context, repo, id string) (store.Worktree, error) {
	if !validBorrowID(id) {
		return store.Worktree{}, fmt.Errorf("bad borrow id %q", id)
	}
	if st := canonicalPath(g.stateDir); st != "" {
		if c := canonicalPath(repo); within(c, st) || within(st, c) {
			return store.Worktree{}, fmt.Errorf("the repo %s holds or is inside aggy's state dir %s", repo, g.stateDir)
		}
	}
	out, _, err := g.env.git(ctx, repo, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return store.Worktree{}, fmt.Errorf("the repo has no commit to branch from: %w", err)
	}
	base := strings.TrimSpace(out)
	if len(base) < 40 || strings.Trim(base, "0123456789abcdef") != "" {
		return store.Worktree{}, fmt.Errorf("git gave %q for HEAD", base)
	}
	if err := safefs.MkdirAll(filepath.Join(g.stateDir, worktreesDir), 0o700); err != nil {
		return store.Worktree{}, err
	}
	wt := store.Worktree{Path: g.worktreePath(id), Branch: branchPrefix + id, Base: base, Repo: repo, State: store.WorktreePending}
	off, err := g.env.filterOff(ctx, repo)
	if err != nil {
		return store.Worktree{}, err
	}
	// Checkout would run a smudge filter and the post-checkout hook:
	// filterOff and gitSafe switch both off.
	if _, _, err := g.env.git(ctx, repo, append(off, "worktree", "add", "-b", wt.Branch, "--", wt.Path, base)...); err != nil {
		return store.Worktree{}, err
	}
	// The admin dir, read from the .git file git just wrote: aggy's later
	// git calls name it, never the .git file the child can rewrite.
	gd, _, err := g.env.git(ctx, wt.Path, "rev-parse", "--absolute-git-dir")
	if err == nil {
		wt.GitDir = strings.TrimSpace(gd)
		if !filepath.IsAbs(wt.GitDir) {
			err = fmt.Errorf("git gave %q for the worktree's git dir", wt.GitDir)
		}
	}
	if err != nil {
		_ = g.Remove(context.WithoutCancel(ctx), wt)
		return store.Worktree{}, err
	}
	return wt, nil
}

// owned refuses a worktree record that is not one aggy made: its path
// must be <state>/worktrees/<id> and its branch aggy/edit-<id>, for one
// borrow id. A record is aggy's own store, but a wrong row must never
// make aggy remove another directory or branch.
func (g *gitWorktrees) owned(wt store.Worktree) error {
	id := filepath.Base(wt.Path)
	if !validBorrowID(id) || wt.Path != g.worktreePath(id) || wt.Branch != branchPrefix+id || !filepath.IsAbs(wt.Repo) {
		return fmt.Errorf("refusing: %s on %s is not a worktree aggy made", wt.Path, wt.Branch)
	}
	return nil
}

// Remove removes wt and its branch with git: git worktree remove, never
// a recursive delete of a path, then git branch -D on aggy's own branch.
// A worktree directory already gone is pruned from git's records.
func (g *gitWorktrees) Remove(ctx context.Context, wt store.Worktree) error {
	if err := g.owned(wt); err != nil {
		return err
	}
	off, err := g.env.filterOff(ctx, wt.Repo)
	if err != nil {
		return err
	}
	if _, err := safefs.Lstat(wt.Path); errors.Is(err, fs.ErrNotExist) {
		if _, _, err := g.env.git(ctx, wt.Repo, "worktree", "prune"); err != nil {
			return err
		}
	} else if _, _, err := g.env.git(ctx, wt.Repo, append(off, "worktree", "remove", "--force", "--", wt.Path)...); err != nil {
		return err
	}
	_, _, err = g.env.git(ctx, wt.Repo, "branch", "-D", wt.Branch)
	return err
}
