// Package execrun is the runner that starts vendor CLI processes for
// probes, maintainer captures, and borrow children: ExecRunner runs one
// absolute CLI path with no stdin, a scrubbed environment (see Env), a
// private working directory, capped output, and a time limit, and
// HeadlessRunner does the same for a borrow child, whose stdin may be its
// prompt file and whose working dir the borrow runner made.
//
// It is the only process-starting code outside internal/supervisor and
// internal/borrow, and it is its own package so lint can keep it out of
// the adapters: depguard denies importing it under internal/adapter, and
// allows it only in internal/cli, internal/devfix, and tests, so no other
// package can re-export it to them. Adapters build commands and run them
// only through the adapter.Runner their caller passes.
package execrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// ErrTimeout matches a run the runner killed at its time limit.
var ErrTimeout = errors.New("execrun: command timed out")

// MaxOutput caps each of stdout and stderr; the rest is dropped.
const MaxOutput = 1 << 20

// passEnv lists the variables a probed CLI inherits from aggy. Everything
// else, including API keys and proxy base URLs, is dropped.
var passEnv = []string{
	"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE",
	// The vendor CLIs' own config locations, so a probe sees the same
	// install the user runs.
	"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME",
}

// extraNames are the only variables a command's own Env may add, by
// exact name. It is an allowlist: everything else is refused, including
// API keys, base URLs, proxies, variables that inject code into a CLI's
// runtime (BASH_ENV, ENV, NODE_OPTIONS, NODE_PATH, PERL5OPT, RUBYOPT,
// PYTHONPATH, GIT_SSH_COMMAND, ELECTRON_RUN_AS_NODE, ...), trust stores
// (SSL_CERT_FILE, NODE_EXTRA_CA_CERTS), overrides of the vendor homes
// (CLAUDE_CONFIG_DIR, CODEX_HOME, GROK_HOME, XDG_CONFIG_HOME), which come
// only from aggy's own environment, and every other AGGY_* variable
// (AGGY_HOME among them: a child never gets aggy's state dir).
//
//   - AGGY_DELEGATED=1 marks a borrow child, so the child's own aggy
//     borrow refuses to run (one hop). It is set by aggy for the child,
//     never taken from aggy's own environment (see passEnv).
//   - AGGY_VENDOR and AGGY_MODEL name the vendor and model an
//     interactive session runs, for the linked skill.
var extraNames = []string{"AGGY_DELEGATED", "AGGY_VENDOR", "AGGY_MODEL"}

// extraPrefix is the test fakes' variables (AGGY_FAKE_SCENARIO,
// AGGY_FAKE_RECORD, AGGY_FAKE_VENDOR). The real CLIs ignore them.
const extraPrefix = "AGGY_FAKE_"

// extraAllowed reports whether a command may add the variable k.
func extraAllowed(k string) bool {
	if contains(extraNames, k) {
		return true
	}
	rest, ok := strings.CutPrefix(k, extraPrefix)
	return ok && rest != "" && strings.Trim(rest, "ABCDEFGHIJKLMNOPQRSTUVWXYZ_") == ""
}

// cleanPath drops empty, ".", and relative PATH entries, which would make
// the CLI's own lookups depend on its working directory.
func cleanPath(p string) string {
	var keep []string
	for _, d := range filepath.SplitList(p) {
		if d != "" && filepath.IsAbs(d) {
			keep = append(keep, d)
		}
	}
	return strings.Join(keep, string(filepath.ListSeparator))
}

// Env builds the scrubbed environment for a probed CLI: the passEnv
// variables from base (PATH without empty, ".", or relative entries, and
// left unset when no entry is left), TERM=dumb and NO_COLOR=1, then extra.
// An extra entry whose key is not allowlisted (see extraNames), or that
// is not KEY=value, is an error.
func Env(base, extra []string) ([]string, error) {
	var env []string
	for _, kv := range base {
		k, v, ok := strings.Cut(kv, "=")
		if ok && contains(passEnv, k) {
			if k == "PATH" {
				p := cleanPath(v)
				if p == "" {
					continue
				}
				kv = "PATH=" + p
			}
			env = append(env, kv)
		}
	}
	env = append(env, "TERM=dumb", "NO_COLOR=1")
	for _, kv := range extra {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("execrun: bad environment entry %q", kv)
		}
		if !extraAllowed(k) {
			return nil, fmt.Errorf("execrun: refusing to pass %s to a vendor CLI (only %s and %s* may be added)",
				k, strings.Join(extraNames, ", "), extraPrefix)
		}
		env = append(env, kv)
	}
	return env, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ExecRunner returns a Runner that executes exactly cmd.Path, which must be
// an absolute path: there is no PATH lookup. The process gets no stdin
// (/dev/null), a scrubbed environment (see Env), cmd.Dir or else a private
// empty temp dir (removed afterwards) as its working directory, and at
// most timeout to finish. Past it, the process and its group are killed
// and the error matches ErrTimeout; when the caller's ctx ends first, the
// error is ctx's. A non-zero exit is not an error: it is in
// Output.ExitCode.
func ExecRunner(timeout time.Duration) adapter.Runner {
	return func(ctx context.Context, c adapter.Command) (adapter.Output, error) {
		if c.Stdin != adapter.StdinNull {
			return adapter.Output{}, errors.New("execrun: the runner gives commands no stdin")
		}
		return run(ctx, c, timeout)
	}
}

// HeadlessRunner returns a Runner for borrow children. It is ExecRunner
// except for stdin and the working directory: a command may read a
// regular file as stdin (adapter.StdinFile, the prompt file, so the
// prompt never goes in argv), and it must name its working directory,
// which the borrow runner made for it. StdinPath is opened through
// safefs.OpenRegular: never through a symlink, and never a FIFO or
// device that could block or feed the CLI forever. Past timeout the
// whole process group is killed and the error matches ErrTimeout.
func HeadlessRunner(timeout time.Duration) adapter.Runner {
	return func(ctx context.Context, c adapter.Command) (adapter.Output, error) {
		if c.Stdin != adapter.StdinNull && c.Stdin != adapter.StdinFile {
			return adapter.Output{}, errors.New("execrun: a headless child reads no terminal")
		}
		if c.Dir == "" || !filepath.IsAbs(c.Dir) {
			return adapter.Output{}, fmt.Errorf("execrun: a headless child needs an absolute working dir, got %q", c.Dir)
		}
		return run(ctx, c, timeout)
	}
}

func run(ctx context.Context, c adapter.Command, timeout time.Duration) (adapter.Output, error) {
	if !filepath.IsAbs(c.Path) || filepath.Clean(c.Path) != c.Path {
		return adapter.Output{}, fmt.Errorf("execrun: %q is not a clean absolute path", c.Path)
	}
	if err := safefs.Check(c.Path); err != nil {
		return adapter.Output{}, err
	}
	env, err := Env(os.Environ(), c.Env)
	if err != nil {
		return adapter.Output{}, err
	}
	var stdin *os.File
	if c.Stdin == adapter.StdinFile {
		if !filepath.IsAbs(c.StdinPath) {
			return adapter.Output{}, fmt.Errorf("execrun: stdin file %q is not absolute", c.StdinPath)
		}
		stdin, err = safefs.OpenRegular(c.StdinPath)
		if err != nil {
			return adapter.Output{}, fmt.Errorf("execrun: stdin: %w", err)
		}
		defer func() { _ = stdin.Close() }()
	}
	dir := c.Dir
	if dir == "" {
		tmp, err := safefs.MkdirTemp("", "aggy-probe-")
		if err != nil {
			return adapter.Output{}, err
		}
		defer func() { _ = safefs.RemoveAll(tmp) }()
		dir = tmp
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Env = env
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = stdin // an *os.File: the child reads the descriptor itself
	} else {
		cmd.Stdin = nil // /dev/null
	}
	stdout := &capped{max: MaxOutput}
	stderr := &capped{max: MaxOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	setProcessGroup(cmd)
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	out := adapter.Output{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes(), ExitCode: cmd.ProcessState.ExitCode()}
	if perr := parent.Err(); perr != nil {
		return out, perr
	}
	if ctx.Err() != nil {
		return out, fmt.Errorf("%w: %s after %s", ErrTimeout, filepath.Base(c.Path), timeout)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, nil
	}
	return out, err
}

// capped keeps the first max bytes written and drops the rest.
type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}
