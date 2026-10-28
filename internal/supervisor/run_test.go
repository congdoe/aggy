//go:build unix

package supervisor

import (
	"bytes"
	"errors"
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

// startLimit bounds Run's start: Listen and raw mode took over 5 s on a
// loaded macOS CI runner under -race.
const startLimit = 60 * time.Second

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
func (c *fakeChild) Pid() int        { return os.Getpid() }
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

// openPTY opens a test terminal of 24 rows and 80 columns. On darwin
// the kernel can hand out the index of a pair a test just closed while
// that pair's slave is still being torn down, and opening or sizing it
// then fails with ENXIO ("device not configured"); that is retried for a
// while, as anything else fails at once.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	var err error
	for wait := 5 * time.Millisecond; ; wait *= 2 {
		master, slave, err = pty.Open()
		if err == nil {
			if err = pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
				_ = slave.Close()
				_ = master.Close()
			}
		}
		if err == nil || !errors.Is(err, syscall.ENXIO) || wait > time.Second {
			break
		}
		time.Sleep(wait)
	}
	if err != nil {
		t.Fatal(err)
	}
	return master, slave
}

// startRun runs Run on a test terminal against a fake CLI.
func startRun(t *testing.T, double time.Duration) (*termSide, *fakeChild, <-chan Exit) {
	t.Helper()
	return startRunWith(t, double, nil)
}

// startRunWith is startRun with setup applied to the fake CLI first.
func startRunWith(t *testing.T, double time.Duration, setup func(*fakeChild)) (*termSide, *fakeChild, <-chan Exit) {
	t.Helper()
	return startRunOpts(t, double, setup, nil)
}

// startRunOpts is startRunWith with opts applied to the Options first.
func startRunOpts(t *testing.T, double time.Duration, setup func(*fakeChild), opts func(*Options)) (*termSide, *fakeChild, <-chan Exit) {
	t.Helper()
	master, slave := openPTY(t)
	ts := &termSide{master: master}
	read := make(chan struct{})
	started := make(chan struct{})
	ran := make(chan struct{})
	t.Cleanup(func() {
		// Run reads the slave's Fd before it starts the CLI, and os.File's
		// Fd races with Close: the slave is closed only once Run is past it.
		select {
		case <-started:
		case <-ran:
		case <-time.After(startLimit):
		}
		// The pair is torn down before the next test opens one: the slave
		// first, then the master, whose reader then ends.
		_ = slave.Close()
		_ = master.Close()
		select {
		case <-read:
		case <-time.After(waitLimit):
		}
	})
	go func() {
		defer close(read)
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
	done := make(chan Exit, 1)
	o := Options{
		In: slave, Out: slave,
		Command: adapter.Command{Path: "/fake"},
		Start: func(adapter.Command, int, int) (Child, error) {
			close(started)
			return c, nil
		},
		Vendors:     []adapter.Vendor{adapter.Codex, adapter.Grok},
		DoublePress: double,
		Vendor:      adapter.Claude,
		CanSwitch:   func(adapter.Vendor) error { return nil },
	}
	if opts != nil {
		opts(&o)
	}
	go func() {
		defer close(ran)
		ex, err := Run(o)
		if err != nil {
			t.Error(err)
		}
		done <- ex
	}()
	// Run ends before the test does, even when the test failed before
	// finish: otherwise it reads package vars the cleanups restore and
	// reports after the test (this runs before the cleanups above).
	t.Cleanup(func() {
		select {
		case c.exit <- 0:
		default:
		}
		select {
		case <-ran:
		case <-time.After(waitLimit):
		}
	})
	select {
	case <-started:
	case <-ran:
		t.Fatal("Run ended before it started the CLI")
	case <-time.After(startLimit):
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

func finish(t *testing.T, c *fakeChild, done <-chan Exit) {
	t.Helper()
	c.quit(3)
	select {
	case ex := <-done:
		if ex != (Exit{Code: 3}) {
			t.Errorf("exit %+v, want code 3 and no switch", ex)
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

// TestPickerOverCLIAlternateScreen (B13-3): when the CLI is on the
// alternate screen, the picker must not switch screens (they do not
// nest, so leaving would drop the CLI's screen), and the CLI is made to
// repaint.
func TestPickerOverCLIAlternateScreen(t *testing.T) {
	ts, c, done := startRun(t, 20*time.Millisecond)
	c.print("\x1b[?10")
	poll(t, "the terminal to show", []byte("\x1b[?10"), ts.screen)
	c.print("49hCLI-SCREEN")
	poll(t, "the terminal to show", []byte("CLI-SCREEN"), ts.screen)

	ts.typeIn(t, "\x1c")
	poll(t, "the terminal to show", []byte("aggy: switch vendor"), ts.screen)
	ts.typeIn(t, "\x1b")
	for i, want := range []string{"one row fewer", "back"} {
		select {
		case sz := <-c.sizes:
			if i == 0 && sz[0] != 23 || i == 1 && sz[0] != 24 {
				t.Errorf("resize %d (%s) = %v", i, want, sz)
			}
		case <-time.After(waitLimit):
			t.Fatalf("no repaint resize (%s)", want)
		}
	}
	if got := ts.screen(); bytes.Contains(got, []byte(altScreenOn)) || bytes.Count(got, []byte("\x1b[?1049")) != 1 {
		t.Errorf("the picker switched screens over the CLI's alternate screen: %q", got)
	}

	// Back on the main screen, the picker uses its own alternate screen.
	c.print("\x1b[?1049lMAIN")
	poll(t, "the terminal to show", []byte("MAIN"), ts.screen)
	ts.typeIn(t, "\x1c")
	poll(t, "the terminal to show", []byte(altScreenOn), ts.screen)
	ts.typeIn(t, "\x1b")
	poll(t, "the terminal to show", []byte("MAIN"+altScreenOn), ts.screen)
	finish(t, c, done)
}

// TestPickerWaitsForSplitAltSequence (B13-12): a picker opened while the
// CLI's ESC [ ? 1049 h is split across reads must see the switch.
func TestPickerWaitsForSplitAltSequence(t *testing.T) {
	ts, c, done := startRun(t, 20*time.Millisecond)
	c.print("\x1b[?10")
	poll(t, "the terminal to show", []byte("\x1b[?10"), ts.screen)
	ts.typeIn(t, "\x1c")
	time.Sleep(25 * time.Millisecond) // the press has fired; the tracker is mid-sequence
	c.print("49h")
	poll(t, "the terminal to show", []byte("aggy: switch vendor"), ts.screen)
	if bytes.Contains(ts.screen(), []byte(altScreenOn)) {
		t.Errorf("the picker took the CLI for the main screen: %q", ts.screen())
	}
	ts.typeIn(t, "\x1b")
	finish(t, c, done)
}

// TestExitOnAltScreenLeavesIt (B13-14): a CLI that exits on its
// alternate screen must not leave the terminal there.
func TestExitOnAltScreenLeavesIt(t *testing.T) {
	ts, c, done := startRun(t, time.Second)
	c.print("\x1b[?1049hFULL")
	poll(t, "the terminal to show", []byte("FULL"), ts.screen)
	finish(t, c, done)
	poll(t, "the terminal to show", []byte("FULL"+altScreenOff), ts.screen)
}

// TestQueueDropsUntilDrained (B13-15): after an overflow, later small
// input is dropped too until the CLI catches up, so it never sees a gap.
func TestQueueDropsUntilDrained(t *testing.T) {
	q := newByteQueue(4)
	q.push([]byte("ab"))
	q.push([]byte("cde")) // overflows
	q.push([]byte("f"))   // fits, but would follow a gap
	if b, _ := q.pop(); string(b) != "ab" {
		t.Fatalf("pop %q", b)
	}
	q.push([]byte("g")) // drained: accepted again
	if b, _ := q.pop(); string(b) != "g" {
		t.Errorf("pop %q, want g", b)
	}
}
