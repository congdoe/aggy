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
	"github.com/congdoe/aggy/internal/hook"
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

// exitWait is how long a CLI has to exit after aggy types its exit
// command before aggy tells the user it has not. The switch still stands
// (S-4): a CLI that exits later, say after a turn it had queued the exit
// behind, is switched from, unless the user cancels in the picker. Tests
// shorten it.
var exitWait = 10 * time.Second

// confirmGuard is how long a confirmation aggy opened on its own ignores
// the switch key, so a key meant for the CLI as it appears cannot
// confirm (S-2). Tests shorten it.
var confirmGuard = 500 * time.Millisecond

// escTimeout is how long a lone Esc at a prompt aggy opened waits for
// more bytes before it counts as Esc: a key's escape sequence can arrive
// split across reads (S-11). Tests shorten it.
var escTimeout = 50 * time.Millisecond

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
// the user's typing to go quiet. Any other hook from the session, such as
// a Notification (a permission prompt, among others), holds the switch
// until the user has typed to the CLI and a turn has ended since. A
// marked hook is still only a hint (a nested CLI's Stop can arrive
// mid-turn), so aggy then asks the user, and only the switch key, not
// Enter and not within confirmGuard, types the exit command
// (adapter.ExitCommand) through the input queue. Text the user typed and
// did not send blocks the switch outright. Any other key at a prompt aggy
// opened on its own closes it and goes to the CLI, so nothing typed is
// lost. A CLI that has not exited within exitWait is left running, and the
// user told; the switch stands until it exits or the user cancels.
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
	s := &session{o: o, t: t, atPrompt: !o.StartsTurn, events: make(chan hookEvent, 16), asks: make(chan Choice, 1), quit: make(chan struct{})}
	defer close(s.quit)
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
	s.pk = &picker{vendors: o.Vendors, key: key, children: o.Children}
	// pending is a first switch-key press waiting to see whether a second
	// follows within double; it fires when none does.
	var pending <-chan time.Time
	// input takes the user's keys while no picker is up, and the keys a
	// prompt aggy opened hands back (S-11): through the one key filter,
	// so its paste and split-sequence state stays right.
	s.input = func(b []byte) {
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
	}
	for {
		// check polls a switch that waits for the turn end.
		var check <-chan time.Time
		if s.sw.to != "" && !s.sw.typed && !s.open {
			check = time.After(switchPoll)
		}
		select {
		case ev := <-s.events:
			s.hookEvent(ev)
		case <-s.escBy:
			s.escBy = nil
			s.promptEsc()
		case c := <-s.asks:
			if s.sw.to == "" { // a switch the user picked wins
				s.setSwitch(pendingSwitch{to: c.Vendor, asked: true, choice: c})
			}
		case <-check:
			s.offerSwitch()
		case <-s.exitBy:
			s.exitBy = nil
			if s.sw.typed && !s.open {
				s.pk.tell(fmt.Sprintf("%s has not exited after aggy typed its exit command, so it is still running.", s.o.Vendor),
					fmt.Sprintf("If it exits, aggy switches to %s. To cancel, press the switch key, then Esc in the list.", s.sw.to))
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
			s.input(b)
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
					ex.SwitchTo, ex.Next = s.sw.to, s.sw.choice
					ex.Next.Vendor = s.sw.to
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
	// pickerOverCLI is how the open picker was drawn; statusRows, when
	// set, is the row count of the screen a confirmation's status line
	// went on the bottom row of.
	pickerOverCLI bool
	statusRows    int
	// toChild holds input for the CLI. The main loop only ever pushes,
	// which never blocks, so a CLI that stops reading cannot stall signal
	// handling, the picker, or the terminal-closed check.
	toChild *byteQueue

	// The switch state, owned by the main loop. events and asks bring
	// it the session's hook event names, in arrival order, and the
	// agent's switch requests from the socket; quit closes when Run
	// returns.
	sw     pendingSwitch
	events chan hookEvent
	asks   chan Choice
	quit   chan struct{}
	// atPrompt: a turn end from the session came after the last Enter the
	// user typed (or none was typed since a launch that started no turn),
	// and no hold since.
	atPrompt bool
	// held: a hook other than a turn end (a Notification: maybe a
	// permission prompt) came from the session, and the user has not
	// typed to the CLI since. A turn end does not clear it: a nested
	// CLI's Stop can follow a prompt that is still open (S-3).
	held bool
	// dirty: the user typed something after their last Enter, which the
	// CLI may still hold unsent; toldDirty: they were told it blocks the
	// switch.
	dirty, toldDirty bool
	// lastByte is the last byte sent to the CLI; inPaste: a bracketed
	// paste was opened and not closed.
	lastByte byte
	inPaste  bool
	// openedAt is when the picker last opened.
	openedAt time.Time
	// input feeds keys through the key filter (set by Run).
	input func([]byte)
	// escBy fires escTimeout after a lone Esc at a prompt aggy opened.
	escBy <-chan time.Time
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
	// choice is what was picked or asked for with the vendor (T6665).
	choice Choice
}

// hookEvent is a hook event from the session: its name and the vendor
// session id its payload names.
type hookEvent struct {
	name, sessionID string
}

// onHook is the socket's hook callback: it passes the event on, and
// hands an event of the vendor on screen from the session to the main
// loop, in order (none is dropped: a lost hold would let a switch through).
// It runs on a connection's goroutine.
func (s *session) onHook(r Request) {
	if s.o.OnHook != nil {
		s.o.OnHook(r)
	}
	if r.FromSession && r.Vendor == string(s.o.Vendor) && r.Event != "SessionStart" {
		ev := hookEvent{name: r.Event, sessionID: hook.Parse(s.o.Vendor, r.Payload).SessionID}
		select {
		case s.events <- ev:
		case <-s.quit:
		}
	}
}

// hookEvent applies one hook event from the session. A turn end of the
// on-screen child itself (its payload names the child's session id, S-9)
// puts the CLI at its prompt unless a hold stands; a turn end of another
// session, such as a nested claude -p or codex exec in the agent's shell,
// or one whose id is not known yet, is ignored. Every other event
// (SessionStart excepted, which onHook skips), from any session in the
// tree, holds the switch: Notification is how Claude and Grok say they
// wait for the user, a permission prompt among others, and its type field
// is not verified, so every one counts.
func (s *session) hookEvent(ev hookEvent) {
	if turnEnd(ev.name) {
		if ev.sessionID != "" && s.o.SessionID != nil && ev.sessionID == s.o.SessionID() {
			s.atPrompt = !s.held
		}
		return
	}
	s.atPrompt, s.held = false, true
}

// onSwitch takes a switch-request from the session (the socket refuses
// any other): the vendor must be one the picker offers and CanSwitch
// allows. It queues it for the main loop, which asks the user at the
// turn end. It runs on a connection's goroutine.
func (s *session) onSwitch(r Request) error {
	v := adapter.Vendor(r.Vendor)
	if !holdSignal(s.o.Vendor) {
		// TODO(T6753): log switch.agent_no_hold.
		return fmt.Errorf("an agent cannot ask for a switch away from %s: aggy cannot tell when %s waits for an approval, so only the user may start one, with aggy's switch key",
			s.o.Vendor, s.o.Vendor)
	}
	if err := s.offered(v); err != nil && !errors.Is(err, ErrChecking) {
		return err
	}
	if !s.waiting.CompareAndSwap(false, true) {
		return errors.New("a switch is already waiting")
	}
	// The only one past the swap; the channel holds one.
	s.asks <- Choice{Vendor: v, New: r.New, Model: r.Model, Brief: r.Brief}
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
//
// Only a plain Enter sends (S-13): a chunk that ends in a \r not preceded
// by ESC (Option+Enter, a newline in Claude) and not by a backslash (also
// a newline in Claude), even one in the chunk before, and not inside a
// bracketed paste. Anything else leaves text that may be unsent. When in
// doubt it stays dirty: the user can press Enter on an empty prompt.
// shortcut: a CLI that turns on kitty's all-keys-as-escapes mode gets
// Enter as CSI 13 u, which never clears it, so it cannot be switched
// from by aggy. Upgrade if T6684 finds one.
func (s *session) send(b []byte) {
	s.lastIn = time.Now()
	s.held = false
	if len(b) == 0 {
		return
	}
	if i, j := bytes.LastIndex(b, pasteStart), bytes.LastIndex(b, pasteEnd); i > j {
		s.inPaste = true
	} else if j >= 0 {
		s.inPaste = false
	}
	if bytes.IndexByte(b, '\r') >= 0 {
		s.atPrompt, s.toldDirty = false, false // an Enter may start a turn
	}
	prev := s.lastByte
	if len(b) > 1 {
		prev = b[len(b)-2]
	}
	s.dirty = b[len(b)-1] != '\r' || prev == esc || prev == '\\' || s.inPaste
	s.lastByte = b[len(b)-1]
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
	if s.dirty {
		// S-1: the exit command would go after the unsent text and send
		// both, so the switch waits; the user is told once.
		if !s.toldDirty {
			s.toldDirty = true
			s.pk.tell(fmt.Sprintf("aggy is ready to switch to %s, but you typed text you have not sent to %s.", v, on),
				"Send it, or clear it and press Enter on the empty prompt; aggy asks again at the",
				fmt.Sprintf("next turn end, or when you pick %s again after that Enter.", v))
			s.openPicker()
		}
		return
	}
	if err := s.offered(v); errors.Is(err, ErrChecking) {
		return // offered once the check is done
	} else if err != nil {
		s.setSwitch(pendingSwitch{})
		s.pk.tell(fmt.Sprintf("Cannot switch to %s: %v", v, err))
		s.openPicker()
		return
	}
	// One status line (S-12): what matters first, as a narrow terminal
	// cuts the end.
	cmd, verified := adapter.ExitCommand(on)
	lines := []string{fmt.Sprintf("See %s's prompt? %s types %s; Esc stays.", on, keyName(s.pk.key), cmd)}
	if s.sw.asked {
		lines = append(lines, "(The agent asked.)")
	}
	if !verified {
		lines = append(lines, fmt.Sprintf("(%s is unverified.)", cmd))
	}
	lines = append(lines, choiceNote(s.sw.choice)...)
	s.pk.ask(v, lines...)
	s.openPicker()
}

// choiceNote describes a switch's options, for the status line's end.
func choiceNote(c Choice) []string {
	var out []string
	if c.New {
		out = append(out, "(A new session.)")
	}
	if c.Model != "" {
		out = append(out, "(Model "+c.Model+".)")
	}
	if c.Brief {
		out = append(out, "(With a brief.)")
	}
	return out
}

// typeExit types the on-screen CLI's exit command, through the input
// queue, after anything the user typed before it.
//
// It sends no Esc first to clear a menu: what Esc does at each CLI's
// prompt and approval menus is not in docs/verification.md (S-3).
func (s *session) typeExit() {
	if s.dirty || s.held {
		return
	}
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
	s.pk.waiting, s.pk.checking = s.sw.to, false
	// Starts the checks that take time (the policy probe) for every
	// vendor offered, in the background, so a pick rarely waits.
	if s.o.CanSwitch != nil {
		for _, v := range s.o.Vendors {
			if err := s.o.CanSwitch(v); errors.Is(err, ErrChecking) && v == s.sw.to {
				s.pk.checking = true
			}
		}
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
	s.openedAt = time.Now()
	s.pickerOverCLI = s.alt.on
	s.statusRows = 0
	if s.pk.confirm != "" {
		if rows, cols := s.t.size(); rows > 1 {
			s.statusRows, s.pickerOverCLI = rows, true
			s.pk.drawStatus(s.o.Out, rows, cols, s.alt.on)
			return
		}
	}
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
	if s.statusRows > 0 {
		s.pk.eraseStatus(s.o.Out, s.statusRows, s.alt.on)
	} else {
		s.pk.erase(s.o.Out, s.pickerOverCLI)
	}
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
	if s.pk.confirm != "" || s.pk.note {
		s.promptInput(b)
		return
	}
	done, c := s.pk.handle(b)
	if !done {
		s.pk.draw(s.o.Out, s.pickerOverCLI) // a toggle, the model edit, or a child list
		return
	}
	picked := c.Vendor
	s.closePicker()
	switch {
	case s.sw.typed:
		// The exit command is typed: Esc cancels the switch, so a late
		// exit ends aggy; a pick changes nothing.
		if picked == "" {
			s.setSwitch(pendingSwitch{})
		}
	case picked != "" && picked == s.sw.to:
		// Picked again while it waits: the user says no turn is running
		// (an Enter that started none leaves no turn end to wait for).
		// Not past a hold (S-3), and not past unsent text (S-8): that
		// stays until the user's next Enter, which offerSwitch reports.
		// The new pick's options replace the old.
		if s.held {
			s.pk.tell(fmt.Sprintf("%s said it is waiting for you (a permission prompt or another notice).", s.o.Vendor),
				fmt.Sprintf("Answer it, or send a message; aggy switches to %s at the next turn end.", picked))
			s.openPicker()
			return
		}
		s.atPrompt, s.toldDirty = true, false
		s.sw.choice, s.sw.asked = c, false
	case picked != "":
		if err := s.offered(picked); err != nil && !errors.Is(err, ErrChecking) {
			// TODO(T6753): log picker.refused.
			s.pk.tell(fmt.Sprintf("Cannot switch to %s: %v", picked, err))
			s.openPicker()
			return
		}
		s.setSwitch(pendingSwitch{to: picked, choice: c})
	default: // Esc on the list cancels a waiting switch
		s.setSwitch(pendingSwitch{})
	}
}

// promptInput handles a key at a prompt aggy opened on its own (a
// confirmation or a note), which the user may have been typing into when
// it appeared (S-2). Esc closes it (and, at a confirmation, drops the
// switch), once escTimeout shows it is not the start of a longer key
// (S-11). The switch key confirms a confirmation once confirmGuard has
// passed, and at a note opens the list (S-7); neither reaches the CLI.
// Any other input closes the prompt and goes to the CLI as typed, through
// the key filter: nothing is lost, and the switch, if any, still waits.
func (s *session) promptInput(b []byte) {
	if s.escBy != nil {
		// The Esc was the start of a longer key: hand it all back.
		s.escBy = nil
		b = append([]byte{esc}, b...)
		s.closePicker()
		s.input(b)
		return
	}
	confirming := s.pk.confirm != ""
	switch {
	case isEsc(b):
		s.escBy = time.After(escTimeout)
	case s.pk.isKey(b) && confirming:
		if time.Since(s.openedAt) < confirmGuard {
			return // too soon to be meant: ignored
		}
		s.closePicker()
		s.typeExit()
	case s.pk.isKey(b):
		s.closePicker()
		s.openList()
	default:
		s.closePicker()
		s.input(b)
	}
}

// promptEsc is a lone Esc at a prompt aggy opened, once nothing followed
// it within escTimeout.
func (s *session) promptEsc() {
	if !s.open || (s.pk.confirm == "" && !s.pk.note) {
		return
	}
	confirming := s.pk.confirm != ""
	s.closePicker()
	if confirming {
		s.setSwitch(pendingSwitch{})
	}
}

// isEsc reports whether b is a lone Esc press, as a byte or a kitty
// report.
func isEsc(b []byte) bool {
	if len(b) == 1 {
		return b[0] == esc
	}
	r, ok := parseCSIu(b)
	return ok && r.n == len(b) && r.code == 27
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
