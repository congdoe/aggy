//go:build unix

package execrun

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// RunDirect runs c with StartDirect and returns its exit code (see
// Direct.Wait).
//
// The terminal sends Ctrl+C and Ctrl+\ to the whole foreground group,
// the CLI included, which decides what they mean; aggy must outlive the
// CLI to clean up, so it only catches them. A TERM or HUP sent to aggy
// alone is passed on. Caught, not ignored: an ignored signal would stay
// ignored in the CLI.
func RunDirect(c adapter.Command) (int, error) {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	d, err := StartDirect(c)
	if err != nil {
		return 1, err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case s := <-sigs:
				if s == syscall.SIGTERM || s == syscall.SIGHUP {
					_ = d.cmd.Process.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()
	return d.Wait()
}

// Direct is an interactive vendor CLI started by StartDirect: on aggy's
// own terminal, not in a pseudo-terminal.
type Direct struct{ cmd *exec.Cmd }

// StartDirect starts c on aggy's own stdin, stdout, and stderr, in
// aggy's process group, so the terminal treats it as the foreground job
// (Ctrl+C, Ctrl+Z, and resizes reach it directly). This is aggy --direct,
// for when the pass-through breaks a terminal feature. The checks and
// environment rules are StartPTY's: c.Path a clean absolute path,
// c.Stdin StdinTerminal, and only ptyExtraNames added to the user's
// environment.
func StartDirect(c adapter.Command) (*Direct, error) {
	if !filepath.IsAbs(c.Path) || filepath.Clean(c.Path) != c.Path {
		return nil, fmt.Errorf("execrun: %q is not a clean absolute path", c.Path)
	}
	if c.Stdin != adapter.StdinTerminal {
		return nil, errors.New("execrun: StartDirect runs interactive commands only")
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
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Direct{cmd: cmd}, nil
}

// Wait waits for the CLI to exit and returns its exit code, or 128 plus
// the signal number when a signal ended it.
func (d *Direct) Wait() (int, error) {
	err := d.cmd.Wait()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return 1, err
	}
	if ws, ok := d.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return d.cmd.ProcessState.ExitCode(), nil
}
