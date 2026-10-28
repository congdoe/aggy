//go:build unix

package supervisor

import (
	"bytes"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/congdoe/aggy/internal/adapter"
)

const waitLimit = 5 * time.Second

// fakeChild is a CLI that records what it is sent and prints what the
// test writes to out.
type fakeChild struct {
	outR *io.PipeReader
	out  *io.PipeWriter
	mu   sync.Mutex
	got  bytes.Buffer
	exit chan int
	// stuck makes Write block until the test ends, like a CLI that has
	// stopped reading.
	stuck chan struct{}
	sigs  chan syscall.Signal // every Signal call
	sizes chan [2]int         // every Resize call
}

func (c *fakeChild) Read(b []byte) (int, error) { return c.outR.Read(b) }
func (c *fakeChild) Write(b []byte) (int, error) {
	if c.stuck != nil {
		<-c.stuck
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got.Write(b)
}
func (c *fakeChild) Close() error { return c.out.Close() }
func (c *fakeChild) Resize(r, cl int) error {
	select {
	case c.sizes <- [2]int{r, cl}:
	default:
	}
	return nil
}
func (c *fakeChild) Continue() error { return nil }
func (c *fakeChild) Signal(sig syscall.Signal) error {
	select {
	case c.sigs <- sig:
	default:
	}
	return nil
}
func (c *fakeChild) Wait() (bool, int, error) { return false, <-c.exit, nil }
func (c *fakeChild) received() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.got.Bytes())
}
func (c *fakeChild) print(s string) { go func() { _, _ = c.out.Write([]byte(s)) }() }
func (c *fakeChild) quit(code int)  { c.exit <- code }
func (c *fakeChild) waitGot(t *testing.T, w []byte) {
	t.Helper()
	poll(t, "the CLI to receive", w, c.received)
}

// term is the user's terminal: the test types on master; Run reads and
// draws on the slave.
type termSide struct {
	master *os.File
	mu     sync.Mutex
	shown  bytes.Buffer
}

func (s *termSide) screen() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.shown.Bytes())
}

func poll(t *testing.T, what string, want []byte, get func() []byte) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		if bytes.Contains(get(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waited for %s %q; have %q", what, want, get())
}

// startRun runs Run on a test terminal against a fake CLI.
func startRun(t *testing.T, double time.Duration) (*termSide, *fakeChild, <-chan int) {
	t.Helper()
	return startRunWith(t, double, nil)
}

// startRunWith is startRun with setup applied to the fake CLI first.
func startRunWith(t *testing.T, double time.Duration, setup func(*fakeChild)) (*termSide, *fakeChild, <-chan int) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	ts := &termSide{master: master}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			ts.mu.Lock()
			ts.shown.Write(buf[:n])
			ts.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	r, w := io.Pipe()
	c := &fakeChild{outR: r, out: w, exit: make(chan int, 1),
		sigs: make(chan syscall.Signal, 16), sizes: make(chan [2]int, 16)}
	if setup != nil {
		setup(c)
	}
	started := make(chan struct{})
	done := make(chan int, 1)
	go func() {
		code, err := Run(Options{
			In: slave, Out: slave,
			Command: adapter.Command{Path: "/fake"},
			Start: func(adapter.Command, int, int) (Child, error) {
				close(started)
				return c, nil
			},
			Vendors:     []adapter.Vendor{adapter.Codex, adapter.Grok},
			DoublePress: double,
		})
		if err != nil {
			t.Error(err)
		}
		done <- code
	}()
	select {
	case <-started:
	case <-time.After(waitLimit):
		t.Fatal("Run did not start the CLI")
	}
	return ts, c, done
}

func (s *termSide) typeIn(t *testing.T, b string) {
	t.Helper()
	if _, err := s.master.Write([]byte(b)); err != nil {
		t.Fatal(err)
	}
}

func finish(t *testing.T, c *fakeChild, done <-chan int) {
	t.Helper()
	c.quit(3)
	select {
	case code := <-done:
		if code != 3 {
			t.Errorf("exit code %d, want 3", code)
		}
	case <-time.After(waitLimit):
		t.Fatal("Run did not return")
	}
}

func TestDoublePressSendsOneKey(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"one read", []string{"\x1c\x1c"}, "a\x1cb"},
		{"two reads", []string{"\x1c", "\x1c"}, "a\x1cb"},
		{"kitty reports", []string{"\x1b[92;5u\x1b[92;5u"}, "a\x1b[92;5ub"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, c, done := startRun(t, time.Second)
			ts.typeIn(t, "a")
			c.waitGot(t, []byte("a"))
			for _, ch := range tt.chunks {
				ts.typeIn(t, ch)
			}
			ts.typeIn(t, "b")
			c.waitGot(t, []byte(tt.want))
			if bytes.Contains(ts.screen(), []byte("switch vendor")) {
				t.Error("a double press opened the picker")
			}
			finish(t, c, done)
		})
	}
}

func TestPasteWithSwitchKeyPassesThrough(t *testing.T) {
	ts, c, done := startRun(t, 50*time.Millisecond)
	paste := "\x1b[200~one\x1ctwo\x1b[92;5uthree\x1b[201~"
	ts.typeIn(t, paste+"!")
	c.waitGot(t, []byte(paste+"!"))
	time.Sleep(100 * time.Millisecond) // longer than the double-press window
	if bytes.Contains(ts.screen(), []byte("switch vendor")) {
		t.Error("a pasted switch key opened the picker")
	}
	finish(t, c, done)
}

func TestPickerOpensPausesAndRestores(t *testing.T) {
	ts, c, done := startRun(t, 50*time.Millisecond)
	c.print("before|")
	poll(t, "the terminal to show", []byte("before|"), ts.screen)

	ts.typeIn(t, "\x1c")
	poll(t, "the terminal to show", []byte(altScreenOn+"aggy: switch vendor"), ts.screen)
	poll(t, "the terminal to show", []byte("1  codex\r\n  2  grok"), ts.screen)

	// The CLI's output waits while the picker is up.
	c.print("during|")
	time.Sleep(100 * time.Millisecond)
	if bytes.Contains(ts.screen(), []byte("during|")) {
		t.Fatal("CLI output reached the screen over the picker")
	}
	// Typing goes to the picker, not the CLI.
	ts.typeIn(t, "x")
	time.Sleep(50 * time.Millisecond)

	ts.typeIn(t, "\x1b")
	poll(t, "the terminal to show", []byte(altScreenOff+"during|"), ts.screen)

	ts.typeIn(t, "after")
	c.waitGot(t, []byte("after"))
	if got := c.received(); !bytes.Equal(got, []byte("after")) {
		t.Errorf("the CLI received %q; the switch key and picker input must not reach it", got)
	}
	finish(t, c, done)
}

func TestPickerTakesInputAfterOnePress(t *testing.T) {
	ts, c, done := startRun(t, time.Second)
	ts.typeIn(t, "\x1c2")
	poll(t, "the terminal to show", []byte(altScreenOff), ts.screen)
	ts.typeIn(t, "z")
	c.waitGot(t, []byte("z"))
	if got := c.received(); !bytes.Equal(got, []byte("z")) {
		t.Errorf("the CLI received %q", got)
	}
	finish(t, c, done)
}

func TestExitWithPickerOpenRestoresScreen(t *testing.T) {
	ts, c, done := startRun(t, 20*time.Millisecond)
	ts.typeIn(t, "\x1c")
	poll(t, "the terminal to show", []byte("switch vendor"), ts.screen)
	finish(t, c, done)
	poll(t, "the terminal to show", []byte(altScreenOff), ts.screen)
}

// TestStalledCLIDoesNotBlockSignals (B13-2): a CLI that stops reading
// must not stall the main loop, so signals still reach it.
func TestStalledCLIDoesNotBlockSignals(t *testing.T) {
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	ts, c, done := startRunWith(t, time.Second, func(c *fakeChild) { c.stuck = stuck })
	// Many separate reads: more than any fixed buffer between the
	// terminal and the stalled CLI.
	for i := 0; i < 200; i++ {
		ts.typeIn(t, "k")
		time.Sleep(time.Millisecond)
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case sig := <-c.sigs:
		if sig != syscall.SIGTERM {
			t.Errorf("the CLI got %v, want SIGTERM", sig)
		}
	case <-time.After(waitLimit):
		t.Fatal("SIGTERM never reached the stalled CLI")
	}
	finish(t, c, done)
}
