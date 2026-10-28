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

// drainWait bounds how long Run waits, after the CLI exits, for its last
// output. A process the CLI left running can hold the terminal open.
const drainWait = 500 * time.Millisecond

// stopDrain is how long a suspend waits for the CLI's last output (its
// own terminal reset) before aggy stops.
//
// shortcut: a fixed wait, not a drain; output that lags more is shown on
// resume instead. Upgrade if the terminal checklist (T6684) shows it.
const stopDrain = 50 * time.Millisecond

// Run supervises one CLI: it puts In in raw mode, starts the CLI at In's
// size, and copies bytes both ways unchanged until the CLI exits. It
// returns the CLI's exit code. The terminal is restored on every return,
// on a panic, and around a suspend.
//
//   - SIGWINCH: In's new size is passed to the CLI.
//   - SIGINT, SIGQUIT, SIGTERM, SIGHUP sent to aggy are forwarded to the
//     CLI, which decides whether to exit. In raw mode the terminal sends
//     none of them itself: Ctrl+C is a byte for the CLI.
//   - Ctrl+Z is a byte for the CLI too. The CLI leads its own session
//     in the pseudo-terminal, so its process group is orphaned and the
//     kernel discards a SIGTSTP it sends itself: a CLI that suspends on
//     Ctrl+Z that way does not stop, as when it runs as a tmux pane's
//     command. When the CLI does stop (SIGSTOP), aggy restores the
//     terminal and stops, so the shell gets it back; on fg it re-enters
//     raw mode, passes on the current size, and resumes the CLI. A
//     SIGTSTP sent to aggy does the same, without stopping the CLI.
//     TODO(T6684): check what each CLI shows on Ctrl+Z under aggy.
func Run(o Options) (int, error) {
	fd := int(o.In.Fd())
	if !term.IsTerminal(fd) {
		return 1, errors.New("supervisor: standard input is not a terminal")
	}
	t := &tty{fd: fd}
	var srv *Server
	if o.RunDir != "" {
		var err error
		srv, err = Listen(o.RunDir, o.ParentID, t.restore, o.OnHook)
		if err != nil {
			return 1, err
		}
		defer func() { _ = srv.Close() }()
		// Hooks and skills inside the CLI reach aggy through these.
		o.Command.Env = SocketEnv(o.Command.Env, srv.Path(), o.ParentID)
	}
	if err := t.raw(); err != nil {
		return 1, err
	}
	defer t.restore()

	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGQUIT,
		syscall.SIGTERM, syscall.SIGHUP, syscall.SIGTSTP)
	defer signal.Stop(sigs)

	rows, cols := t.size()
	child, err := o.Start(o.Command, rows, cols)
	if err != nil {
		return 1, err
	}
	defer func() { _ = child.Close() }()
	if srv != nil {
		srv.SetRoot(child.Pid())
	}

	s := &session{o: o, t: t, child: child}
	outDone := make(chan struct{})
	s.goSafe(func() { s.copyOut(); close(outDone) })
	input := make(chan []byte)
	s.goSafe(func() { s.readIn(input) })
	s.toChild = newByteQueue(maxQueued)
	s.goSafe(s.writeChild)
	defer s.toChild.close()
	waits := make(chan waitResult, 1)
	s.goSafe(func() { s.wait(waits) })

	// B13-14: a CLI that exits on its alternate screen would leave the
	// terminal there. Runs after closePicker (defers run in reverse).
	defer s.leaveAlt()
	defer s.closePicker()

	key := o.SwitchKey
	if key == 0 {
		key = 0x1c
	}
	double := o.DoublePress
	if double <= 0 {
		double = DefaultDoublePress
	}
	filter := newKeyFilter(key)
	s.pk = &picker{vendors: o.Vendors, key: key}
	// pending is a first switch-key press waiting to see whether a second
	// follows within double; it fires when none does.
	var pending <-chan time.Time
	for {
		select {
		case b, ok := <-input:
			if !ok {
				// The terminal is gone: hang up the CLI, as a closed
				// terminal would.
				input = nil
				_ = child.Signal(syscall.SIGHUP)
				continue
			}
			if s.open {
				s.pickerInput(b)
				continue
			}
			for _, ev := range filter.feed(b) {
				switch {
				case s.open:
					s.pickerInput(ev.data)
				case ev.press && pending != nil:
					// A double press: the CLI gets one switch key.
					pending = nil
					s.toChild.push(ev.data)
				case ev.press:
					pending = time.After(double)
				case pending != nil:
					// Other input after one press: the picker opens and
					// takes it, so Ctrl+\ 2 picks the second vendor.
					pending = nil
					s.openPicker()
					s.pickerInput(ev.data)
				default:
					s.toChild.push(ev.data)
				}
			}
		case <-pending:
			pending = nil
			s.openPicker()
		case sig := <-sigs:
			switch sig {
			case syscall.SIGWINCH:
				s.resize()
			case syscall.SIGTSTP:
				s.suspend(false)
			default:
				_ = child.Signal(sig.(syscall.Signal))
			}
		case w := <-waits:
			switch {
			case w.err != nil:
				return 1, w.err
			case w.stopped:
				s.suspend(true)
			default:
				s.closePicker()
				select {
				case <-outDone:
				case <-time.After(drainWait):
				}
				return w.code, nil
			}
		}
	}
}

type waitResult struct {
	stopped bool
	code    int
	err     error
}

type session struct {
	o     Options
	t     *tty
	child Child
	// outMu is held while the CLI's output must not reach the terminal:
	// during a suspend, and while the picker is on screen.
	outMu sync.Mutex
	pk    *picker
	open  bool // the picker is on screen, holding outMu
	// alt follows the CLI's alternate screen; guarded by outMu.
	alt altTracker
	// pickerOverCLI is how the open picker was drawn.
	pickerOverCLI bool
	// toChild holds input for the CLI. The main loop only ever pushes,
	// which never blocks, so a CLI that stops reading cannot stall signal
	// handling, the picker, or the terminal-closed check.
	toChild *byteQueue
}

// altSettle bounds how long openPicker waits for the CLI's output to
// finish an escape sequence split across reads.
//
// shortcut: past it the picker trusts the last whole sequence; a CLI that
// stalls mid-sequence that long is not expected.
const altSettle = 50 * time.Millisecond

// openPicker pauses the CLI's output and draws the picker. Input goes to
// the picker until it closes. It first lets the alternate-screen tracker
// get back between sequences (B13-12): mid ESC [ ? 1049 h, its on is
// about to change.
func (s *session) openPicker() {
	deadline := time.Now().Add(altSettle)
	s.outMu.Lock()
	for !s.alt.idle() && time.Now().Before(deadline) {
		s.outMu.Unlock()
		time.Sleep(2 * time.Millisecond)
		s.outMu.Lock()
	}
	s.open = true
	s.pickerOverCLI = s.alt.on
	s.pk.draw(s.o.Out, s.pickerOverCLI)
}

// leaveAlt takes the terminal off the alternate screen when the CLI left
// it there.
func (s *session) leaveAlt() {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if s.alt.on {
		_, _ = s.o.Out.Write([]byte(altScreenOff))
		s.alt.on = false
	}
}

// closePicker restores the vendor's screen and resumes its output.
func (s *session) closePicker() {
	if !s.open {
		return
	}
	s.pk.erase(s.o.Out, s.pickerOverCLI)
	s.open = false
	s.outMu.Unlock()
	if s.pickerOverCLI {
		s.repaint()
	}
}

// repaintNudge is how long the CLI sees a size one row smaller before
// it gets its own size back.
//
// shortcut: a fixed gap, so a CLI that reads its size only once per
// burst of SIGWINCH still sees a change; upgrade if T6684 shows a CLI
// that does not repaint.
const repaintNudge = 50 * time.Millisecond

// repaint makes the CLI redraw its screen, which the picker drew over:
// a resize to one row fewer and back, each a SIGWINCH. The kernel sends
// none for an unchanged size, so a plain resize would not do.
func (s *session) repaint() {
	rows, cols := s.t.size()
	if rows < 2 || cols < 1 {
		return
	}
	_ = s.child.Resize(rows-1, cols)
	time.AfterFunc(repaintNudge, s.resize)
}

func (s *session) pickerInput(b []byte) {
	done, picked := s.pk.handle(b)
	if !done {
		return
	}
	s.closePicker()
	// TODO(T6664): switch to picked at the CLI's turn end. Until then
	// the picker returns to the session unchanged.
	_ = picked
}

// goSafe runs f in a goroutine that restores the terminal before a panic
// ends the process.
func (s *session) goSafe(f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.t.restore()
				panic(r)
			}
		}()
		f()
	}()
}

func (s *session) copyOut() {
	buf := make([]byte, 32<<10)
	for {
		n, err := s.child.Read(buf)
		if n > 0 {
			s.outMu.Lock()
			s.alt.feed(buf[:n])
			_, werr := s.o.Out.Write(buf[:n])
			s.outMu.Unlock()
			if werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// readIn sends each read from In as a new slice and closes ch when In
// fails.
func (s *session) readIn(ch chan<- []byte) {
	defer close(ch)
	buf := make([]byte, 32<<10)
	for {
		n, err := s.o.In.Read(buf)
		if n > 0 {
			ch <- append([]byte(nil), buf[:n]...)
		}
		if err != nil {
			return
		}
	}
}

// writeChild writes queued input to the CLI, off the main loop.
func (s *session) writeChild() {
	for {
		b, ok := s.toChild.pop()
		if !ok {
			return
		}
		_, _ = s.child.Write(b)
	}
}

func (s *session) wait(ch chan<- waitResult) {
	for {
		stopped, code, err := s.child.Wait()
		ch <- waitResult{stopped, code, err}
		if !stopped || err != nil {
			return
		}
	}
}

func (s *session) resize() {
	rows, cols := s.t.size()
	if rows > 0 && cols > 0 {
		_ = s.child.Resize(rows, cols)
	}
}

// suspend restores the terminal and stops aggy until the shell resumes
// it, then re-enters raw mode, passes on the size (it may have changed),
// and resumes the CLI when it had stopped itself.
func (s *session) suspend(childStopped bool) {
	s.closePicker()
	time.Sleep(stopDrain)
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.t.restore()
	// SIGSTOP cannot be caught, so this stops aggy. On Linux kill can
	// return before the stop takes hold (another thread takes the
	// signal), so wait for the SIGCONT that fg sends, not for kill.
	cont := make(chan os.Signal, 1)
	signal.Notify(cont, syscall.SIGCONT)
	defer signal.Stop(cont)
	_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
	<-cont
	_ = s.t.raw()
	s.resize()
	if childStopped {
		_ = s.child.Continue()
	}
}

// tty is the user's terminal: its saved state and raw mode.
type tty struct {
	fd    int
	mu    sync.Mutex
	saved *term.State
}

func (t *tty) raw() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, err := term.MakeRaw(t.fd)
	if err != nil {
		return err
	}
	if t.saved == nil {
		t.saved = st
	}
	return nil
}

// restore puts the terminal back as it was before the first raw. It may
// run more than once.
func (t *tty) restore() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.saved != nil {
		_ = term.Restore(t.fd, t.saved)
	}
}

// size returns the terminal's rows and columns, or 0, 0.
func (t *tty) size() (rows, cols int) {
	w, h, err := term.GetSize(t.fd)
	if err != nil {
		return 0, 0
	}
	return h, w
}
