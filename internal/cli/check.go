package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/redact"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/termsafe"
)

const checkUsage = `Usage: aggy check
       aggy check <borrow-id> [--yes]

Runs the repo's checks (typecheck, lint, tests): the commands listed
under check in <repo>/.aggy/config.toml, in order, at the repo root,
for example

  check = [["go", "vet", "./..."], ["make", "lint"], ["go", "test", "./..."]]
  check_minutes = 10   # each command's time limit (default 10, at most 120)

A program is a bare name found on PATH or ./ and a path inside the
repo; nothing goes through a shell. The commands are the repo's own
code: aggy prints them before it runs them, and runs them only when you
ask. Each gets aggy's scrubbed environment (no API keys, no proxy
settings) and no stdin. It prints one line per command and the tail of
a failing command's output, with secret-shaped lines removed.

With a borrow id, it gates an edit borrow's diff: it shows the diff
stat, takes the check list from this main checkout's config (never the
edit worktree's), and runs it in the worktree once you say y (or pass
--yes). The worktree's tests and scripts are code the borrow child
wrote. The result is recorded, and aggy accept shows it. aggy check
refuses to run by hand inside an edit worktree, whose config the child
could have written.

Exit status: 0 when every command passed, 1 when one failed, none is
configured, or a gate was not run, 2 on a usage or config error.
`

// checkTail caps the lines of a failing command's output aggy check
// shows.
const checkTail = 20

// repoCheckEnv is what aggy check needs from the world; tests replace it.
type repoCheckEnv struct {
	getwd    func() (string, error)
	lookPath func(string) (string, error)
	runner   func(time.Duration) adapter.Runner
	now      func() time.Time
	stateDir func() (string, error)
}

func defaultRepoCheckEnv() repoCheckEnv {
	return repoCheckEnv{getwd: os.Getwd, lookPath: probe.LookPath, runner: execrun.CheckRunner, now: time.Now, stateDir: home.Dir}
}

func runCheck(args []string, stdout, stderr io.Writer) int {
	// Ctrl-C, a kill, or a hangup cancels ctx, and the runner then kills
	// the running check's whole process group before aggy exits.
	ctx, stop := execrun.NotifyContext(context.Background())
	defer stop()
	if len(args) > 0 && args[0] != "-h" && args[0] != "--help" && args[0] != "help" {
		return checkEditWith(ctx, args, defaultBorrowEnv(), defaultRepoCheckEnv(), os.Stdin, isTerminal(os.Stdin), stdout, stderr)
	}
	return defaultRepoCheckEnv().check(ctx, args, stdout, stderr)
}

func (e repoCheckEnv) check(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
			fmt.Fprint(stdout, checkUsage)
			return 0
		}
		fmt.Fprint(stderr, checkUsage)
		return 2
	}
	fail := func(code diag.Code, msg string, exit int) int {
		refuse(ctx, stderr, "check", diag.EvCheckRefused, code, msg, diag.Rec{})
		return exit
	}
	cwd, err := e.getwd()
	if err != nil {
		return fail(diag.CodeNotGit, err.Error(), 2)
	}
	root, ok := repoRoot(cwd)
	if !ok {
		return fail(diag.CodeNotGit, cwd+" is in no git work tree", 2)
	}
	// W-5: an edit worktree's config and code are the borrow child's.
	state, err := e.stateDir()
	if err != nil {
		return fail(diag.CodeStore, err.Error(), 2)
	}
	wts := resolvePath(filepath.Join(state, worktreesDir))
	for _, d := range []string{cwd, root} {
		if within(resolvePath(d), wts) {
			return fail(diag.CodeEditWorktree, "refusing: "+root+" is an aggy edit worktree, and its .aggy/config.toml and code were written by a borrow child; run aggy check <borrow-id> from the main checkout, which takes the check list from there and asks first", 2)
		}
	}
	rc, err := config.LoadRepo(root)
	if err != nil {
		return fail(diag.CodeConfig, err.Error(), 2)
	}
	if len(rc.Check) == 0 {
		return fail(diag.CodeNoChecks, "no checks configured; list them under check in "+config.RepoPath(root), 1)
	}
	fmt.Fprintf(stdout, "aggy check runs these commands from %s, at %s, %d minutes each at most:\n",
		termsafe.Field(config.RepoPath(root)), termsafe.Field(root), checkMinutes(rc))
	for _, c := range rc.Check {
		fmt.Fprintf(stdout, "  %s\n", showCommand(c))
	}
	return e.runList(ctx, root, rc, stdout, diag.Rec{})
}

func checkMinutes(rc config.RepoConfig) int {
	if rc.CheckMinutes == 0 {
		return config.DefaultCheckMinutes
	}
	return rc.CheckMinutes
}

// runList runs rc's checks at root, in order, printing a line for each.
// It returns 0 when every one passed and 1 otherwise. ids are the
// diagnostic log's ids (an edit borrow's, for the check gate).
func (e repoCheckEnv) runList(ctx context.Context, root string, rc config.RepoConfig, stdout io.Writer, ids diag.Rec) int {
	run := e.runner(time.Duration(checkMinutes(rc)) * time.Minute)
	failed := 0
	begin := e.now()
	defer func() {
		rec := ids
		rec.Event, rec.Duration = diag.EvCheckFinished, e.now().Sub(begin)
		rec.Attrs = map[string]any{"total": len(rc.Check), "failed": failed, "interrupted": ctx.Err() != nil}
		dlog.Log(ctx, rec)
	}()
	for i, c := range rc.Check {
		start := e.now()
		res, tail := e.checkOne(ctx, run, root, c)
		fmt.Fprintf(stdout, "%-4s %s (%s)\n", map[bool]string{true: "ok", false: "FAIL"}[res == ""], showCommand(c), e.now().Sub(start).Round(100*time.Millisecond))
		if res != "" {
			rec := ids
			// Only the command's place and why it failed: its output and
			// arguments are the repo's, and never stored.
			rec.Event, rec.Code, rec.Msg, rec.Duration = diag.EvCheckCommandFailed, diag.CodeChecksFailed, res, e.now().Sub(start)
			rec.Attrs = map[string]any{"index": i}
			dlog.Log(ctx, rec)
			failed++
			fmt.Fprintf(stdout, "     %s\n", termsafe.Line(res))
			for _, l := range tail {
				fmt.Fprintf(stdout, "     | %s\n", termsafe.Line(l))
			}
		}
		if ctx.Err() != nil {
			fmt.Fprintln(stdout, "aggy check: interrupted")
			return 1
		}
	}
	if failed > 0 {
		fmt.Fprintf(stdout, "aggy check: %d of %d failed\n", failed, len(rc.Check))
		return 1
	}
	fmt.Fprintf(stdout, "aggy check: all %d passed\n", len(rc.Check))
	return 0
}

// checkOne runs c at root. It returns "" when c passed, and otherwise why
// it failed and the redacted tail of its output.
func (e repoCheckEnv) checkOne(ctx context.Context, run adapter.Runner, root string, c config.Command) (string, []string) {
	path, err := e.program(root, c[0])
	if err != nil {
		return "cannot start: " + err.Error(), nil
	}
	out, err := run(ctx, adapter.Command{Path: path, Args: c[1:], Dir: root})
	tail := outputTail(out)
	switch {
	case errors.Is(err, execrun.ErrTimeout):
		return "timed out", tail
	case err != nil:
		return "cannot start: " + err.Error(), tail
	case out.ExitCode != 0:
		return fmt.Sprintf("exit status %d", out.ExitCode), tail
	}
	return "", nil
}

// program resolves a check's program: a bare name on PATH, or ./ and a
// path inside the repo, which must be a regular executable file.
func (e repoCheckEnv) program(root, name string) (string, error) {
	rel, ok := strings.CutPrefix(name, "./")
	if !ok {
		return e.lookPath(name)
	}
	p := filepath.Join(root, rel)
	if !within(resolvePath(p), resolvePath(root)) {
		return "", fmt.Errorf("%s resolves outside the repo", name)
	}
	fi, err := safefs.Stat(p)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not an executable file", name)
	}
	return p, nil
}

// outputTail is the last checkTail lines of a command's stdout then
// stderr, with every secret-shaped line removed (redact.Lines).
// execrun.CheckRunner kept the end of each stream.
func outputTail(out adapter.Output) []string {
	text := strings.TrimRight(string(out.Stdout), "\n")
	if s := strings.TrimRight(string(out.Stderr), "\n"); s != "" {
		if text != "" {
			text += "\n"
		}
		text += s
	}
	text, _ = redact.Lines(text)
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > checkTail {
		lines = lines[len(lines)-checkTail:]
	}
	return lines
}

// showCommand prints a command for the terminal, quoting arguments that
// hold spaces.
func showCommand(c config.Command) string {
	parts := make([]string, len(c))
	for i, a := range c {
		if a == "" || strings.ContainsAny(a, " \t'\"") {
			a = fmt.Sprintf("%q", a)
		}
		parts[i] = termsafe.Field(a)
	}
	return strings.Join(parts, " ")
}
