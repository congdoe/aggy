package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const acceptUsage = `Usage: aggy accept <borrow-id> [--check] [--yes]
       aggy reject <borrow-id>

accept shows the stat of an edit borrow's diff against the commit its
worktree was made from and what the repo's checks said about it, offers
to run them first (--check runs them without asking; see aggy check),
then applies the diff to the main checkout on y (or --yes). A failed
check does not stop an accept, but it is shown as a warning. It refuses a diff that touches .git, a symlink or submodule,
any top-level dot dir, the repo's core.hooksPath, or a file that holds
hooks, CI, config, or instructions a tool or CLI loads, and a main
checkout with changes in the paths the diff touches.

reject removes the edit's worktree and its aggy/edit-<id> branch.
`

func runAccept(args []string, stdout, stderr io.Writer) int {
	return acceptWith(args, defaultBorrowEnv(), os.Stdin, isTerminal(os.Stdin), stdout, stderr)
}

func runReject(args []string, stdout, stderr io.Writer) int {
	return rejectWith(args, defaultBorrowEnv(), stdout, stderr)
}

// editTarget finds the edit borrow named by an id or unique id prefix
// and its worktree, still pending, and checks it is one aggy made.
func editTarget(ctx context.Context, s *store.Store, g *gitWorktrees, id string) (store.Borrow, store.Worktree, error) {
	bs, err := s.BorrowsByPrefix(ctx, id)
	if err != nil || id == "" {
		return store.Borrow{}, store.Worktree{}, fmt.Errorf("no borrow %q", id)
	}
	if len(bs) != 1 {
		return store.Borrow{}, store.Worktree{}, fmt.Errorf("%d borrows start with %q", len(bs), id)
	}
	b := bs[0]
	wt, err := s.BorrowWorktree(ctx, b.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return b, wt, fmt.Errorf("borrow %s has no worktree (only edit borrows do)", b.ID)
	case err != nil:
		return b, wt, err
	case b.Status == "running":
		return b, wt, fmt.Errorf("borrow %s is still running", b.ID)
	case wt.State != store.WorktreePending:
		return b, wt, fmt.Errorf("borrow %s was already %s", b.ID, wt.State)
	}
	return b, wt, g.owned(wt)
}

// openEdit parses <id> and any of the flags allowed, and opens the store
// and finds the state dir for an accept, reject, or check.
func openEdit(args []string, allowed ...string) (id string, flags map[string]bool, s *store.Store, stateDir string, err error) {
	flags = map[string]bool{}
	for _, a := range args {
		switch {
		case slices.Contains(allowed, a):
			flags[a] = true
		case strings.HasPrefix(a, "-") || id != "":
			return "", nil, nil, "", fmt.Errorf("unexpected %s", termsafe.Field(a))
		default:
			id = a
		}
	}
	if id == "" {
		return "", nil, nil, "", errors.New("want a borrow id")
	}
	if stateDir, err = home.Dir(); err != nil {
		return "", nil, nil, "", err
	}
	s, err = store.OpenDefault(context.Background())
	return id, flags, s, stateDir, err
}

// asker reads y/n answers from one reader, so two questions in a row
// do not lose buffered input.
func asker(in io.Reader, stdout io.Writer) func(q string) bool {
	r := bufio.NewReader(io.LimitReader(in, 2*maxAnswer))
	return func(q string) bool {
		fmt.Fprintf(stdout, "%s [y/N] ", q)
		line, _ := r.ReadString('\n')
		a := strings.ToLower(strings.TrimSpace(line))
		return a == "y" || a == "yes"
	}
}

// showEdit checks the edit's diff (editDiff) and prints its stat and the
// check gate's result so far. It returns the patch.
func showEdit(ctx context.Context, g *gitWorktrees, b store.Borrow, wt store.Worktree, stdout io.Writer) (string, error) {
	if _, err := safefs.Lstat(wt.Path); err != nil {
		return "", fmt.Errorf("the worktree is gone: %w", err)
	}
	stat, patch, err := g.editDiff(ctx, wt)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(stdout, "borrow %s: diff against %.12s\n", b.ID, wt.Base)
	for _, l := range strings.Split(strings.TrimRight(stat, "\n"), "\n") {
		fmt.Fprintf(stdout, "  %s\n", termsafe.Line(l))
	}
	fmt.Fprintf(stdout, "checks: %s\n", gateText(wt.Check))
	return patch, nil
}

func gateText(r string) string {
	switch r {
	case store.CheckPass:
		return "passed"
	case store.CheckFail:
		return "FAILED"
	case store.CheckSkipped:
		return "skipped"
	}
	return "not run"
}

// gateEdit is the check gate on an edit's diff (W-5), run only after
// showEdit printed its stat. The check list comes from the main
// checkout's config, never from the worktree, where the child could have
// written one; the commands run in the worktree, on the child's code,
// only when run is true or ask says yes. It records pass or fail, or
// skipped when the person said no, and returns the result ("" when
// nothing was decided).
func gateEdit(ctx context.Context, s *store.Store, g *gitWorktrees, ce repoCheckEnv, b store.Borrow, wt store.Worktree, run bool, ask func(string) bool, stdout io.Writer) (string, error) {
	rc, err := config.LoadRepo(wt.Repo)
	if err != nil {
		return "", err
	}
	if len(rc.Check) == 0 {
		fmt.Fprintf(stdout, "No checks configured in %s.\n", termsafe.Field(config.RepoPath(wt.Repo)))
		return "", nil
	}
	fmt.Fprintf(stdout, "The repo's checks, from %s (the main checkout, not the worktree), %d minutes each at most:\n",
		termsafe.Field(config.RepoPath(wt.Repo)), checkMinutes(rc))
	for _, c := range rc.Check {
		fmt.Fprintf(stdout, "  %s\n", showCommand(c))
	}
	fmt.Fprintf(stdout, "They run in %s, outside the child's sandbox, as you. That worktree is the borrow child's work: its tests, scripts, and build files are code the child wrote.\n",
		termsafe.Field(wt.Path))
	if !run {
		if ask == nil {
			fmt.Fprintf(stdout, "Checks not run: run aggy check %s --yes, or in a terminal, to run them.\n", b.ID)
			return "", nil
		}
		if !ask("Run them?") {
			fmt.Fprintln(stdout, "Checks skipped.")
			return store.CheckSkipped, s.SetWorktreeCheck(ctx, b.ID, store.CheckSkipped)
		}
	}
	res := store.CheckPass
	if ce.runList(ctx, wt.Path, rc, stdout) != 0 {
		res = store.CheckFail
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err := s.SetWorktreeCheck(context.WithoutCancel(ctx), b.ID, res); err != nil {
		return "", err
	}
	// A check that wrote files the diff would hold makes accept refuse the
	// worktree (editDiff), so say so now.
	if tree, err := g.Tree(ctx, wt); err != nil || tree != wt.Tree {
		fmt.Fprintln(stdout, "Warning: the checks changed files in the worktree, so aggy accept will refuse it until they are undone.")
	}
	return res, nil
}

// checkEditWith is aggy check <borrow-id> [--yes]: the check gate on an
// edit's diff, on its own.
func checkEditWith(ctx context.Context, args []string, env borrowEnv, ce repoCheckEnv, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	id, flags, s, stateDir, err := openEdit(args, "--yes")
	if err != nil {
		fmt.Fprintf(stderr, "aggy check: %s\n\n%s", termsafe.Field(err.Error()), checkUsage)
		return 2
	}
	defer func() { _ = s.Close() }()
	g := &gitWorktrees{env: env, stateDir: stateDir, out: stderr}
	b, wt, err := editTarget(ctx, s, g, id)
	if err == nil {
		_, err = showEdit(ctx, g, b, wt, stdout)
	}
	var ask func(string) bool
	if tty {
		ask = asker(in, stdout)
	}
	res := ""
	if err == nil {
		res, err = gateEdit(ctx, s, g, ce, b, wt, flags["--yes"], ask, stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "aggy check: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	if res != store.CheckPass {
		return 1
	}
	return 0
}

func rejectWith(args []string, env borrowEnv, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(stdout, acceptUsage)
		return 0
	}
	id, _, s, stateDir, err := openEdit(args)
	if err != nil {
		fmt.Fprintf(stderr, "aggy reject: %s\n\n%s", termsafe.Field(err.Error()), acceptUsage)
		return 2
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	g := &gitWorktrees{env: env, stateDir: stateDir, out: stderr}
	b, wt, err := editTarget(ctx, s, g, id)
	// The state is the per-record lock accept takes too: whichever moves
	// it off pending first wins, and the other refuses.
	if err == nil {
		err = s.SetWorktreeState(ctx, b.ID, store.WorktreePending, store.WorktreeRejected)
	}
	if err == nil {
		if err = g.Remove(ctx, wt); err != nil {
			_ = s.SetWorktreeState(ctx, b.ID, store.WorktreeRejected, store.WorktreePending)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "aggy reject: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	fmt.Fprintf(stdout, "Rejected: removed %s and branch %s.\n", termsafe.Field(wt.Path), termsafe.Field(wt.Branch))
	return 0
}

func acceptWith(args []string, env borrowEnv, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(stdout, acceptUsage)
		return 0
	}
	id, flags, s, stateDir, err := openEdit(args, "--yes", "--check")
	if err != nil {
		fmt.Fprintf(stderr, "aggy accept: %s\n\n%s", termsafe.Field(err.Error()), acceptUsage)
		return 2
	}
	defer func() { _ = s.Close() }()
	fail := func(err error) int {
		fmt.Fprintf(stderr, "aggy accept: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	// Ctrl-C, a kill, or a hangup during the checks kills their process
	// group (execrun.CheckRunner).
	ctx, stop := execrun.NotifyContext(context.Background())
	defer stop()
	yes := flags["--yes"]
	g := &gitWorktrees{env: env, stateDir: stateDir, out: stderr}
	b, wt, err := editTarget(ctx, s, g, id)
	if err != nil {
		return fail(err)
	}
	patch, err := showEdit(ctx, g, b, wt, stdout)
	if err != nil {
		return fail(err)
	}
	var ask func(string) bool
	if tty {
		ask = asker(in, stdout)
	}
	if flags["--check"] || (wt.Check == "" && tty && !yes) {
		if wt.Check, err = gateEdit(ctx, s, g, defaultRepoCheckEnv(), b, wt, flags["--check"], ask, stdout); err != nil {
			return fail(err)
		}
	}
	if wt.Check == store.CheckFail {
		fmt.Fprintln(stdout, "Warning: the repo's checks FAILED on this diff.")
	}
	if !yes {
		if !tty {
			fmt.Fprintln(stdout, "Not applied: run again with --yes, or in a terminal, to apply it.")
			return 1
		}
		if !ask(fmt.Sprintf("Apply this to %s?", termsafe.Field(wt.Repo))) {
			fmt.Fprintln(stdout, "Not applied.")
			return 1
		}
	}
	if wt.Check == "" {
		// Applied without the checks: recorded, so the record says so.
		_ = s.SetWorktreeCheck(ctx, b.ID, store.CheckSkipped)
	}
	// Claimed before the write (the per-record lock reject takes too), so
	// a second accept or a reject cannot act on it meanwhile.
	if err := s.SetWorktreeState(ctx, b.ID, store.WorktreePending, store.WorktreeAccepted); err != nil {
		return fail(err)
	}
	if err := g.apply(ctx, wt, patch); err != nil {
		_ = s.SetWorktreeState(context.WithoutCancel(ctx), b.ID, store.WorktreeAccepted, store.WorktreePending)
		return fail(err)
	}
	fmt.Fprintf(stdout, "Applied to %s (not staged or committed). The worktree and branch %s stay until aggy gc.\n",
		termsafe.Field(wt.Repo), termsafe.Field(wt.Branch))
	return 0
}

// wtGit is the git options that name wt's admin dir and work tree, so
// git never reads the worktree's .git file, which the child could have
// rewritten to point at another repo's config.
func wtGit(wt store.Worktree) []string {
	return []string{"--git-dir=" + wt.GitDir, "--work-tree=" + wt.Path}
}

// wtRun runs git on wt through its admin dir, with every filter driver
// switched off.
func (g *gitWorktrees) wtRun(ctx context.Context, wt store.Worktree) (func(args ...string) (string, bool, error), error) {
	pre := wtGit(wt)
	off, err := g.env.filterOff(ctx, wt.Path, pre...)
	if err != nil {
		return nil, err
	}
	return func(args ...string) (string, bool, error) {
		return g.env.git(ctx, wt.Path, append(append(append([]string{}, pre...), off...), args...)...)
	}, nil
}

// Tree stages everything the child left in wt, in the worktree's own
// index, and returns the git tree of it.
func (g *gitWorktrees) Tree(ctx context.Context, wt store.Worktree) (string, error) {
	run, err := g.wtRun(ctx, wt)
	if err != nil {
		return "", err
	}
	if _, _, err := run("add", "-A"); err != nil {
		return "", err
	}
	out, _, err := run("write-tree")
	tree := strings.TrimSpace(out)
	if err == nil && (len(tree) < 40 || strings.Trim(tree, "0123456789abcdef") != "") {
		err = fmt.Errorf("git gave %q for the tree", tree)
	}
	return tree, err
}

// editDiff returns the diff stat and the binary patch from the base
// commit to the tree recorded when the borrow ended. It refuses a
// worktree that no longer matches that tree (a process the child left
// may have changed it since), a diff that is empty, too large to hold
// whole, or touches what an accept must never write (checkTouched), and
// a main checkout with changes in the touched paths.
func (g *gitWorktrees) editDiff(ctx context.Context, wt store.Worktree) (stat, patch string, err error) {
	if wt.Tree == "" {
		return "", "", errors.New("no state of the worktree was recorded when the borrow ended; review it by hand, then aggy reject it")
	}
	now, err := g.Tree(ctx, wt)
	if err != nil {
		return "", "", err
	}
	if now != wt.Tree {
		return "", "", fmt.Errorf("refusing: the worktree changed after the borrow ended (a process the child left behind?); review %s by hand", wt.Path)
	}
	run, err := g.wtRun(ctx, wt)
	if err != nil {
		return "", "", err
	}
	diff := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames"}
	raw, cut, err := run(append(diff, "--raw", "-z", wt.Base, wt.Tree, "--")...)
	if err != nil {
		return "", "", err
	}
	if cut {
		return "", "", errors.New("the diff touches too many files to check")
	}
	hooks, err := g.hooksDir(ctx, wt.Repo)
	if err != nil {
		return "", "", err
	}
	paths, err := checkTouched(raw, hooks)
	if err != nil {
		return "", "", err
	}
	if len(paths) == 0 {
		return "", "", errors.New("the edit changed nothing")
	}
	if err := g.cleanIn(ctx, wt.Repo, paths); err != nil {
		return "", "", err
	}
	if stat, _, err = run(append(diff, "--stat", wt.Base, wt.Tree, "--")...); err != nil {
		return "", "", err
	}
	// shortcut: the patch is held in memory, capped at execrun.MaxOutput;
	// a larger edit is refused until a streamed apply exists.
	if patch, cut, err = run(append(diff, "--binary", "--full-index", wt.Base, wt.Tree, "--")...); err != nil {
		return "", "", err
	}
	if cut {
		return "", "", fmt.Errorf("the diff is over %d bytes, too large to apply here; review it in %s", execrun.MaxOutput, wt.Path)
	}
	return stat, patch, nil
}

// hooksDir returns the repo's own core.hooksPath (the last value from
// git's config files, never aggy's own -c override) relative to repo:
// "" when it is unset or outside the repo, "." when it is the root.
func (g *gitWorktrees) hooksDir(ctx context.Context, repo string) (string, error) {
	o, err := g.env.gitOutput(ctx, repo, "config", "--show-origin", "-z", "--get-all", "core.hooksPath")
	switch {
	case err != nil:
		return "", err
	case o.ExitCode == 1 && len(o.Stdout) == 0:
		return "", nil
	case o.ExitCode != 0:
		return "", fmt.Errorf("git config: %s", strings.TrimSpace(string(o.Stderr)))
	}
	var val string
	f := strings.Split(strings.TrimSuffix(string(o.Stdout), "\x00"), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		if !strings.HasPrefix(f[i], "command line:") {
			val = f[i+1]
		}
	}
	if val == "" {
		return "", nil
	}
	if filepath.IsAbs(val) {
		if !within(val, repo) {
			return "", nil
		}
		val, _ = filepath.Rel(repo, val)
	}
	val = filepath.ToSlash(filepath.Clean(val))
	if val == ".." || strings.HasPrefix(val, "../") || strings.HasPrefix(val, "~") {
		return "", nil
	}
	return val, nil
}

// checkTouched reads git diff --raw -z output and returns the touched
// paths. It refuses a symlink or submodule on either side, any path
// borrow.ProtectedPath names, and any path in hooks, the repo's
// core.hooksPath relative to its root ("" for none, "." for the root,
// which protects every top-level file).
func checkTouched(raw, hooks string) ([]string, error) {
	fields := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	var paths []string
	for i := 0; i+1 < len(fields); i += 2 {
		meta, p := fields[i], fields[i+1]
		m := strings.Fields(strings.TrimPrefix(meta, ":"))
		if len(m) < 2 {
			return nil, fmt.Errorf("git diff gave %q", meta)
		}
		for _, mode := range m[:2] {
			if mode == "120000" || mode == "160000" {
				return nil, fmt.Errorf("refusing: the diff makes or changes a symlink or submodule at %s", p)
			}
		}
		inHooks := hooks != "" && ((hooks == "." && !strings.Contains(p, "/")) ||
			strings.EqualFold(p, hooks) || strings.HasPrefix(strings.ToLower(p), strings.ToLower(hooks)+"/"))
		if borrow.ProtectedPath(p) || inHooks {
			return nil, fmt.Errorf("refusing: the diff touches %s, which may hold hooks, CI, config, or instructions a tool or CLI runs or loads; review the worktree by hand", p)
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// cleanIn refuses when the main checkout at repo has any change, staged,
// unstaged, or untracked, in paths.
func (g *gitWorktrees) cleanIn(ctx context.Context, repo string, paths []string) error {
	off, err := g.env.filterOff(ctx, repo)
	if err != nil {
		return err
	}
	args := append(append([]string{"--literal-pathspecs"}, off...), "status", "--porcelain", "-z", "--untracked-files=all", "--")
	out, _, err := g.env.git(ctx, repo, append(args, paths...)...)
	if err != nil {
		return err
	}
	if out != "" {
		var dirty []string
		for _, e := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
			if len(e) > 3 {
				dirty = append(dirty, e[3:])
			}
		}
		return fmt.Errorf("refusing: the main checkout has changes in paths the diff touches (%s); commit or stash them first", strings.Join(dirty, ", "))
	}
	return nil
}

// apply checks and applies patch to the main checkout's work tree with
// git apply, which refuses paths outside the repo and writes through no
// symlink, with every filter driver switched off.
func (g *gitWorktrees) apply(ctx context.Context, wt store.Worktree, patch string) error {
	dir, err := safefs.MkdirTemp(g.env.tempDir, "aggy-accept-")
	if err != nil {
		return err
	}
	defer func() { _ = safefs.RemoveAll(dir) }()
	file := filepath.Join(dir, "edit.patch")
	if err := safefs.WriteFile(file, []byte(patch), 0o600); err != nil {
		return err
	}
	off, err := g.env.filterOff(ctx, wt.Repo)
	if err != nil {
		return err
	}
	if _, _, err := g.env.git(ctx, wt.Repo, append(off, "apply", "--check", "--binary", file)...); err != nil {
		return fmt.Errorf("the diff no longer applies: %w", err)
	}
	_, _, err = g.env.git(ctx, wt.Repo, append(off, "apply", "--binary", file)...)
	return err
}
