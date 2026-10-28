//go:build unix

package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
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
	s := &session{o: o, t: t, atPrompt: !o.StartsTurn, lenKnown: true, events: make(chan hookEvent, 16), asks: make(chan Choice, 1), quit: make(chan struct{}), shares: make(chan shareReading)}
	defer close(s.quit)
	var srv *Server
	if o.RunDir != "" {
		var err error
		srv, err = Listen(o.RunDir, o.ParentID, t.restore, s.onHook)
		if err != nil {
			return Exit{Code: 1}, err
		}
		defer func() { _ = srv.Close() }()
		srv.SetLog(s.logWait)
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
		// It also polls a near-limit warning due at a turn end.
		var check <-chan time.Time
		if (s.sw.to != "" && !s.sw.typed || s.warnDue) && !s.open {
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
				s.sw.rec = s.log(diag.Rec{Event: diag.EvSwitchQueued, Attrs: switchAttrs(c, map[string]any{"origin": "agent", "to": string(c.Vendor)})})
			} else {
				s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeSwitchBusy, Msg: "the agent's switch came after the user's pick", Attrs: map[string]any{"to": string(c.Vendor)}})
			}
		case <-check:
			if s.sw.to != "" && !s.sw.typed {
				s.offerSwitch()
			}
			s.offerWarn()
		case h := <-s.o.Limits:
			s.onLimit(h)
		case r := <-s.shares:
			if r.seq == s.shareSeq {
				s.share = &r
			}
		case <-s.exitBy:
			s.exitBy = nil
			if s.sw.typed {
				s.log(diag.Rec{Event: diag.EvSwitchExitLate, Code: diag.CodeExitTimeout, Cause: s.sw.rec, Attrs: map[string]any{"to": string(s.sw.to)}})
			}
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
					s.log(diag.Rec{Event: diag.EvSwitchCompleted, Cause: s.sw.rec, Exit: diag.Int(w.code), Attrs: switchAttrs(ex.Next, map[string]any{"to": string(s.sw.to)})})
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
	// pasteTail is the last bytes sent, for a paste marker split across
	// reads. compLen counts the bytes in the composer since the last
	// clean send, while lenKnown (an Enter a popup may have taken leaves
	// it unknown); popup is set once an @ or a leading / was typed since
	// then; cleared is set when the user emptied the line, until the
	// Enter that confirms it (T6754, O56-3).
	pasteTail []byte
	compLen   int
	lenKnown  bool
	popup     bool
	cleared   bool
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
	// warnDue: a turn of the on-screen child ended and the near-limit
	// check (offerWarn) has not run since; warned: the warning was shown
	// (or a limit hit) and its crossing has not ended, warnedWindow and
	// warnedReset being the window it was about (T6699). shares brings
	// askShare's readings; share is the latest ask's, nil until it is in.
	warnDue, warned bool
	warnedWindow    string
	warnedReset     time.Time
	shares          chan shareReading
	share           *shareReading
	shareSeq        int
	// lastTurnEnd is the id of the record of the latest counted turn end
	// of the on-screen child, and lastLimit of the latest limit: the
	// causes of what each sets off (G67-2).
	lastTurnEnd, lastLimit string
	// foreign is, per session id a turn end named that was not the
	// on-screen one, when it was last recorded and how many came since
	// (G67-4). Only in memory; owned by the main loop.
	foreign map[string]*foreignSeen
}

// foreignSeen is one foreign session's turn ends: when one was last
// recorded, and how many came since.
type foreignSeen struct {
	at      time.Time
	repeats int
}

// maxForeign caps the sessions foreign keeps; past it, it starts over.
const maxForeign = 64

// turnCause is the cause of a switch's step at a turn end: the counted
// turn end, when it came after the switch's latest record (ids sort by
// time), else that record.
func (s *session) turnCause() string {
	if s.lastTurnEnd > s.sw.rec {
		return s.lastTurnEnd
	}
	return s.sw.rec
}

// ignoredTurnEnd records a turn end that named another session, at most
// once a minute per session, with how many came since the last record.
// A turn end naming no session is keyed by "" and recorded the same
// way, as a security rejection (G67-R1).
func (s *session) ignoredTurnEnd(ev hookEvent) {
	now := time.Now()
	if s.foreign == nil || len(s.foreign) >= maxForeign {
		s.foreign = map[string]*foreignSeen{}
	}
	f := s.foreign[ev.sessionID]
	if f != nil && now.Sub(f.at) < time.Minute {
		f.repeats++
		return
	}
	repeats := 0
	if f != nil {
		repeats = f.repeats
	}
	s.foreign[ev.sessionID] = &foreignSeen{at: now}
	attrs := map[string]any{"event": hookName(ev.name), "repeats": repeats}
	if ev.sessionID == "" {
		// The vendor's own hook always names its session.
		s.log(diag.Rec{Event: diag.EvHookRejected, Code: diag.CodeNoSession, Msg: "a turn end that names no session", Attrs: attrs})
		return
	}
	s.log(diag.Rec{Event: diag.EvHookIgnored, Code: diag.CodeForeignSession, Msg: "a turn end of another session", Attrs: attrs})
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
	limit bool // a limit of the vendor on screen suggested it (T6667)
	// unverified: that limit came from a hook, which any process in the
	// CLI's tree can send (O56-2), so it is labeled like an agent's ask.
	unverified bool
	typed      bool // the exit command has been typed
	// choice is what was picked or asked for with the vendor (T6665).
	choice Choice
	// rec is the id of the switch's latest diagnostic record, the cause
	// of its next.
	rec string
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
	switch {
	case !r.FromSession:
		// The socket recorded it (peer.lineage_failed).
	case r.Vendor != string(s.o.Vendor):
		s.logWait(diag.Rec{Event: diag.EvHookRejected, Code: diag.CodeWrongVendor, Msg: "a hook event of another vendor than the one on screen",
			Attrs: map[string]any{"event": hookName(r.Event), "from": r.Vendor}})
	case r.Event == "SessionStart":
		s.logWait(diag.Rec{Event: diag.EvHookAccepted, Attrs: map[string]any{"event": hookName(r.Event)}})
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
		own := ""
		if s.o.SessionID != nil {
			own = s.o.SessionID()
		}
		// Neither session id is stored: the payload's could be any text.
		switch {
		case ev.sessionID == "":
			s.ignoredTurnEnd(ev)
		case own == "":
			s.log(diag.Rec{Event: diag.EvHookAccepted, Msg: "the on-screen session's id is not known yet, so the turn end is not counted",
				Attrs: map[string]any{"event": hookName(ev.name), "counted": false}})
		case ev.sessionID != own:
			s.ignoredTurnEnd(ev)
		default:
			s.lastTurnEnd = s.log(diag.Rec{Event: diag.EvHookAccepted, Attrs: map[string]any{"event": hookName(ev.name), "counted": true, "held": s.held}})
		}
		if ev.sessionID != "" && ev.sessionID == own {
			s.atPrompt = !s.held
			s.warnDue = true
			s.askShare()
			if ev.name == "Stop" && holdSignal(s.o.Vendor) && s.pk != nil {
				s.pk.preselect = "" // a turn ended without a limit (O56-5)
			}
		}
		return
	}
	s.log(diag.Rec{Event: diag.EvHookAccepted, Attrs: map[string]any{"event": hookName(ev.name), "holds": true}})
	s.atPrompt, s.held = false, true
}

// onSwitch takes a switch-request from the session (the socket refuses
// any other): the vendor must be one the picker offers and CanSwitch
// allows. It queues it for the main loop, which asks the user at the
// turn end. It runs on a connection's goroutine.
func (s *session) onSwitch(r Request) error {
	v := adapter.Vendor(r.Vendor)
	if !holdSignal(s.o.Vendor) {
		return coded(diag.CodeAgentNoHold, fmt.Errorf("an agent cannot ask for a switch away from %s: aggy cannot tell when %s waits for an approval, so only the user may start one, with aggy's switch key",
			s.o.Vendor, s.o.Vendor))
	}
	if r.New || r.Model != "" {
		// O56-1: a new session or a model is the user's choice, in the
		// picker; the agent may ask for a vendor and a brief only.
		return coded(diag.CodeAgentOptions, errors.New("an agent may ask for a vendor and a brief only; a new session or a model for the launch is the user's choice, in aggy's picker"))
	}
	if err := s.offered(v); err != nil && !errors.Is(err, ErrChecking) {
		return coded(diag.CodeSwitchVendor, err)
	}
	if !s.waiting.CompareAndSwap(false, true) {
		return coded(diag.CodeSwitchBusy, errors.New("a switch is already waiting"))
	}
	// The only one past the swap; the channel holds one.
	s.asks <- Choice{Vendor: v, Brief: r.Brief}
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
	if err := s.o.CanSwitch(v); err != nil {
		return &checkError{err}
	}
	return nil
}

// checkError is CanSwitch's answer: its text can hold a path or a CLI's
// own error text, so a record never stores it (G67-1).
type checkError struct{ err error }

func (e *checkError) Error() string { return e.err.Error() }
func (e *checkError) Unwrap() error { return e.err }

// refusedMsg is a record's message for a switch to v refused with err:
// err's text when it is aggy's own (offered), else a fixed one.
func refusedMsg(v adapter.Vendor, err error) string {
	var ce *checkError
	if errors.As(err, &ce) {
		return fmt.Sprintf("cannot switch to %s: the repo's allowed_vendors, its CLI not on PATH, or the safety policy", v)
	}
	return err.Error()
}

// send queues the user's input for the CLI and notes what it may start:
// an Enter may start a turn, and text after the last Enter may be unsent.
//
// It follows the composer byte by byte. Only a plain Enter sends (S-13):
// a \r not preceded by ESC (Option+Enter, a newline in Claude) and not by
// a backslash (also a newline in Claude), even one in the read before,
// and not inside a bracketed paste, whose markers are found across reads
// too (T6754). After an @ or a leading / (T6754, O56-3) an Enter may only
// accept an entry in the CLI's file or command popup and leave the text,
// with more than was typed, in the composer: from then on every Enter
// counts as unsent until one on a line the user explicitly emptied, with
// Ctrl+U (unverified: it is the readline kill-line key and is assumed to
// clear the composer) or with backspaces down to zero while aggy still
// knew the length. When in doubt it stays dirty.
// TODO(O56-3): aggy installs no UserPromptSubmit hook yet; a turn-start
// hook could confirm a send, but any process in the CLI's tree can send
// one, so it would need care before clearing this.
// shortcut: a CLI that turns on kitty's all-keys-as-escapes mode gets
// Enter as CSI 13 u, which never clears it, so it cannot be switched
// from by aggy. Upgrade if T6684 finds one.
func (s *session) send(b []byte) {
	s.lastIn = time.Now()
	s.held = false
	if len(b) == 0 {
		return
	}
	if bytes.IndexByte(b, '\r') >= 0 {
		s.atPrompt, s.toldDirty = false, false // an Enter may start a turn
	}
	for _, c := range b {
		s.compose(c)
	}
	s.dirty = !s.lenKnown || s.compLen > 0 || s.popup || s.cleared
	s.toChild.push(b)
}

// compose follows one byte the user sent (see send).
func (s *session) compose(c byte) {
	prev := s.lastByte
	s.lastByte = c
	// The paste markers, found with the bytes before, so one split
	// across reads still counts.
	s.pasteTail = append(s.pasteTail, c)
	if bytes.HasSuffix(s.pasteTail, pasteStart) {
		s.inPaste = true
	} else if bytes.HasSuffix(s.pasteTail, pasteEnd) {
		s.inPaste = false
	}
	if n := len(pasteEnd); len(s.pasteTail) >= n {
		s.pasteTail = append(s.pasteTail[:0], s.pasteTail[len(s.pasteTail)-n+1:]...)
	}
	switch {
	case c == '\r' && (prev == esc || prev == '\\' || s.inPaste):
		s.compLen++ // a newline in the composer
	case c == '\r' && s.popup:
		s.lenKnown = false // the popup may have taken it and added text
	case c == '\r':
		s.compLen, s.lenKnown, s.cleared = 0, true, false // sent
	case c == ctrlU:
		// The line is empty, but it counts only once an Enter confirms
		// it: in a multiline draft Ctrl+U may clear one line only.
		s.compLen, s.lenKnown, s.popup, s.cleared = 0, true, false, true
	case c == 0x7f || c == 0x08:
		if s.compLen > 0 {
			s.compLen--
			if s.compLen == 0 && s.lenKnown && s.popup {
				s.popup, s.cleared = false, true
			}
		}
	default:
		if c == '@' || c == '/' && s.compLen == 0 {
			s.popup = true
		}
		s.compLen++
	}
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
			s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeUnsentText, Cause: s.turnCause(),
				Msg: "the switch waits: text the user typed is not sent", Attrs: map[string]any{"to": string(v)}})
			s.pk.tell(fmt.Sprintf("aggy is ready to switch to %s, but you typed text you have not sent to %s.", v, on),
				"Send it, or clear it (Ctrl+U or Backspace) and press Enter on the empty prompt; aggy asks again at the",
				fmt.Sprintf("next turn end, or when you pick %s again after that Enter.", v))
			s.openPicker()
		}
		return
	}
	if err := s.offered(v); errors.Is(err, ErrChecking) {
		return // offered once the check is done
	} else if err != nil {
		s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeSwitchVendor, Cause: s.turnCause(), Msg: refusedMsg(v, err), Attrs: map[string]any{"to": string(v)}})
		s.setSwitch(pendingSwitch{})
		s.pk.tell(fmt.Sprintf("Cannot switch to %s: %v", v, err))
		s.openPicker()
		return
	}
	// One status line (S-12, O56-1): who or what asked and the switch's
	// options first, then the keys, then the check; a narrow terminal
	// cuts the end.
	cmd, verified := adapter.ExitCommand(on)
	var why []string
	switch {
	case s.sw.asked:
		why = append(why, "the agent asks")
	case s.sw.limit:
		why = append(why, limitWho(LimitHit{Vendor: on, Unverified: s.sw.unverified}))
	}
	why = append(why, choiceNote(s.sw.choice)...)
	head := "-> " + string(v) // the full line fits 80 columns without an origin
	if len(why) > 0 {
		head = strings.Join(why, ", ") + " " + head
	}
	line := fmt.Sprintf("%s. %s switches, Esc stays. See %s's prompt? It types %s.", head, keyName(s.pk.key), on, cmd)
	if !verified {
		line += fmt.Sprintf(" (%s is unverified.)", cmd)
	}
	lines := []string{line}
	s.sw.rec = s.log(diag.Rec{Event: diag.EvSwitchOffered, Cause: s.turnCause(), Attrs: switchAttrs(s.sw.choice, map[string]any{"to": string(v), "asked": s.sw.asked, "limit": s.sw.limit})})
	s.pk.preselect = "" // offered once (O56-5)
	s.pk.ask(v, lines...)
	s.openPicker()
}

// offerWarn shows the near-limit warning (T6699) after a turn end of the
// on-screen child, once the CLI is at its prompt and quiet, as a switch
// is offered, and once the share read at that turn end (askShare) is in:
// when the vendor on screen has used WarnAt of its limit or more
// (Options.Used, only where its CLI writes that locally), once per
// crossing. It never switches: the switch key opens the list with the
// first vendor with quota preselected, and a pick there waits for a turn
// end and the user's confirmation like any, unsent text included. A turn
// started, a switch already waiting, or the picker up drops it until the
// next turn end.
//
// A crossing ends when the warned window's reset time has passed, or a
// reading of that same window is below WarnAt; a lower share from
// another window or rollout does not end it (L60-3). After a limit
// (onLimit), whose window is not known, any reading below WarnAt does.
func (s *session) offerWarn() {
	if !s.warnDue {
		return
	}
	if !s.atPrompt || s.sw.to != "" || s.open {
		s.warnDue = false
		return
	}
	now := time.Now()
	if now.Sub(s.lastIn) < switchQuiet || now.Sub(time.Unix(0, s.lastOut.Load())) < switchQuiet {
		return
	}
	if s.share == nil {
		return // still being read
	}
	r := *s.share
	s.warnDue = false
	if s.warned && !s.warnedReset.IsZero() && !now.Before(s.warnedReset) {
		s.warned = false
	}
	if !r.ok {
		return
	}
	if r.Used < s.o.WarnAt {
		if s.warned && (s.warnedWindow == "" || r.Window == s.warnedWindow) {
			s.warned = false
		}
		return
	}
	if s.warned {
		return
	}
	s.warned, s.warnedWindow, s.warnedReset = true, r.Window, r.Reset
	// The epsilon keeps 0.29*100 (28.999...) at 29%.
	pct := int(math.Floor(r.Used*100 + 1e-9))
	pre := s.withQuota()
	rec := diag.Rec{Event: diag.EvNearLimitWarned, Cause: s.lastTurnEnd,
		Attrs: map[string]any{"used_pct": pct, "warn_at_pct": int(math.Floor(s.o.WarnAt*100 + 1e-9)), "window": r.Window, "to": string(pre)}}
	if pre == "" {
		rec.Code = diag.CodeNearNoQuota
	}
	s.log(rec)
	s.pk.warn(pre, warnForms(s.o.Vendor, pct, keyName(s.pk.key), pre)...)
	s.openPicker()
}

// warnForms is the near-limit warning's line, from longest to shortest
// (L60-4): v is at pct% of its limit, and the switch key hands off to
// pre ("": no vendor has quota left). Each form ends in its keys; with
// vendor names of at most 6 bytes the first fits 80 columns, and the last
// 13.
func warnForms(v adapter.Vendor, pct int, key string, pre adapter.Vendor) []string {
	if pre == "" {
		return []string{
			fmt.Sprintf("aggy: %s is at %d%% of its limit. No vendor with quota left; Esc dismisses.", v, pct),
			fmt.Sprintf("aggy: %s %d%%: no quota left, Esc", v, pct),
			fmt.Sprintf("%s %d%%: no quota, Esc", v, pct),
			"Esc",
		}
	}
	return []string{
		fmt.Sprintf("aggy: %s is at %d%% of its limit. %s hands off to %s, Esc dismisses.", v, pct, key, pre),
		fmt.Sprintf("aggy: %s %d%%: %s -> %s, Esc", v, pct, key, pre),
		fmt.Sprintf("%s -> %s, Esc", key, pre),
		key + " / Esc",
	}
}

// askShare reads the share used of the vendor on screen off the main
// loop, at a turn end of the on-screen child (L60-2); the main loop keeps
// the reading of the latest ask only.
func (s *session) askShare() {
	if s.o.Used == nil || s.o.WarnAt <= 0 {
		return
	}
	s.shareSeq++
	s.share = nil
	seq, v := s.shareSeq, s.o.Vendor
	s.goSafe(func() {
		sh, ok := s.o.Used(v)
		select {
		case s.shares <- shareReading{Share: sh, ok: ok, seq: seq}:
		case <-s.quit:
		}
	})
}

// shareReading is one answer of Options.Used.
type shareReading struct {
	Share
	ok  bool
	seq int
}

// withQuota is the first vendor the picker offers that has quota left
// (HasQuota), or "".
func (s *session) withQuota() adapter.Vendor {
	for _, o := range s.o.Vendors {
		if s.o.HasQuota == nil || s.o.HasQuota(o) {
			return o
		}
	}
	return ""
}

// onLimit handles a limit the vendor on screen reported (T6667). The
// first vendor the picker offers that has quota left is preselected:
// Enter in the list takes it, with a brief. Where the vendor on screen
// has a hold signal it is also queued as a switch with a brief, which,
// like any, waits for the turn end and the user's switch key on the
// status line: one key accepts, and nothing is ever confirmed for the
// user. Without a hold signal (Codex) a limit, like an agent's request,
// could be forged from the CLI's tree (S-12), so aggy only says so and
// leaves the pick to the user.
//
// TODO(T6617): a model-quota limit (one model out, others not) is not
// verified; any limit counts for the whole vendor.
//
// A limit from a hook (Unverified) could be forged by any process in the
// CLI's tree (O56-2): it gets the rules of an agent's ask and is labeled
// unverified; a vendor without a hold signal never queues either.
func (s *session) onLimit(h LimitHit) {
	v := h.Vendor
	if v != s.o.Vendor {
		return
	}
	s.lastLimit = s.log(diag.Rec{Event: diag.EvLimitReceived, Code: diag.CodeRateLimit, Attrs: map[string]any{"unverified": h.Unverified}})
	// L60-1: a limit ends any near-limit warning: none follows for this
	// crossing, and one on screen gives way to the limit's own note.
	s.warned, s.warnDue, s.warnedWindow, s.warnedReset = true, false, "", time.Time{}
	if s.open && s.pk.warning {
		s.closePicker()
	}
	pre := s.withQuota()
	s.pk.preselect = pre
	if s.open || s.sw.to != "" {
		return // the picker is up, or a switch already waits
	}
	switch {
	case pre == "":
		s.log(diag.Rec{Event: diag.EvLimitNotOffered, Code: diag.CodeNoQuota, Cause: s.lastLimit})
		s.pk.tell(fmt.Sprintf("%s, and no other vendor aggy offers has quota left.", limitWho(h)))
		s.openPicker()
	case !holdSignal(v):
		s.log(diag.Rec{Event: diag.EvLimitNotOffered, Code: diag.CodeLimitNoHold, Cause: s.lastLimit, Attrs: map[string]any{"to": string(pre)}})
		s.pk.tell(fmt.Sprintf("%s. %s has quota left: press %s, then Enter, to switch to it with a brief.", limitWho(h), pre, keyName(s.pk.key)),
			fmt.Sprintf("aggy cannot tell when %s waits for an approval, so it does not offer the switch itself.", v))
		s.openPicker()
	default:
		s.setSwitch(pendingSwitch{to: pre, limit: true, unverified: h.Unverified, choice: Choice{Vendor: pre, Brief: true}})
		s.sw.rec = s.log(diag.Rec{Event: diag.EvSwitchQueued, Cause: s.lastLimit,
			Attrs: switchAttrs(s.sw.choice, map[string]any{"origin": "limit", "to": string(pre), "unverified": h.Unverified})})
	}
}

// limitWho says who reported h: a hook's report is labeled unverified
// (O56-2).
func limitWho(h LimitHit) string {
	if h.Unverified {
		return string(h.Vendor) + " reports a limit (unverified)"
	}
	return string(h.Vendor) + " hit its limit"
}

// choiceNote names a switch's options, for the status line.
func choiceNote(c Choice) []string {
	var out []string
	if c.New {
		out = append(out, "new session")
	}
	if c.Model != "" {
		out = append(out, "model "+c.Model)
	}
	if c.Brief {
		out = append(out, "brief")
	}
	return out
}

// typeExit types the on-screen CLI's exit command, through the input
// queue, after anything the user typed before it.
//
// It sends no Esc first to clear a menu: what Esc does at each CLI's
// prompt and approval menus is not in docs/verification.md (S-3).
func (s *session) typeExit() {
	switch {
	case s.dirty:
		s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeUnsentText, Cause: s.sw.rec, Msg: "the switch key was pressed, but text the user typed is not sent"})
		return
	case s.held:
		s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeSwitchHeld, Cause: s.sw.rec, Msg: "the switch key was pressed, but the CLI waits for the user"})
		return
	}
	cmd, _ := adapter.ExitCommand(s.o.Vendor)
	s.toChild.push([]byte(cmd + "\r"))
	s.sw.typed = true
	s.sw.rec = s.log(diag.Rec{Event: diag.EvSwitchConfirmed, Cause: s.sw.rec, Attrs: map[string]any{"to": string(s.sw.to)}})
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
	// After a limit, Enter takes the suggested vendor, with a brief.
	s.pk.opts.Brief = s.pk.preselect != ""
	// Starts the checks that take time (the policy probe) for every
	// vendor offered, in the background, so a pick rarely waits.
	if s.o.CanSwitch != nil {
		for _, v := range s.o.Vendors {
			if err := s.o.CanSwitch(v); errors.Is(err, ErrChecking) && v == s.sw.to {
				s.pk.checking = true
			}
		}
	}
	s.log(diag.Rec{Event: diag.EvPickerOpened, Attrs: map[string]any{"preselect": string(s.pk.preselect), "waiting": string(s.sw.to)}})
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
	if s.pk.confirm != "" || s.pk.warning {
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
	s.pk.preselect = "" // used, or turned down (O56-5)
	s.closePicker()
	if picked == "" {
		s.log(diag.Rec{Event: diag.EvPickerDismissed, Attrs: map[string]any{"waiting": string(s.sw.to)}})
		if s.sw.to != "" {
			s.log(diag.Rec{Event: diag.EvSwitchCancelled, Cause: s.sw.rec, Attrs: map[string]any{"how": "esc_list", "to": string(s.sw.to), "exit_typed": s.sw.typed}})
		}
	} else {
		s.log(diag.Rec{Event: diag.EvPickerChosen, Attrs: switchAttrs(c, map[string]any{"to": string(picked), "again": picked == s.sw.to})})
	}
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
			s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeSwitchHeld, Cause: s.sw.rec, Msg: "picked again, but the CLI waits for the user"})
			s.pk.tell(fmt.Sprintf("%s said it is waiting for you (a permission prompt or another notice).", s.o.Vendor),
				fmt.Sprintf("Answer it, or send a message; aggy switches to %s at the next turn end.", picked))
			s.openPicker()
			return
		}
		s.atPrompt, s.toldDirty = true, false
		s.sw.choice, s.sw.asked = c, false
		s.sw.rec = s.log(diag.Rec{Event: diag.EvSwitchQueued, Cause: s.sw.rec, Attrs: switchAttrs(c, map[string]any{"origin": "user", "to": string(picked), "again": true})})
	case picked != "":
		if err := s.offered(picked); err != nil && !errors.Is(err, ErrChecking) {
			s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeSwitchVendor, Msg: refusedMsg(picked, err), Attrs: map[string]any{"to": string(picked)}})
			s.pk.tell(fmt.Sprintf("Cannot switch to %s: %v", picked, err))
			s.openPicker()
			return
		}
		s.setSwitch(pendingSwitch{to: picked, choice: c})
		s.sw.rec = s.log(diag.Rec{Event: diag.EvSwitchQueued, Attrs: switchAttrs(c, map[string]any{"origin": "user", "to": string(picked)})})
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
		to := s.pk.warnTo // a near-limit warning's suggestion (T6699)
		s.closePicker()
		if to != "" {
			s.pk.preselect = to
		}
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
		s.log(diag.Rec{Event: diag.EvSwitchCancelled, Cause: s.sw.rec, Attrs: map[string]any{"how": "esc_confirm", "to": string(s.sw.to)}})
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

// holdSignal reports whether vendor v tells aggy, by a hook, when it
// waits for the user (a permission prompt): Claude and Grok send
// Notification; Codex has no such hook (design.md section 11). Without
// it, a Stop forged by a tool process in the CLI's own tree is
// indistinguishable from a real turn end, so a switch the agent asks for
// is refused on such a vendor (S-12): only the user, looking at the
// screen, starts one. TODO(T6617): revisit when Codex has a signal.
func holdSignal(v adapter.Vendor) bool { return v != adapter.Codex }

// turnEnd reports whether a hook event ends a turn (design.md section
// 11): Stop for every vendor, StopCancelled (Grok), and StopFailure
// (Claude, Grok: "a turn ends because of an API error",
// docs/verification.md). Codex sends only Stop.
func turnEnd(event string) bool {
	return event == "Stop" || event == "StopCancelled" || event == "StopFailure"
}

// ctrlU clears the line in a readline-style composer.
const ctrlU = 0x15
