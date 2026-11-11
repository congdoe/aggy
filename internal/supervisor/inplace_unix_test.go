//go:build unix

package supervisor

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// TestInPlace (T6981): raw mode for a list drawn in place, its size, a
// resize passed on, and the terminal put back, with the cleanup run once,
// however often Restore is called.
func TestInPlace(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	if err := pty.Setsize(master, &pty.Winsize{Rows: 30, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	cleanups := 0
	resized := make(chan struct{}, 1)
	p, err := StartInPlace(slave, func() { cleanups++ }, func() { resized <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	if rows, cols := p.Size(); rows != 30 || cols != 100 {
		t.Errorf("size %d x %d", rows, cols)
	}
	if now, _ := term.GetState(int(slave.Fd())); *now == *before {
		t.Error("not in raw mode")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	select {
	case <-resized:
	case <-time.After(5 * time.Second):
		t.Error("a resize was not passed on")
	}
	p.Restore()
	p.Restore()
	if cleanups != 1 {
		t.Errorf("cleanup ran %d times", cleanups)
	}
	if now, _ := term.GetState(int(slave.Fd())); *now != *before {
		t.Error("the terminal was not put back")
	}
	f, err := os.CreateTemp(t.TempDir(), "notty")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartInPlace(f, nil, nil); err == nil {
		t.Error("raw mode on a file that is no terminal")
	}
}
