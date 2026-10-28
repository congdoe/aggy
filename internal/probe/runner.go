package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// ErrTimeout matches a run the runner killed at its time limit.
var ErrTimeout = errors.New("probe: command timed out")

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

// extraKey matches the only variables a command's own Env may add: aggy's
// AGGY_* variables (AGGY_DELEGATED, AGGY_VENDOR, AGGY_MODEL, and the test
// fakes' AGGY_FAKE_*). It is an allowlist: everything else is refused,
// including API keys, base URLs, proxies, variables that inject code into
// a CLI's runtime (BASH_ENV, ENV, NODE_OPTIONS, NODE_PATH, PERL5OPT,
// RUBYOPT, PYTHONPATH, GIT_SSH_COMMAND, ELECTRON_RUN_AS_NODE, ...), trust
// stores (SSL_CERT_FILE, NODE_EXTRA_CA_CERTS), and overrides of the
// vendor homes (CLAUDE_CONFIG_DIR, CODEX_HOME, GROK_HOME, XDG_CONFIG_HOME),
// which come only from aggy's own environment.
var extraKey = regexp.MustCompile(`^AGGY_[A-Z0-9_]+$`)

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
// An extra entry whose key is not an AGGY_* variable (see extraKey), or
// that is not KEY=value, is an error.
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
			return nil, fmt.Errorf("probe: bad environment entry %q", kv)
		}
		if !extraKey.MatchString(k) {
			return nil, fmt.Errorf("probe: refusing to pass %s to a vendor CLI (only AGGY_* variables may be added)", k)
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
		if !filepath.IsAbs(c.Path) || filepath.Clean(c.Path) != c.Path {
			return adapter.Output{}, fmt.Errorf("probe: %q is not a clean absolute path", c.Path)
		}
		if c.Stdin != adapter.StdinNull {
			return adapter.Output{}, errors.New("probe: the runner gives commands no stdin")
		}
		if err := safefs.Check(c.Path); err != nil {
			return adapter.Output{}, err
		}
		env, err := Env(os.Environ(), c.Env)
		if err != nil {
			return adapter.Output{}, err
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
		cmd.Stdin = nil // /dev/null
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

// LookPath finds the vendor binary name on aggy's PATH, using only its
// absolute entries (see cleanPath), and returns a clean absolute path for
// ExecRunner. It starts nothing.
func LookPath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("probe: %q is not a bare program name", name)
	}
	for _, dir := range filepath.SplitList(cleanPath(os.Getenv("PATH"))) {
		p := filepath.Join(dir, name)
		fi, err := safefs.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		return filepath.Clean(p), nil
	}
	return "", fmt.Errorf("probe: %s: %w", name, exec.ErrNotFound)
}
