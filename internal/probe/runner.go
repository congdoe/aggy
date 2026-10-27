package probe

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

// forbiddenEnv reports whether a variable must never reach a vendor CLI
// from aggy: API keys and base URLs that would bill an API key or proxy
// plan traffic.
func forbiddenEnv(key string) bool {
	k := strings.ToUpper(key)
	return strings.HasSuffix(k, "_API_KEY") || strings.HasSuffix(k, "_BASE_URL")
}

// Env builds the scrubbed environment for a probed CLI: the passEnv
// variables from base, TERM=dumb and NO_COLOR=1, then extra. An extra entry
// that names an API key or base URL, or is not KEY=value, is an error.
func Env(base, extra []string) ([]string, error) {
	var env []string
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if ok && contains(passEnv, k) {
			env = append(env, kv)
		}
	}
	env = append(env, "TERM=dumb", "NO_COLOR=1")
	for _, kv := range extra {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("probe: bad environment entry %q", kv)
		}
		if forbiddenEnv(k) {
			return nil, fmt.Errorf("probe: refusing to pass %s to a vendor CLI", k)
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
// (/dev/null), a scrubbed environment (see Env), cmd.Dir or else the
// system temp dir as its working directory, and at most timeout to finish. Past
// it, the process and its group are killed and the error matches
// ErrTimeout. A non-zero exit is not an error: it is in Output.ExitCode.
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
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, c.Path, c.Args...)
		cmd.Env = env
		cmd.Dir = c.Dir
		if cmd.Dir == "" {
			cmd.Dir = os.TempDir()
		}
		cmd.Stdin = nil // /dev/null
		stdout := &capped{max: MaxOutput}
		stderr := &capped{max: MaxOutput}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		setProcessGroup(cmd)
		cmd.WaitDelay = time.Second
		err = cmd.Run()
		out := adapter.Output{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes(), ExitCode: cmd.ProcessState.ExitCode()}
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
