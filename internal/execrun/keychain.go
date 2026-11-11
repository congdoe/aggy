package execrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/keyring"
	"github.com/congdoe/aggy/internal/safefs"
)

// The keychain tool's runners (aggy key, T6931): macOS's security or Linux's
// secret-tool (keyring.Helper), never a vendor CLI. Like NotifyRunner, each is
// narrow: a fixed absolute path, the argument lists keyring builds (none holds
// a key), and keychainEnv. A key's value is never in an argument, an
// environment variable, an error, or a log line here.

// keychainPass are the variables the tool inherits from aggy: who and where
// the person is, and, for secret-tool, the session bus it reaches the secret
// service over and the display an unlock prompt shows on. No vendor home, API
// key, base URL, or AGGY_* variable but the fakes' (tests) is passed.
var keychainPass = []string{"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE",
	"DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "DISPLAY", "WAYLAND_DISPLAY"}

// KeychainEnv is the keychain tool's whole environment, from base: the
// keychainPass variables (PATH cleaned as Env cleans it) and the fakes'
// AGGY_FAKE_* variables, which the real tools ignore.
func KeychainEnv(base []string) []string {
	var env []string
	for _, kv := range base {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case k == "PATH":
			if p := cleanPath(v); p != "" {
				env = append(env, "PATH="+p)
			}
		case contains(keychainPass, k):
			env = append(env, kv)
		case extraAllowed(k) && strings.HasPrefix(k, extraPrefix):
			env = append(env, kv)
		}
	}
	return env
}

// keychainCommand checks path and args and builds the command. args are
// keyring's: each is checked to be short and free of control characters, so
// a mistake cannot slip a line into the tool.
func keychainCommand(ctx context.Context, path string, args []string) (*exec.Cmd, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("execrun: the keychain tool %q is not a clean absolute path", path)
	}
	if err := safefs.Check(path); err != nil {
		return nil, err
	}
	for _, a := range args {
		if len(a) > 256 || strings.ContainsFunc(a, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return nil, errors.New("execrun: a keychain tool argument is too long or holds a control character")
		}
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = KeychainEnv(os.Environ())
	cmd.Dir = "/"
	return cmd, nil
}

// exitOf is the command's exit status, or err when it did not run to an exit.
func exitOf(ctx context.Context, cmd *exec.Cmd, err error, timeout time.Duration) (int, error) {
	if ctx.Err() != nil {
		return -1, fmt.Errorf("%w: the keychain tool after %s", ErrTimeout, timeout)
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return -1, err
	}
	if cmd.ProcessState == nil {
		return -1, errors.New("execrun: the keychain tool did not run")
	}
	return cmd.ProcessState.ExitCode(), nil
}

// KeychainPrompt runs the tool to store a key, on the person's terminal: tty
// is its stdin, and it asks for the value itself, with echo off, so the value
// goes from the keyboard to the tool and never through aggy. The caller has
// checked tty is a terminal (a pipe would give secret-tool the value with no
// person there). It stays in aggy's process group, the terminal's foreground
// group, so it can read the terminal; past timeout it is killed. Its stdout is
// dropped and its stderr is the person's terminal.
func KeychainPrompt(ctx context.Context, path string, args []string, tty *os.File, timeout time.Duration) (int, error) {
	if tty == nil {
		return -1, errors.New("execrun: the keychain tool needs the terminal")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, err := keychainCommand(ctx, path, args)
	if err != nil {
		return -1, err
	}
	cmd.Stdin = tty
	cmd.Stdout = nil // /dev/null
	cmd.Stderr = os.Stderr
	return exitOf(ctx, cmd, cmd.Run(), timeout)
}

// KeychainRun runs the tool with no stdin and every output dropped: a
// presence check or a removal. Only its exit status comes back.
func KeychainRun(ctx context.Context, path string, args []string, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, err := keychainCommand(ctx, path, args)
	if err != nil {
		return -1, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	return exitOf(ctx, cmd, reap(cmd), timeout)
}

// KeychainRead runs the tool to print one key and returns it as a Value, with
// the exit status. stdout goes straight into one fixed buffer through a pipe
// aggy reads itself (no copying goroutine with buffers of its own), which is
// the Value's backing array; stderr is dropped, since aggy never shows the
// tool's text. A non-zero exit gives no Value, and the buffer is zeroed. On
// macOS the read shows the person a dialog (the item trusts no program), so
// timeout must leave time to answer it.
func KeychainRead(ctx context.Context, path string, args []string, timeout time.Duration) (*keyring.Value, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, err := keychainCommand(ctx, path, args)
	if err != nil {
		return nil, -1, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, -1, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, w, nil
	setProcessGroup(cmd)
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		return nil, -1, err
	}
	// One byte over the cap tells a value that is too long; two more for a
	// trailing CRLF.
	buf := make([]byte, keyring.MaxValue+3)
	n, over := 0, false
	read := make(chan struct{})
	go func() {
		defer close(read)
		for {
			if n == len(buf) {
				// Drain and drop the rest so the tool can exit.
				var sink [512]byte
				for {
					m, err := r.Read(sink[:])
					clear(sink[:m])
					if m > 0 {
						over = true
					}
					if err != nil {
						return
					}
				}
			}
			m, err := r.Read(buf[n:])
			n += m
			if err != nil {
				return
			}
		}
	}()
	werr := reap(cmd)
	select {
	case <-read:
	case <-time.After(time.Second):
		// A process that escaped the group holds the pipe open.
		_ = r.Close()
		<-read
	}
	_ = r.Close()
	code, err := exitOf(ctx, cmd, werr, timeout)
	if err != nil || code != 0 || over || n == len(buf) {
		clear(buf)
		if err == nil && code == 0 {
			err = errors.New("the stored key is too long")
		}
		return nil, code, err
	}
	v, err := keyring.Parse(buf[:n])
	if err != nil {
		clear(buf)
		return nil, code, err
	}
	return v, code, nil
}
