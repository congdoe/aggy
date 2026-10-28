//go:build unix

package execrun

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/creack/pty"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// PTY is an interactive vendor CLI running in a pseudo-terminal, started
// by StartPTY. Read and Write go to the terminal's master side.
type PTY struct {
	*os.File // the master side
	pid      int
	proc     *os.Process
}

// StartPTY starts c in a new pseudo-terminal of rows by cols, as the
// leader of a new session with the terminal as its controlling terminal.
// c.Path must be a clean absolute path and c.Stdin StdinTerminal.
//
// The CLI is the user's own on-screen session, so unlike a probe it gets
// aggy's whole environment (see PTYEnv), plus c.Env, which may hold only
// the variables in ptyExtraNames.
func StartPTY(c adapter.Command, rows, cols int) (*PTY, error) {
	if !filepath.IsAbs(c.Path) || filepath.Clean(c.Path) != c.Path {
		return nil, fmt.Errorf("execrun: %q is not a clean absolute path", c.Path)
	}
	if c.Stdin != adapter.StdinTerminal {
		return nil, errors.New("execrun: StartPTY runs interactive commands only")
	}
	if c.Dir == "" || !filepath.IsAbs(c.Dir) {
		return nil, fmt.Errorf("execrun: working directory %q is not absolute", c.Dir)
	}
	if err := safefs.Check(c.Path); err != nil {
		return nil, err
	}
	env, err := PTYEnv(os.Environ(), c.Env)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env = env
	cmd.Dir = c.Dir
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: clampSize(rows), Cols: clampSize(cols)})
	if err != nil {
		return nil, err
	}
	return &PTY{File: f, pid: cmd.Process.Pid, proc: cmd.Process}, nil
}

// ptyExtraNames are the only variables StartPTY may add to the user's
// session. It is its own list, apart from the borrow allowlist (Env):
// an on-screen CLI is never marked AGGY_DELEGATED, and a borrow never
// gets the socket.
var ptyExtraNames = []string{"AGGY_VENDOR", "AGGY_SOCKET", "AGGY_PARENT"}

// PTYEnv is base, without any variable extra sets, followed by extra. An
// extra entry must be KEY=value with a key in ptyExtraNames: aggy never
// adds API keys, base URLs, or anything else to the user's session.
func PTYEnv(base, extra []string) ([]string, error) {
	set := map[string]bool{}
	for _, kv := range extra {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("execrun: bad environment entry %q", kv)
		}
		if !contains(ptyExtraNames, k) {
			return nil, fmt.Errorf("execrun: refusing to pass %s to an interactive CLI (only %s may be added)", k, strings.Join(ptyExtraNames, ", "))
		}
		set[k] = true
	}
	env := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); !set[k] {
			env = append(env, kv)
		}
	}
	return append(env, extra...), nil
}

func clampSize(n int) uint16 {
	if n <= 0 || n > 0xffff {
		return 0
	}
	return uint16(n)
}

// Pid returns the CLI's process id, which is also its process group id.
func (p *PTY) Pid() int { return p.pid }

// Resize sets the terminal size; the kernel sends the CLI SIGWINCH.
func (p *PTY) Resize(rows, cols int) error {
	return pty.Setsize(p.File, &pty.Winsize{Rows: clampSize(rows), Cols: clampSize(cols)})
}

// Wait blocks until the CLI stops or exits. A stop (the CLI suspended
// itself, as on Ctrl+Z) returns stopped true; call Wait again after
// Continue. An exit returns the exit code, or 128 plus the signal number
// when a signal ended it. Wait reaps the process itself, so it can see
// stops, which exec.Cmd.Wait cannot.
func (p *PTY) Wait() (stopped bool, code int, err error) {
	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(p.pid, &ws, syscall.WUNTRACED, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return false, -1, err
		}
		switch {
		case isStopped(ws):
			return true, 0, nil
		case ws.Exited():
			_ = p.proc.Release()
			return false, ws.ExitStatus(), nil
		case ws.Signaled():
			_ = p.proc.Release()
			return false, 128 + int(ws.Signal()), nil
		}
	}
}

// isStopped is WIFSTOPPED. ws.Stopped is false for SIGSTOP on BSD and
// darwin, so it cannot be used here.
func isStopped(ws syscall.WaitStatus) bool { return ws&0xff == 0x7f }

// Signal sends sig to the CLI's process group.
func (p *PTY) Signal(sig syscall.Signal) error { return syscall.Kill(-p.pid, sig) }

// Continue resumes a stopped CLI's process group.
func (p *PTY) Continue() error { return p.Signal(syscall.SIGCONT) }
