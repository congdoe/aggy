//go:build unix

package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/congdoe/aggy/internal/adapter"
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

// switchQuiet is how long the CLI's output and the user's input must have
// been quiet, after a turn end, before a switch is offered.
//
// shortcut: a running turn shows a spinner or streams text in all three
// CLIs as seen in use, not checked against their source; the user's
// confirmation is the real guard. Upgrade if T6684 shows an idle CLI that
// keeps drawing. Tests shorten it.
var switchQuiet = time.Second

// switchPoll is how often a waiting switch checks whether it may go on.
const switchPoll = 100 * time.Millisecond

// exitWait bounds how long a CLI has to exit after aggy types its exit
// command. Past it, aggy leaves the CLI running and drops the switch.
// Tests shorten it.
var exitWait = 10 * time.Second

// Run supervises one CLI: it puts In in raw mode, starts the CLI at In's
// size, and copies bytes both ways unchanged until the CLI exits. It
// returns the CLI's exit code, and the vendor to launch next when it
// exited for a switch. The terminal is restored on every return, on a
// panic, and around a suspend.
//
// A switch (T6664) is picked in the picker or asked for by the agent (a
// switch-request from the session). It never interrupts a turn: it waits
// for the vendor's turn-end hook, from the CLI's own process tree
// (FromSession), with no Enter typed since, and for the CLI's output and
// the user's typing to go quiet. A marked hook is still only a hint (a
// nested CLI's Stop can arrive mid-turn), so aggy then asks the user, and
// only on Enter types the exit command (adapter.ExitCommand) through the
// input queue. A CLI that has not exited within exitWait is left running
// and the switch dropped, with a note.
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
func Run(o Options) (Exit, error) {
	fd := int(o.In.Fd())
	if !term.IsTerminal(fd) {
		return Exit{Code: 1}, errors.New("supervisor: standard input is not a terminal")
	}
	t := &tty{fd: fd}
	s := &session{o: o, t: t, atPrompt: !o.StartsTurn, turnEnds: make(chan struct{}, 1), asks: make(chan adapter.Vendor, 1)}
	var srv *Server
	if o.RunDir != "" {
		var err error
		srv, err = Listen(o.RunDir, o.ParentID, t.restore, s.onHook)
		if err != nil {
			return Exit{Code: 1}, err
		}
		defer func() { _ = srv.Close() }()
		if o.CanSwitch != nil {
			srv.SetSwitch(s.onSwitch)
		}
		// Hooks and skills inside the CLI reach aggy through these.
		o.Command.Env = SocketEnv(o.Command.Env, srv.Path(), o.ParentID)
	}
	if err := t.raw(); err != nil {
		return Exit{Code: 1}, err
	}
	defer t.restore()

	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGQUIT,
		syscall.SIGTERM, syscall.SIGHUP, syscall.SIGTSTP)
	defer signal.Stop(sigs)

	rows, cols := t.size()
	child, err := o.Start(o.Command, rows, cols)
	if err != nil {
		return Exit{Code: 1}, err
	}
	defer func() { _ = child.Close() }()
	if srv != nil {
		srv.SetRoot(child.Pid())
	}
	s.child = child
	outDone := make(chan struct{})
	s.goSafe(func() { s.copyOut(); close(outDone) })
	input := inputOf(o.In)
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
		// check polls a switch that waits for the turn end.
		var check <-chan time.Time
		if s.sw.to != "" && !s.sw.typed && !s.open {
			check = time.After(switchPoll)
		}
		select {
		case <-s.turnEnds:
			s.atPrompt = true
		case v := <-s.asks:
			if s.sw.to == "" { // a switch the user picked wins
				s.setSwitch(pendingSwitch{to: v, asked: true})
			}
		case <-check:
			s.offerSwitch()
		case <-s.exitBy:
			s.exitBy = nil
			if s.sw.typed {
				v := s.o.Vendor
				s.setSwitch(pendingSwitch{})
				s.pk.tell(fmt.Sprintf("%s did not exit after aggy typed its exit command, so it is still running and aggy did not switch.", v))
				s.openPicker()
			}
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
					s.send(ev.data)
				case ev.press:
					pending = time.After(double)
				case pending != nil:
					// Other input after one press: the picker opens and
					// takes it, so Ctrl+\ 2 picks the second vendor.
					pending = nil
					s.openList()
					s.pickerInput(ev.data)
				default:
					s.send(ev.data)
				}
			}
		case <-pending:
			pending = nil
			s.openList()
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
				return Exit{Code: 1}, w.err
			case w.stopped:
				s.suspend(true)
			default:
				s.closePicker()
				select {
				case <-outDone:
				case <-time.After(drainWait):
				}
				// Only an exit after aggy typed the exit command is a
				// switch; a CLI the user quit ends aggy.
				ex := Exit{Code: w.code}
				if s.sw.typed {
					ex.SwitchTo = s.sw.to
				}
				return ex, nil
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

	// The switch state, owned by the main loop. turnEnds and asks bring
	// it hook turn ends and the agent's switch requests from the socket.
	sw       pendingSwitch
	turnEnds chan struct{}
	asks     chan adapter.Vendor
	// atPrompt: a turn end from the session came after the last Enter the
	// user typed (or none was typed since a launch that started no turn).
	atPrompt bool
	// dirty: the user typed something after their last Enter, which the
	// CLI may still hold unsent.
	dirty bool
	// lastIn is when the user last typed to the CLI; lastOut (unix ns),
	// written by copyOut, when the CLI last drew.
	lastIn  time.Time
	lastOut atomic.Int64
	// exitBy fires exitWait after the exit command was typed.
	exitBy <-chan time.Time
	// waiting mirrors sw.to != "" for the socket's goroutines, so a
	// second switch-request is refused rather than dropped.
	waiting atomic.Bool
}

// setSwitch sets the switch state.
func (s *session) setSwitch(p pendingSwitch) {
	s.sw = p
	s.waiting.Store(p.to != "")
}

// pendingSwitch is a switch waiting for the turn end.
type pendingSwitch struct {
	to    adapter.Vendor
	asked bool // the agent asked for it (a skill), not the user
	typed bool // the exit command has been typed
}

// onHook is the socket's hook callback: it passes the event on, and a
// turn end of the vendor on screen from the session tells the main loop.
// It runs on a connection's goroutine.
func (s *session) onHook(r Request) {
	if s.o.OnHook != nil {
		s.o.OnHook(r)
	}
	if r.FromSession && r.Vendor == string(s.o.Vendor) && turnEnd(r.Event) {
		select {
		case s.turnEnds <- struct{}{}:
		default:
		}
	}
}

// onSwitch takes a switch-request from the session (the socket refuses
// any other): the vendor must be one the picker offers and CanSwitch
// allows. It queues it for the main loop, which asks the user at the
// turn end. It runs on a connection's goroutine.
func (s *session) onSwitch(r Request) error {
	v := adapter.Vendor(r.Vendor)
	if err := s.offered(v); err != nil {
		return err
	}
	if !s.waiting.CompareAndSwap(false, true) {
		return errors.New("a switch is already waiting")
	}
	s.asks <- v // the only one past the swap; the channel holds one
	return nil
}

// offered says why v cannot be switched to, or nil.
func (s *session) offered(v adapter.Vendor) error {
	if v == s.o.Vendor {
		return fmt.Errorf("%s is already on screen", v)
	}
	ok := false
	for _, o := range s.o.Vendors {
		ok = ok || o == v
	}
	if !ok {
		return fmt.Errorf("%s is not offered here (allowed_vendors, switch.order, or aggy setup found it signed out)", v)
	}
	if s.o.CanSwitch == nil {
		return errors.New("this session cannot switch")
	}
	return s.o.CanSwitch(v)
}

// send queues the user's input for the CLI and notes what it may start:
// an Enter may start a turn, and text after the last Enter may be unsent.
func (s *session) send(b []byte) {
	s.lastIn = time.Now()
	if i := bytes.LastIndexByte(b, '\r'); i >= 0 {
		s.atPrompt = false
		s.dirty = i < len(b)-1
	} else {
		s.dirty = true
	}
	s.toChild.push(b)
}

// offerSwitch asks the user to confirm the waiting switch once the CLI
// is at its prompt and quiet. The vendor is checked again first: the
// repo config or PATH may have changed since the pick.
func (s *session) offerSwitch() {
	now := time.Now()
	if !s.atPrompt || now.Sub(s.lastIn) < switchQuiet || now.Sub(time.Unix(0, s.lastOut.Load())) < switchQuiet {
		return
	}
	v, on := s.sw.to, s.o.Vendor
	if err := s.offered(v); err != nil {
		s.setSwitch(pendingSwitch{})
		s.pk.tell(fmt.Sprintf("Cannot switch to %s: %v", v, err))
		s.openPicker()
		return
	}
	var lines []string
	if s.sw.asked {
		lines = append(lines, fmt.Sprintf("The agent asked to switch to %s.", v))
	}
	cmd, verified := adapter.ExitCommand(on)
	lines = append(lines, fmt.Sprintf("%s's turn seems to have ended. aggy will type %s into %s, then start %s.", on, cmd, on, v))
	if !verified {
		lines = append(lines, fmt.Sprintf("(%s is not yet verified to end %s.)", cmd, on))
	}
	if s.dirty {
		lines = append(lines, "You typed text you have not sent; it would go before "+cmd+". Esc, clear it, and switch again.")
	}
	lines = append(lines, "If a turn is still running, press Esc.")
	s.pk.ask(v, lines...)
	s.openPicker()
}

// typeExit types the on-screen CLI's exit command, through the input
// queue, after anything the user typed before it.
func (s *session) typeExit() {
	cmd, _ := adapter.ExitCommand(s.o.Vendor)
	s.toChild.push([]byte(cmd + "\r"))
	s.sw.typed = true
	s.exitBy = time.After(exitWait)
}

// altSettle bounds how long openPicker waits for the CLI's output to
// finish an escape sequence split across reads.
//
// shortcut: past it the picker trusts the last whole sequence; a CLI that
// stalls mid-sequence that long is not expected.
const altSettle = 50 * time.Millisecond

// openList opens the picker on the vendor list.
func (s *session) openList() {
	s.pk.list()
	s.pk.waiting = ""
	if !s.sw.typed {
		s.pk.waiting = s.sw.to
	}
	s.openPicker()
}

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
	confirming, note := s.pk.confirm != "", s.pk.note
	done, picked := s.pk.handle(b)
	if !done {
		return
	}
	s.closePicker()
	switch {
	case note || s.sw.typed:
	case confirming && picked != "":
		s.typeExit()
	case confirming: // Esc: stay
		s.setSwitch(pendingSwitch{})
	case picked != "" && picked == s.sw.to:
		// Picked again while it waits: the user says no turn is running
		// (an Enter that started none leaves no turn end to wait for).
		s.atPrompt = true
	case picked != "":
		if err := s.offered(picked); err != nil {
			s.pk.tell(fmt.Sprintf("Cannot switch to %s: %v", picked, err))
			s.openPicker()
			return
		}
		s.setSwitch(pendingSwitch{to: picked})
	default: // Esc on the list cancels a waiting switch
		s.setSwitch(pendingSwitch{})
	}
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
			s.lastOut.Store(time.Now().UnixNano())
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

// readers holds one reader per input file, kept across Runs: a read on
// a terminal cannot be cancelled, so a reader per Run would outlive it
// and take the next CLI's first keys after a switch.
var (
	readersMu sync.Mutex
	readers   = map[*os.File]chan []byte{}
)

// inputOf returns in's reader: each read from in as a new slice, closed
// when in fails.
func inputOf(in *os.File) <-chan []byte {
	readersMu.Lock()
	defer readersMu.Unlock()
	if ch, ok := readers[in]; ok {
		return ch
	}
	ch := make(chan []byte)
	readers[in] = ch
	go func() {
		defer close(ch)
		buf := make([]byte, 32<<10)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				ch <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
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
