package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
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
	// allow is the person's setup opt-ins, and cfgPath their config file,
	// named when a setup command is not allowed.
	allow   config.UserWorktree
	cfgPath string
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
	if err == nil {
		err = g.prepare(ctx, wt)
	}
	if err != nil {
		_ = g.Remove(context.WithoutCancel(ctx), wt)
		return store.Worktree{}, err
	}
	return wt, nil
}

// prepare copies the ignored files the repo config lists into a new
// worktree, then runs the repo's setup command when the person allowed
// it. Only edit worktrees get either: research and news children run in
// an empty temp dir and never see a copied file.
func (g *gitWorktrees) prepare(ctx context.Context, wt store.Worktree) error {
	rc, err := config.LoadRepo(wt.Repo)
	if err != nil {
		return err
	}
	for _, rel := range rc.Worktree.Copy {
		if err := g.copyIgnored(ctx, wt, rel); err != nil {
			fmt.Fprintf(g.out, "aggy borrow: not copied into the worktree: %s\n", termsafe.Field(err.Error()))
		}
	}
	return g.setup(ctx, wt, rc.Worktree.Setup)
}

// maxCopy caps one copied file: the copy list is for small local
// settings such as .env, not data.
const maxCopy = 16 << 20

// copyIgnored copies the file rel (validated by config.LoadRepo: clean,
// relative, no .git part) from the main checkout into wt. It refuses a
// symlink anywhere on either side, a file that is not regular or has
// another hard link, a credential file (safefs), and a file that is not
// ignored in the worktree, which would land in the edit's diff. It never
// replaces a file.
func (g *gitWorktrees) copyIgnored(ctx context.Context, wt store.Worktree, rel string) error {
	elems := strings.Split(rel, "/")
	for i := 1; i < len(elems); i++ {
		d := filepath.Join(append([]string{wt.Repo}, elems[:i]...)...)
		if fi, err := safefs.Lstat(d); err != nil || !fi.IsDir() {
			return fmt.Errorf("%s: %s is not a real directory in the main checkout", rel, strings.Join(elems[:i], "/"))
		}
	}
	src := filepath.Join(wt.Repo, filepath.FromSlash(rel))
	fi, err := safefs.Lstat(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s: not in the main checkout", rel)
	case err != nil:
		return fmt.Errorf("%s: %w", rel, err)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%s: not a regular file (a link or a directory)", rel)
	case fi.Size() > maxCopy:
		return fmt.Errorf("%s: over %d bytes", rel, maxCopy)
	}
	// Exit 0: ignored. A tracked file is never reported ignored.
	o, err := g.env.gitOutput(ctx, wt.Path, "--git-dir="+wt.GitDir, "--work-tree="+wt.Path, "check-ignore", "-q", "--", rel)
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	if o.ExitCode != 0 {
		return fmt.Errorf("%s: not ignored in the worktree, so it would land in the edit's diff", rel)
	}
	data, err := safefs.ReadFileNoFollow(src, fi)
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	dir, err := safefs.Lstat(wt.Path)
	if err != nil {
		return err
	}
	if len(elems) > 1 {
		if dir, err = safefs.MkdirChain(wt.Path, dir, elems[:len(elems)-1], 0o700); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	}
	if err := safefs.WriteFileNoFollowIn(dir, filepath.Join(wt.Path, filepath.FromSlash(rel)), data, fi.Mode().Perm()&0o700, nil); err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	fmt.Fprintf(g.out, "aggy borrow: copied %s into the worktree\n", termsafe.Field(rel))
	return nil
}

// setupTimeout bounds the repo's setup command.
const setupTimeout = 15 * time.Minute

// setup runs the repo's worktree setup command in wt. It is the repo's
// own code, as trusted as its build scripts, so it runs only when the
// person's own config allows this exact command for this repo (a repo
// cannot allow it), and it is always shown first. It runs through
// execrun with the scrubbed environment, no stdin, and a time limit.
func (g *gitWorktrees) setup(ctx context.Context, wt store.Worktree, cmd config.Command) error {
	if len(cmd) == 0 {
		return nil
	}
	shown := quoteCommand(cmd)
	if !g.allow.AllowsSetup(canonicalPath(wt.Repo), cmd, canonicalPath) {
		fmt.Fprintf(g.out, "aggy borrow: the repo's worktree setup command was not run: %s\n", shown)
		fmt.Fprintf(g.out, "  It runs the repo's own code. To allow it for this repo, add to %s:\n", termsafe.Field(g.cfgPath))
		fmt.Fprintf(g.out, "  [[worktree.allow_setup]]\n  repo = %s\n  command = [%s]\n", tomlString(wt.Repo), tomlList(cmd))
		return nil
	}
	fmt.Fprintf(g.out, "aggy borrow: running the repo's worktree setup command in %s: %s\n", termsafe.Field(wt.Path), shown)
	prog := cmd[0]
	switch {
	case !strings.Contains(prog, "/"):
		p, err := probe.LookPath(prog)
		if err != nil {
			return fmt.Errorf("worktree setup: %s: %w", termsafe.Field(prog), err)
		}
		prog = p
	case !filepath.IsAbs(prog):
		prog = filepath.Join(wt.Path, prog)
		if !within(prog, wt.Path) {
			return fmt.Errorf("worktree setup: %s is outside the worktree", termsafe.Field(cmd[0]))
		}
	}
	if g.env.setupRun == nil {
		return errors.New("worktree setup: no runner")
	}
	out, err := g.env.setupRun(ctx, adapter.Command{Path: prog, Args: cmd[1:], Dir: wt.Path})
	if err == nil && out.ExitCode != 0 {
		tail := borrow.Excerpt(string(out.Stderr), 5, 160)
		err = fmt.Errorf("exit %d: %s", out.ExitCode, termsafe.Field(strings.Join(tail, " | ")))
	}
	if err != nil {
		return fmt.Errorf("worktree setup: %w", err)
	}
	return nil
}

// quoteCommand shows a command for the person, each part quoted and
// escaped for the terminal.
func quoteCommand(cmd config.Command) string {
	parts := make([]string, len(cmd))
	for i, c := range cmd {
		parts[i] = termsafe.Field(strconv.Quote(c))
	}
	return strings.Join(parts, " ")
}

func tomlString(s string) string { return termsafe.Field(strconv.Quote(s)) }

func tomlList(cmd config.Command) string {
	parts := make([]string, len(cmd))
	for i, c := range cmd {
		parts[i] = tomlString(c)
	}
	return strings.Join(parts, ", ")
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
// A worktree directory already gone has only aggy's own admin entry
// removed (dropAdmin), never a repo-wide prune, which would also drop the
// person's own worktrees that happen to be missing (an unmounted disk).
func (g *gitWorktrees) Remove(ctx context.Context, wt store.Worktree) error {
	if err := g.owned(wt); err != nil {
		return err
	}
	off, err := g.env.filterOff(ctx, wt.Repo)
	if err != nil {
		return err
	}
	// A symlink (or anything but a directory) at aggy's path is not the
	// worktree aggy made: removing through it could reach another tree.
	switch fi, err := safefs.Lstat(wt.Path); {
	case errors.Is(err, fs.ErrNotExist):
		if err := g.dropAdmin(ctx, wt); err != nil {
			return err
		}
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("refusing: %s is not a directory (a symlink?)", wt.Path)
	default:
		if _, _, err := g.env.git(ctx, wt.Repo, append(off, "worktree", "remove", "--force", "--", wt.Path)...); err != nil {
			return err
		}
	}
	// A removal cut short after the worktree went may have left the
	// branch, or not.
	o, err := g.env.gitOutput(ctx, wt.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+wt.Branch)
	if err != nil || o.ExitCode != 0 {
		return err
	}
	_, _, err = g.env.git(ctx, wt.Repo, "branch", "-D", wt.Branch)
	return err
}

// dropAdmin removes the admin entry of aggy's worktree wt, whose
// directory is gone: <git-common-dir>/worktrees/<id>, only when it is a
// real directory whose gitdir file points at wt's own .git, so it is the
// entry git made for this worktree and no other.
func (g *gitWorktrees) dropAdmin(ctx context.Context, wt store.Worktree) error {
	out, _, err := g.env.git(ctx, wt.Repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	admin := filepath.Join(strings.TrimSpace(out), "worktrees", filepath.Base(wt.Path))
	if wt.GitDir != "" && filepath.Clean(wt.GitDir) != admin {
		return fmt.Errorf("refusing: the recorded git dir %s is not %s", wt.GitDir, admin)
	}
	fi, err := safefs.Lstat(admin)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // nothing left to remove
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("refusing: %s is not a directory", admin)
	}
	data, err := safefs.ReadFileLimit(filepath.Join(admin, "gitdir"), 4096)
	if err != nil {
		return fmt.Errorf("refusing to remove %s: %w", admin, err)
	}
	if got := filepath.Clean(strings.TrimSpace(string(data))); got != filepath.Join(wt.Path, ".git") {
		return fmt.Errorf("refusing: %s belongs to %s, not aggy's %s", admin, got, wt.Path)
	}
	return safefs.RemoveAll(admin)
}
