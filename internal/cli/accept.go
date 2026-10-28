package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
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
		return store.Borrow{}, store.Worktree{}, withCode(diag.CodeAcceptTarget, fmt.Errorf("no borrow %q", id))
	}
	if len(bs) != 1 {
		return store.Borrow{}, store.Worktree{}, withCode(diag.CodeAcceptTarget, fmt.Errorf("%d borrows start with %q", len(bs), id))
	}
	b := bs[0]
	wt, err := s.BorrowWorktree(ctx, b.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		err = fmt.Errorf("borrow %s has no worktree (only edit borrows do)", b.ID)
	case err != nil:
		return b, wt, withCode(diag.CodeStore, err)
	case b.Status == "running":
		err = fmt.Errorf("borrow %s is still running", b.ID)
	case wt.State != store.WorktreePending:
		err = fmt.Errorf("borrow %s was already %s", b.ID, wt.State)
	case !wt.Removed.IsZero():
		err = fmt.Errorf("borrow %s: its worktree was removed (aggy gc)", b.ID)
	default:
		err = g.owned(wt)
	}
	return b, wt, withCode(diag.CodeAcceptTarget, err)
}

// editRec is the diagnostic log's ids for edit borrow b.
func editRec(b store.Borrow, wt store.Worktree) diag.Rec {
	r := diag.Rec{Borrow: b.ID, Child: b.ChildID, Vendor: b.Vendor}
	if wt.Path != "" {
		r.Worktree = filepath.Base(wt.Path)
	}
	return r
}

// discardEdit removes borrow id's worktree and branch (aggy reject, and
// aggy gc). A pending one moves to rejected first: the state is the
// per-record lock accept takes too, so whichever moves it off pending
// first wins and the other refuses. The removal is then claimed in the
// store, so a reject and a gc never both act on it. A failure gives both
// back.
func discardEdit(ctx context.Context, s *store.Store, g *gitWorktrees, id string, wt store.Worktree, now time.Time) error {
	if err := g.owned(wt); err != nil {
		return err
	}
	pending := wt.State == store.WorktreePending
	if pending {
		if err := s.SetWorktreeState(ctx, id, store.WorktreePending, store.WorktreeRejected); err != nil {
			return err
		}
		wt.State = store.WorktreeRejected
	}
	undo := func() {
		if pending {
			_ = s.SetWorktreeState(context.WithoutCancel(ctx), id, store.WorktreeRejected, store.WorktreePending)
		}
	}
	if err := s.ClaimWorktreeRemoval(ctx, id, wt.State, now); err != nil {
		undo()
		return err
	}
	if err := g.Remove(ctx, wt); err != nil {
		_ = s.UnclaimWorktreeRemoval(ctx, id)
		undo()
		return err
	}
	return nil
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
		return "", nil, nil, "", withCode(diag.CodeStore, err)
	}
	s, err = store.OpenDefault(context.Background())
	return id, flags, s, stateDir, withCode(diag.CodeStore, err)
}

// editUsage prints an openEdit error: a usage error with the usage text
// (exit 2), or a store error (exit 1).
func editUsage(stderr io.Writer, cmd string, ev diag.Event, err error, usage string) int {
	c := codeOf(err, diag.CodeUsage)
	refuse(context.Background(), stderr, cmd, ev, c, err.Error(), diag.Rec{})
	if c != diag.CodeUsage {
		return 1
	}
	fmt.Fprintf(stderr, "\n%s", usage)
	return 2
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
	if d := g.env.childWritable(wt.Repo); d != "" {
		return "", withCode(diag.CodeGateTempDir, fmt.Errorf("refusing to run the checks: the main checkout %s is under %s, which an edit child's sandbox may write (Codex workspace-write leaves temp dirs writable unless told otherwise), so its check list may be the child's; run the edit from a repo outside temp dirs", wt.Repo, d))
	}
	rc, err := config.LoadRepo(wt.Repo)
	if err != nil {
		return "", withCode(diag.CodeConfig, err)
	}
	decided := func(res string) {
		rec := editRec(b, wt)
		rec.Event, rec.Attrs = diag.EvGateDecided, map[string]any{"result": res}
		dlog.Log(ctx, rec)
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
			decided(store.CheckSkipped)
			return store.CheckSkipped, s.SetWorktreeCheck(ctx, b.ID, store.CheckSkipped)
		}
	}
	res := store.CheckPass
	if ce.runList(ctx, wt.Path, rc, stdout, editRec(b, wt)) != 0 {
		res = store.CheckFail
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	// The checks passed on the recorded tree only if they left it as it
	// was: a check that writes (gofmt -w, go generate, golden updates)
	// changed what would be applied, so it is recorded as a failure.
	tree, terr := g.Tree(ctx, wt)
	if terr != nil || tree != wt.Tree {
		res = store.CheckFail
	}
	if err := s.SetWorktreeCheck(context.WithoutCancel(ctx), b.ID, res); err != nil {
		return "", withCode(diag.CodeStore, err)
	}
	decided(res)
	if terr != nil {
		return "", fmt.Errorf("after the checks: %w", terr)
	}
	if tree != wt.Tree {
		return "", withCode(diag.CodeChecksChanged, fmt.Errorf("refusing: the checks changed files in the worktree, so it no longer matches the diff the child left; review %s by hand, or undo the changes and run the checks again", wt.Path))
	}
	return res, nil
}

// checkEditWith is aggy check <borrow-id> [--yes]: the check gate on an
// edit's diff, on its own.
func checkEditWith(ctx context.Context, args []string, env borrowEnv, ce repoCheckEnv, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	id, flags, s, stateDir, err := openEdit(args, "--yes")
	if err != nil {
		return editUsage(stderr, "check", diag.EvGateRefused, err, checkUsage)
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
		refuse(ctx, stderr, "check", diag.EvGateRefused, codeOf(err, diag.CodeGateFailed), err.Error(), editRec(b, wt))
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
		return editUsage(stderr, "reject", diag.EvAcceptRefused, err, acceptUsage)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	g := &gitWorktrees{env: env, stateDir: stateDir, out: stderr}
	b, wt, err := editTarget(ctx, s, g, id)
	if err == nil {
		err = discardEdit(ctx, s, g, b.ID, wt, env.now())
	}
	if err != nil {
		refuse(ctx, stderr, "reject", diag.EvAcceptRefused, codeOf(err, diag.CodeAcceptFailed), err.Error(), editRec(b, wt))
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
		return editUsage(stderr, "accept", diag.EvAcceptRefused, err, acceptUsage)
	}
	defer func() { _ = s.Close() }()
	var rec diag.Rec
	fail := func(err error) int {
		refuse(context.Background(), stderr, "accept", diag.EvAcceptRefused, codeOf(err, diag.CodeAcceptFailed), err.Error(), rec)
		return 1
	}
	// Ctrl-C, a kill, or a hangup during the checks kills their process
	// group (execrun.CheckRunner).
	ctx, stop := execrun.NotifyContext(context.Background())
	defer stop()
	yes := flags["--yes"]
	g := &gitWorktrees{env: env, stateDir: stateDir, out: stderr}
	b, wt, err := editTarget(ctx, s, g, id)
	rec = editRec(b, wt)
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
		// The checks ran code outside any sandbox: take the diff again, so
		// the worktree must still match its recorded tree and the main
		// checkout must still be clean in the touched paths.
		if wt.Check == store.CheckPass || wt.Check == store.CheckFail {
			if _, patch, err = g.editDiff(ctx, wt); err != nil {
				return fail(err)
			}
		}
	}
	if wt.Check == store.CheckFail {
		fmt.Fprintln(stdout, "Warning: the repo's checks FAILED on this diff.")
	}
	if !yes {
		declined := rec
		declined.Event = diag.EvAcceptDeclined
		if !tty {
			declined.Attrs = map[string]any{"why": "no terminal and no --yes"}
			dlog.Log(ctx, declined)
			fmt.Fprintln(stdout, "Not applied: run again with --yes, or in a terminal, to apply it.")
			return 1
		}
		if !ask(fmt.Sprintf("Apply this to %s?", termsafe.Field(wt.Repo))) {
			declined.Attrs = map[string]any{"why": "answered no"}
			dlog.Log(ctx, declined)
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
		return fail(withCode(diag.CodeAcceptTarget, err))
	}
	if err := g.apply(ctx, wt, patch); err != nil {
		_ = s.SetWorktreeState(context.WithoutCancel(ctx), b.ID, store.WorktreeAccepted, store.WorktreePending)
		return fail(err)
	}
	applied := rec
	applied.Event, applied.Attrs = diag.EvAcceptApplied, map[string]any{"check": gateText(wt.Check)}
	dlog.Log(ctx, applied)
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
		return "", "", withCode(diag.CodeTreeChanged, fmt.Errorf("refusing: the worktree changed after the borrow ended (a process the child left behind?); review %s by hand", wt.Path))
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
// "" when it is unset or outside the repo, "." when it is the root. A
// leading ~/ is expanded the way git does (HOME), and both paths are
// compared by real path (/tmp against /private/tmp, a symlinked prefix).
// A path it cannot resolve safely (~user, no HOME, an unreadable
// ancestor) is an error, so accept refuses rather than skip the check.
// TODO(T6748): a core.hooksPath set only through the environment
// (GIT_CONFIG_COUNT, GIT_CONFIG_GLOBAL, XDG_CONFIG_HOME) is not seen:
// aggy's git runs with the scrubbed environment.
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
	refuse := func(why string) (string, error) {
		return "", withCode(diag.CodeProtected, fmt.Errorf("refusing: core.hooksPath %s: %s; aggy cannot tell where it points, so the diff cannot be checked against it", val, why))
	}
	p := val
	if p == "~" || strings.HasPrefix(p, "~/") {
		h := ""
		if g.env.getenv != nil {
			h = g.env.getenv("HOME")
		}
		if !filepath.IsAbs(h) {
			return refuse("no HOME")
		}
		p = filepath.Join(h, strings.TrimPrefix(p, "~"))
	} else if strings.HasPrefix(p, "~") {
		return refuse("another user's home")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(repo, p)
	}
	real, err := realPath(p)
	if err != nil {
		return refuse(pathErr(err))
	}
	root, err := realPath(repo)
	if err != nil {
		return refuse("the repo: " + pathErr(err))
	}
	if !within(real, root) {
		return "", nil
	}
	rel, err := filepath.Rel(root, real)
	if err != nil {
		return refuse(err.Error())
	}
	return filepath.ToSlash(rel), nil
}

// pathErr is a path error's cause alone ("not a directory"), or the
// whole error for any other.
func pathErr(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// realPath resolves every symlink in p's deepest existing ancestor and
// joins the rest, which does not exist yet. It errs when an ancestor
// cannot be checked (other than by not existing).
func realPath(p string) (string, error) {
	p = filepath.Clean(p)
	rest := ""
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(r, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) || filepath.Dir(p) == p {
			return "", err
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = filepath.Dir(p)
	}
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
				return nil, withCode(diag.CodeProtected, fmt.Errorf("refusing: the diff makes or changes a symlink or submodule at %s", p))
			}
		}
		inHooks := hooks != "" && ((hooks == "." && !strings.Contains(p, "/")) ||
			strings.EqualFold(p, hooks) || strings.HasPrefix(strings.ToLower(p), strings.ToLower(hooks)+"/"))
		if borrow.ProtectedPath(p) || inHooks {
			return nil, withCode(diag.CodeProtected, fmt.Errorf("refusing: the diff touches %s, which may hold hooks, CI, config, or instructions a tool or CLI runs or loads; review the worktree by hand", p))
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
		return withCode(diag.CodeDirty, fmt.Errorf("refusing: the main checkout has changes in paths the diff touches (%s); commit or stash them first", strings.Join(dirty, ", ")))
	}
	return nil
}

// apply checks and applies patch to the main checkout's work tree with
// git apply, which refuses paths outside the repo and writes through no
// symlink, with every filter driver switched off.
func (g *gitWorktrees) apply(ctx context.Context, wt store.Worktree, patch string) error {
	// Not in $TMPDIR or /tmp: an edit child's sandbox may write there.
	run := filepath.Join(g.stateDir, "run")
	if err := supervisor.PrivateDir(run); err != nil {
		return err
	}
	dir, err := safefs.MkdirTemp(run, "aggy-accept-")
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
