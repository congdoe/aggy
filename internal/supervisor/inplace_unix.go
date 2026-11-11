//go:build unix

package supervisor

import (
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// InPlace is aggy's terminal in raw mode for a list drawn in place on the
// normal screen before any CLI starts (T6981: the trust list at a first
// start). Raw mode lives here, as the session's does, since only the
// supervisor may set it. It reads nothing itself: the caller reads its own
// stdin, so no reader is left behind to take keys meant for the CLI.
//
// Every way out puts the terminal back: Restore (which the caller defers, so
// a panic restores too), and a signal that would end aggy (SIGTERM, SIGHUP,
// SIGINT, SIGQUIT), after which the signal is raised again with its default
// action. Ctrl-C is a byte in raw mode, so the caller reads it as a key and
// calls Interrupt once it has restored.
type InPlace struct {
	fd      int
	saved   *term.State
	sigs    chan os.Signal
	done    chan struct{}
	once    sync.Once
	cleanup func()
}

// StartInPlace puts in in raw mode. cleanup, when set, runs once just before
// the terminal is restored, however that comes (it takes the drawing off and
// shows the cursor); resized, when set, runs on SIGWINCH, off the caller's
// goroutine.
func StartInPlace(in *os.File, cleanup, resized func()) (*InPlace, error) {
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		return nil, errors.New("supervisor: not a terminal")
	}
	st, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	p := &InPlace{fd: fd, saved: st, sigs: make(chan os.Signal, 4), done: make(chan struct{}), cleanup: cleanup}
	signal.Notify(p.sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGWINCH)
	go p.watch(resized)
	return p, nil
}

// watch takes the signals while the list is up.
func (p *InPlace) watch(resized func()) {
	for {
		select {
		case <-p.done:
			return
		case sig := <-p.sigs:
			if sig == syscall.SIGWINCH {
				if resized != nil {
					resized()
				}
				continue
			}
			p.Restore()
			raise(sig.(syscall.Signal))
			return
		}
	}
}

// Size is the terminal's rows and columns, or 0, 0.
func (p *InPlace) Size() (rows, cols int) {
	w, h, err := term.GetSize(p.fd)
	if err != nil {
		return 0, 0
	}
	return h, w
}

// Restore runs the cleanup and puts the terminal back as it was. It may run
// more than once, and from any goroutine.
func (p *InPlace) Restore() {
	p.once.Do(func() {
		signal.Stop(p.sigs)
		close(p.done)
		if p.cleanup != nil {
			p.cleanup()
		}
		_ = term.Restore(p.fd, p.saved)
	})
}

// Interrupt ends aggy as Ctrl-C at a typed question did: SIGINT with its
// default action. The caller restores the terminal first.
func Interrupt() { raise(syscall.SIGINT) }

// raise sends sig to aggy itself with its default action, and exits as a
// shell reports such a death should the signal not end it.
func raise(sig syscall.Signal) {
	signal.Reset(sig)
	_ = syscall.Kill(os.Getpid(), sig)
	time.Sleep(time.Second)
	os.Exit(128 + int(sig))
}
