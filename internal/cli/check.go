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
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/redact"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/termsafe"
)

const checkUsage = `Usage: aggy check

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

Exit status: 0 when every command passed, 1 when one failed or none is
configured, 2 on a usage or config error.
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
}

func defaultRepoCheckEnv() repoCheckEnv {
	return repoCheckEnv{getwd: os.Getwd, lookPath: probe.LookPath, runner: execrun.ExecRunner, now: time.Now}
}

func runCheck(args []string, stdout, stderr io.Writer) int {
	return defaultRepoCheckEnv().check(context.Background(), args, stdout, stderr)
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
	cwd, err := e.getwd()
	if err != nil {
		fmt.Fprintf(stderr, "aggy check: %s\n", termsafe.Line(err.Error()))
		return 2
	}
	root, ok := repoRoot(cwd)
	if !ok {
		fmt.Fprintf(stderr, "aggy check: %s is in no git work tree\n", termsafe.Field(cwd))
		return 2
	}
	rc, err := config.LoadRepo(root)
	if err != nil {
		fmt.Fprintf(stderr, "aggy check: %s\n", termsafe.Line(err.Error()))
		return 2
	}
	if len(rc.Check) == 0 {
		fmt.Fprintf(stderr, "aggy check: no checks configured; list them under check in %s\n", termsafe.Field(config.RepoPath(root)))
		return 1
	}
	minutes := rc.CheckMinutes
	if minutes == 0 {
		minutes = config.DefaultCheckMinutes
	}
	timeout := time.Duration(minutes) * time.Minute
	fmt.Fprintf(stdout, "aggy check runs these commands from %s, at %s, %d minutes each at most:\n",
		termsafe.Field(config.RepoPath(root)), termsafe.Field(root), minutes)
	for _, c := range rc.Check {
		fmt.Fprintf(stdout, "  %s\n", showCommand(c))
	}
	run := e.runner(timeout)
	failed := 0
	for _, c := range rc.Check {
		start := e.now()
		res, tail := e.checkOne(ctx, run, root, c)
		fmt.Fprintf(stdout, "%-4s %s (%s)\n", map[bool]string{true: "ok", false: "FAIL"}[res == ""], showCommand(c), e.now().Sub(start).Round(100*time.Millisecond))
		if res != "" {
			failed++
			fmt.Fprintf(stdout, "     %s\n", termsafe.Line(res))
			for _, l := range tail {
				fmt.Fprintf(stdout, "     | %s\n", termsafe.Line(l))
			}
		}
		if ctx.Err() != nil {
			break
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
// stderr, with every secret-shaped line removed (redact.Lines). execrun
// already capped each stream.
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
