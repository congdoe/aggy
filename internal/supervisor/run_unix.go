//go:build unix

package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/tooldef"
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
	s := &session{o: o, t: t, atPrompt: !o.StartsTurn, lenKnown: true, events: make(chan hookEvent, 16), asks: make(chan Choice, 1), quit: make(chan struct{}), shares: make(chan shareReading), childChecks: make(chan childCheck),
		toolNotes: make(chan struct{}, 1), toolReads: make(chan toolReading)}
	defer close(s.quit)
	// The last minute's repeats, queued before the caller closes the
	// diagnostic queue (G67-R3); deferred first, so it runs after the
	// socket and the main loop are done.
	defer s.flushForeign(nil)
	defer s.flushShutdown()
	var srv *Server
	if o.RunDir != "" {
		var err error
		srv, err = Listen(o.RunDir, o.ParentID, t.restore, s.onHook)
		if err != nil {
			return Exit{Code: 1}, err
		}
		defer func() { _ = srv.Close() }()
		srv.SetLog(s.logWait)
		srv.SetTools(s.onTools)
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
	s.startGuardWatch()
	defer s.stopHangupKill()
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
	var apply func([]keyEvent)
	// heldBy gives up on an escape sequence the filter holds for the next read.
	var heldBy <-chan time.Time
	// input takes the user's keys while no picker is up, and the keys a
	// prompt aggy opened hands back (S-11): through the one key filter,
	// so its paste and split-sequence state stays right.
	s.input = func(b []byte) { apply(filter.feed(b)) }
	apply = func(evs []keyEvent) {
		for _, ev := range evs {
			switch {
			case ev.report:
				s.toChild.push(ev.data) // the terminal answering the CLI: it opens and closes nothing
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
			case ev.mouse:
				s.pointer(ev.data)
			default:
				s.send(ev.data)
			}
		}
	}
	for {
		switch {
		case !filter.holding():
			heldBy = nil
		case heldBy == nil:
			heldBy = time.After(splitWait)
		}
		// check polls a switch that waits for the turn end.
		// It also polls a near-limit warning due at a turn end.
		var check <-chan time.Time
		if (s.sw.to != "" && !s.sw.typed || s.warnDue || s.toolsDue || s.limitDue != nil) && !s.open {
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
			s.releaseQueued()
			if s.sw.to != "" && !s.sw.typed {
				s.offerSwitch()
			}
			s.offerWarn()
			s.offerToolsNote()
			s.offerLimitNote()
		case <-s.toolNotes:
			s.readTools()
		case <-s.o.ToolsNudge:
			s.readTools()
		case r := <-s.toolReads:
			s.toolsRead(r)
		case h := <-s.o.Limits:
			s.onLimit(h)
		case r := <-s.childChecks:
			s.childChecked(r)
		case r := <-s.shares:
			if r.seq != s.shareRead {
				break // a read given up on as stuck (T6811)
			}
			s.shareBusy = false
			if r.seq == s.shareSeq {
				s.share = &r
			} else {
				s.readShare() // asked again while this one was read
			}
		case <-s.exitBy:
			s.exitBy = nil
			if s.sw.typed {
				s.log(diag.Rec{Event: diag.EvSwitchExitLate, Code: diag.CodeExitTimeout, Cause: s.sw.rec, Attrs: map[string]any{"to": string(s.sw.to)}})
			}
			if s.sw.typed && !s.open {
				s.pk.tell(fmt.Sprintf("%s has not exited after aggy typed its exit command, so it is still running.", s.o.Vendor),
					exitNote(s.sw))
				s.openPicker()
			}
		case b, ok := <-input:
			if !ok {
				// The terminal is gone: hang up the CLI, as a closed
				// terminal would.
				input = nil
				s.signalled = true
				_ = child.Signal(syscall.SIGHUP)
				s.killAfterHangup(child)
				continue
			}
			// heldBy keeps running: it counts from the first byte held, so
			// key repeat after Alt+] cannot keep the keys back.
			if s.open {
				reports, rest := filter.strip(b)
				if len(reports) > 0 {
					s.toChild.push(reports)
				}
				if len(rest) > 0 {
					s.pickerInput(rest)
				}
				continue
			}
			s.input(b)
		case <-heldBy:
			heldBy = nil
			if evs := filter.flush(); s.open {
				for _, ev := range evs {
					s.pickerInput(ev.data)
				}
			} else {
				apply(evs)
			}
		case <-pending:
			pending = nil
			// A prompt aggy opened since the press (an offer, a note)
			// keeps the screen: the list never opens over it (H1).
			if !s.open {
				s.openList()
			}
		case sig := <-sigs:
			switch sig {
			case syscall.SIGWINCH:
				s.resize()
			case syscall.SIGTSTP:
				s.suspend(false)
			default:
				s.signalled = true
				_ = child.Signal(sig.(syscall.Signal))
				if sig == syscall.SIGTERM || sig == syscall.SIGHUP {
					s.killAfterHangup(child)
				}
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
				s.drainSignals(sigs)
				// Only an exit after aggy typed the exit command, or a
				// clean one after it told the user to quit for the switch
				// they picked, is a switch; any other CLI that exited ends
				// aggy, saying so if the user picked one.
				ex := Exit{Code: w.code}
				switches := s.exitSwitches(w.code)
				s.drainSignals(sigs) // the check above can wait some seconds
				if switches && s.signalled {
					switches = s.signalStops(w.code)
				}
				if o.Facts != nil {
					*o.Facts = RunFacts{TurnRan: s.turnRan, Signalled: s.signalled}
				}
				if switches {
					ex.SwitchTo, ex.Next = s.sw.to, s.sw.choice
					ex.Next.Vendor = s.sw.to
					s.log(diag.Rec{Event: diag.EvSwitchCompleted, Cause: s.sw.rec, Exit: diag.Int(w.code), Attrs: switchAttrs(ex.Next, map[string]any{"to": string(s.sw.to), "by_hand": !s.sw.typed})})
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
	// turnRan: a turn end of the on-screen child has been counted since
	// this CLI launched. Until then the CLI may be showing a launch dialog
	// (an update or trust prompt) that aggy cannot tell from a prompt, and
	// the exit command's Enter could accept it, so aggy never types it
	// (T6893). A user's Enter is no evidence: it may only answer a dialog.
	turnRan bool
	// signalled: aggy got a signal to pass on. It never clears: a signal can
	// race the key that confirms a switch, in either order, and a signal to
	// aggy must never lead to launching the next CLI. noTurnOpen: the open
	// note is tellNoTurn's.
	signalled, noTurnOpen bool
	// inputSinceTurn: input has gone to the CLI since its last counted turn
	// end, or its launch. Any of it may have started a turn, in whatever
	// encoding the terminal sends Enter, so notices that can wait do not try
	// to tell (mayBeRunning). A terminal's own reports do not count.
	inputSinceTurn bool
	// queued counts messages sent while a turn may already be running: each
	// may start its own turn as the one before ends. queuedWait: the last
	// counted turn end had one queued, so it left atPrompt false, until the
	// next turn end, or queuedQuiet (releaseQueued).
	queued     int
	queuedWait bool
	// limitDue is a limit's note waiting until it cannot hold a turn's output.
	limitDue *limitNote
	// held: a hook other than a turn end (a Notification: maybe a
	// permission prompt) came from the session, and the user has not
	// typed to the CLI since. A turn end does not clear it: a nested
	// CLI's Stop can follow a prompt that is still open (S-3).
	held bool
	// dirty: the composer may hold something: known text or doubt (knownText,
	// doubt). toldDirty: they were told known text blocks the switch.
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
	// Enter that confirms it (T6754, O56-3). draftLen counts the bytes
	// typed since the last Enter of any kind, popup or not (knownText).
	pasteTail []byte
	compLen   int
	draftLen  int
	lenKnown  bool
	popup     bool
	cleared   bool
	// hadAt: a popup trigger was typed since the last clean send; moved:
	// a cursor key or newline came since the latest trigger or Ctrl+U;
	// pastePop: the open paste's latest @ has no whitespace after it yet.
	hadAt, moved, pastePop bool
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
	// askShare's readings; share is the latest ask's, nil until it is in;
	// shareBusy: a read is in flight.
	warnDue, warned bool
	shareBusy       bool
	warnedWindow    string
	warnedMinutes   int64
	warnedReset     time.Time
	// shareRead is the ask the read in flight is for, started at
	// shareAt; a newer ask past shareStale starts its own (T6811).
	shareRead int
	shareAt   time.Time
	shares    chan shareReading
	share     *shareReading
	shareSeq  int
	// childChecks brings HasChild's answers; childSeq numbers its asks.
	childChecks chan childCheck
	childSeq    int
	// lastTurnEnd is the id of the record of the latest counted turn end
	// of the on-screen child, and lastLimit of the latest limit: the
	// causes of what each sets off (G67-2).
	lastTurnEnd, lastLimit string
	// followed is the session id the on-screen CLI moved to (followSession),
	// "" until then; host is the lineage of the shallowest hook seen naming
	// the on-screen session, the evidence for a move. Owned by the main loop.
	// afterEnter counts the bytes sent since the last Enter that sent or that a
	// popup took; a newline in the composer is not one. slashOnly: the composer
	// has held only a leading / command since the last clean send, and
	// slashEntered: its Enter has been pressed, so a move may clear that doubt.
	// moveRefused: a hook said it moved, and aggy could not confirm it.
	// escEmpty: an Esc reached an empty composer, and no text has been typed
	// since: an Enter now may bring text back. escTwice: two have, so keys that
	// look like text may be a preview's (Codex's backtrack), and only an Enter
	// ends it. escDoubt: such an Enter came. enterAtPrompt: the last Enter was
	// sent with the CLI at its prompt, so it may have started a turn (settleDoubt).
	followed                string
	host                    []int
	afterEnter              int
	slashOnly, slashEntered bool
	moveRefused             bool
	escEmpty, escTwice      bool
	// cmdLine is what was typed since the last Enter, while cmdBad is false:
	// printable keys only, no cursor key, no edit, no paste. localEnter: the
	// Enter just composed ended a line the vendor's docs list as starting no
	// model turn (adapter.LocalCommand).
	cmdLine            []byte
	cmdBad, localEnter bool
	// typedSince: text was typed or pasted, or a line recalled with Up, since the
	// last Enter, in an earlier read. Other cursor keys are not text: Down and
	// Enter answer an approval's menu.
	typedSince              bool
	escDoubt, enterAtPrompt bool
	// popupEnter: an Enter went at the prompt with a popup up, and no turn end has
	// come since, so a later Enter may be the one that sent the line. enterAt: when
	// the last Enter went (settleDoubt).
	popupEnter bool
	enterAt    time.Time
	// hookSeen: a hook of the vendor on screen came from the session.
	hookSeen atomic.Bool
	// foreign is, per session id a turn end named that was not the
	// on-screen one, when it was last recorded and how many came since
	// (G67-4); foreignOver is the same for all the sessions past
	// maxForeign (G67-R3). Only in memory; owned by the main loop.
	foreign     map[string]*foreignSeen
	foreignOver foreignSeen
	// shutdownSeen rate-limits the records of Grok's exit-time stop
	// (ignoredShutdown). Owned by the main loop.
	shutdownSeen foreignSeen
	// toolNotes wakes the main loop for a tools-changed notice; toolReads
	// brings Options.Tools' readings, toolBusy while one is in flight and
	// toolAgain when a notice came during it (T6729). toolSeen is the set
	// last acted on (nil: none since launch or since it went back as
	// launched); toolsDue: a note for a vendor with no hold signal waits
	// to be shown, naming toolsNote.
	toolNotes           chan struct{}
	toolReads           chan toolReading
	toolBusy, toolAgain bool
	toolSeen            ToolSet
	toolsDue            bool
	toolsNote           []string
	// toolsWaitTurn: a tools offer was turned down, so none is offered
	// again before the next turn end (L2). toolSeq numbers the reads,
	// toolRead is the one in flight, started at toolAt (L5).
	toolsWaitTurn     bool
	toolSeq, toolRead int
	toolAt            time.Time
	// guardSeen: OpenCode's guard has reported; asked holds the permissions
	// and questions it has seen open (opencode_unix.go).
	guardSeen  bool
	hangupKill *time.Timer // killAfterHangup
	// exitGap: OpenCode has the exit command's letters and waits for its
	// Enter; gapKeys are what the user typed since (finishExit). askStale: a
	// turn ended with an ask still open, so its reply was lost; askTold: the
	// user was told an ask holds the switch, so picking again releases it.
	exitGap           bool
	gapKeys           []byte
	askStale, askTold bool
	// slashLine: the composer's text began with /, a command. On OpenCode an
	// Enter runs it and leaves no text, unlike a popup that fills in a name.
	slashLine bool
	asked     map[string]bool
}

// turnCause is the cause of a switch's step at a turn end: the counted
// turn end, when it came after the switch's latest record (ids sort by
// time), else that record.
func (s *session) turnCause() string {
	if s.lastTurnEnd > s.sw.rec {
		return s.lastTurnEnd
	}
	return s.sw.rec
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
	// picked: the user chose this switch, in the picker or a note.
	picked bool
	// toldAt is when toldNoTurn was set: a pick goes stale (noTurnPickFresh).
	toldAt time.Time
	// toldNoTurn: the list this was picked from said to quit the CLI, so its
	// clean exit now completes the switch, unless they cancel it or a turn
	// end brings the normal offer (T6893). noteShown: tellNoTurn's note came
	// up for this pick.
	toldNoTurn, noteShown bool
	// toldDoubt: the note that aggy cannot tell if the prompt is empty came, and
	// no key has reached the composer since. doubtOK: the person picked again
	// after it, so the status line may ask, saying so.
	toldDoubt, doubtOK bool
	// resume: a restart of the vendor on screen on the same session, for
	// the tool changes named in tools (T6729).
	resume bool
	tools  []string
	// checkSeq is the ask of the HasChild check that runs for the
	// confirmed switch (0: none), started at checkAt (T6770).
	checkSeq int
	checkAt  time.Time
	// choice is what was picked or asked for with the vendor (T6665).
	choice Choice
	// rec is the id of the switch's latest diagnostic record, the cause
	// of its next.
	rec string
}

// hookEvent is a hook event from the session: its name and the vendor
// session id its payload names. shutdown marks a Grok Stop whose payload
// reason is "shutdown" (grokShutdown); idle, Claude's idle reminder
// (claudeIdle).
type hookEvent struct {
	name, sessionID string
	shutdown, idle  bool
	request         string // an OpenCode permission or question's id
	// source is a SessionStart's source; chain, the sender's lineage.
	source string
	chain  []int
}

// claudeIdle reports whether a hook event is Claude's idle reminder, which says
// nothing is waiting: hooks.md, Notification matcher, `idle_prompt` is "Claude
// finished responding about 60 seconds ago and you haven't typed since"
// (read 2026-11-07; not seen live, T6895). A permission prompt is its own type,
// `permission_prompt`. The type comes from the payload, which any process in
// the CLI's tree can forge, but this only keeps a new hold from starting: it
// clears no hold, so a forged one lets nothing through. Only Claude, and only
// that exact type: every other type holds, Grok's too, whose values are not
// recorded.
func claudeIdle(v adapter.Vendor, event, notificationType string) bool {
	return v == adapter.Claude && event == "Notification" && notificationType == "idle_prompt"
}

// grokShutdown reports whether a hook event is Grok's exit-time stop:
// grok 1.0.70 fires an extra stop hook with "reason":"shutdown" at every
// exit, interactive and headless, and a headless SIGINT gives only that
// one (live, 2026-10-29). It ends no turn, so hookEvent ignores it. The
// reason comes from the payload, which any process in the CLI's tree can
// forge; ignoring a turn end is the safe direction, and the reason does
// nothing else. Only Grok, and only Stop with that exact reason.
func grokShutdown(v adapter.Vendor, event, reason string) bool {
	return v == adapter.Grok && event == "Stop" && reason == "shutdown"
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
	if r.FromSession && r.Vendor == string(s.o.Vendor) {
		s.hookSeen.Store(true)
		p := hook.Parse(s.o.Vendor, r.Payload)
		ev := hookEvent{name: r.Event, sessionID: p.SessionID, shutdown: grokShutdown(s.o.Vendor, r.Event, p.Reason), request: p.Request,
			idle: claudeIdle(s.o.Vendor, r.Event, p.NotificationType), source: p.Source, chain: r.Chain}
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
// wait for the user, a permission prompt among others, so every one counts
// but Claude's idle reminder (claudeIdle). Grok's exit-time stop
// (grokShutdown) of the on-screen child itself is no turn end and holds
// nothing: it is only recorded as ignored. One that names no session or
// another one takes the turn-end paths below, so a forged one is still
// a security rejection (#101).
func (s *session) hookEvent(ev hookEvent) {
	if s.openCodeEvent(ev) {
		return
	}
	own := s.followed
	if own == "" && s.o.SessionID != nil {
		own = s.o.SessionID()
	}
	if own != "" && ev.sessionID == own && len(ev.chain) > 0 && (s.host == nil || len(ev.chain) < len(s.host)) {
		s.host = slices.Clone(ev.chain)
	}
	if ev.name == "SessionStart" {
		s.sessionStart(ev, own)
		return
	}
	if ev.shutdown && ev.sessionID != "" && ev.sessionID == own {
		s.ignoredShutdown()
		return
	}
	if turnEnd(ev.name) {
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
			s.turnRan = true
			s.sw.toldNoTurn = false
			if s.noTurnOpen {
				s.closePicker() // the offer takes over
			}
			if s.open && (s.pk.noTurn || s.pk.moveRefused || s.pk.mayRun) {
				// The list's words about no turn end, or about a move it could
				// not confirm, are over; the rows they took are blanked.
				s.pk.padRows = 0
				if s.pk.noTurn {
					s.pk.padRows += noTurnListRows
				}
				if s.pk.moveRefused {
					s.pk.padRows += moveRefusedRows
				}
				if s.pk.mayRun {
					s.pk.padRows += mayRunRows
				}
				if s.pk.doubtSaid {
					s.pk.padRows++
				}
				s.pk.noTurn, s.pk.moveRefused, s.pk.mayRun, s.pk.doubtSaid = false, false, false, false
				if s.pk.confirm == "" && !s.pk.note && s.pk.stage == "" {
					s.pk.draw(s.o.Out, s.pickerOverCLI)
				}
				s.pk.padRows = 0
			}
			s.moveRefused = false // it names the session on screen
			s.settleDoubt()
			s.popupEnter = false
			// Told of the doubt, and promised this turn end: its status line says
			// the doubt again and goes on from there. No key has come since.
			s.sw.doubtOK = s.sw.doubtOK || s.sw.toldDoubt
			s.atPrompt, s.inputSinceTurn, s.queuedWait = !s.held, false, false
			s.askStale = s.askOpen()
			if s.queued > 0 {
				s.queued--
				s.atPrompt, s.queuedWait = false, true
			}
			s.toolsWaitTurn = false
			s.warnDue = true
			s.askShare()
			if ev.name == "Stop" && holdSignal(s.o.Vendor) && s.pk != nil {
				s.pk.preselect = "" // a turn ended without a limit (O56-5)
				s.limitDue = nil
			}
		}
		return
	}
	if ev.idle {
		s.log(diag.Rec{Event: diag.EvHookAccepted, Attrs: map[string]any{"event": hookName(ev.name), "holds": false}})
		return
	}
	s.log(diag.Rec{Event: diag.EvHookAccepted, Attrs: map[string]any{"event": hookName(ev.name), "holds": true}})
	s.atPrompt, s.held = false, true
}

// sessionMoves reports whether a SessionStart of v with this source says the
// CLI on screen moved to another session. Claude: "resume" for a resumed
// session, "clear" after /clear, "fork" for a new session forked from an
// existing one (hooks.md, SessionStart input; SessionEnd's reasons say the old
// session ends "with /clear" and "via interactive /resume", read 2026-11-07).
// Codex: the same values (session-start.command.input.schema.json at
// rust-v0.164.0; which of its commands sends which is unverified). "startup"
// and "compact" stay in the session, or are a nested CLI's. Grok's payload has
// no source that is recorded, so any SessionStart may be a move there, and only
// its lineage says otherwise (unverified; what would confirm it: T6896's /clear
// on Grok under aggy, and the id its next Stop names). A vendor with another way
// to see a session change (OpenCode has no SessionStart, T6953) calls
// followSession with its own evidence, so a SessionStart sent to OpenCode's
// socket moves nothing.
func sessionMoves(v adapter.Vendor, source string) bool {
	switch v {
	case adapter.Grok:
		return true
	case adapter.OpenCode:
		return false
	}
	return source == "clear" || source == "resume" || source == "fork"
}

// followsFrom reports whether a hook with this lineage ran where the CLI's own
// hooks run: as deep below the CLI as the shallowest hook that named the
// on-screen session, and through the same processes above its own shell. A
// nested claude -p or codex exec runs below the agent's shell tool, and its
// hooks below its own process, so they are deeper, and another process's. That
// is why the check exists: the id is what keeps a nested CLI's Stop from
// counting, so it must not move on that nested CLI's word.
//
// It does not stop a tool that pipes JSON into aggy hook from the same depth:
// such a tool can as well forge a Stop naming the id it can read in the CLI's
// arguments, so this gives it nothing new. What the id then does is count turn
// ends, and a switch still asks the user. Recording the id for a later resume
// needs a session file besides (internal/cli, childIDs).
func (s *session) followsFrom(chain []int) bool {
	above := func(c []int) []int { return c[min(2, len(c)-1):] }
	return len(s.host) > 0 && len(chain) == len(s.host) && slices.Equal(above(chain), above(s.host))
}

// sessionStart takes a SessionStart hook. Naming the session on screen, or no
// other, it means nothing here; naming another, with a source that says the CLI
// moved, it is followed if it ran where the CLI's own hooks run.
func (s *session) sessionStart(ev hookEvent, own string) {
	if ev.sessionID == "" || ev.sessionID == own || own == "" || !sessionMoves(s.o.Vendor, ev.source) {
		return
	}
	if adapter.ValidSessionID(s.o.Vendor, ev.sessionID) != nil || !s.followsFrom(ev.chain) {
		s.log(diag.Rec{Event: diag.EvHookIgnored, Code: diag.CodeFollowRefused, Msg: "a session start that says the CLI moved, from outside where its own hooks run",
			Attrs: map[string]any{"event": hookName(ev.name), "source": safeSource(ev.source), "known_host": s.host != nil}})
		s.moveRefused = true
		return
	}
	via := "session_start"
	if ev.source != "" {
		via = safeSource(ev.source)
	}
	s.followSession(ev.sessionID, via)
}

// safeSource is a source for a record: any process in the tree can pick it.
func safeSource(source string) string {
	switch source {
	case "startup", "resume", "clear", "compact", "fork":
		return source
	}
	return "other"
}

// followSession makes id the on-screen session, on the evidence via: a turn end
// counts only if it names id from now on. It says nothing about whether a turn
// runs: Codex sends SessionStart inside run_turn, so the move can come with the
// first message after /clear already running (core/src/session/turn.rs#L340 at
// 6aaac694, source only, not seen live). So atPrompt, inputSinceTurn, and the
// queue stay as they were, and the next turn end, or a pick that says no turn
// runs, settles them; the list says so. The one doubt a move does
// settle is the one its own command left: a composer that held only that leading
// / command, with nothing typed since its Enter, has been emptied by the command.
func (s *session) followSession(id, via string) {
	s.followed, s.moveRefused = id, false
	s.log(diag.Rec{Event: diag.EvSessionFollowed, Attrs: map[string]any{"via": via}})
	if s.afterEnter == 0 && s.slashOnly && s.slashEntered {
		s.compLen, s.lenKnown, s.popup, s.cleared, s.hadAt, s.moved, s.pastePop = 0, true, false, false, false, false, false
		s.slashOnly, s.slashEntered = false, false
		s.toldDirty = false
		s.updateDirty()
	}
	if f := s.o.OnFollow; f != nil {
		s.goSafe(func() { f(id, via) })
	}
}

// maxSwitchTools caps the tools a switch-request may name.
const maxSwitchTools = 8

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
	if s.o.HooksMissing && !s.hookSeen.Load() {
		return coded(diag.CodeHooksMissing, fmt.Errorf("aggy's hooks for %s are not installed, so aggy would never see a turn end to switch at; run aggy hooks install, then start a new session", s.o.Vendor))
	}
	if r.New || r.Model != "" {
		// O56-1: a new session or a model is the user's choice, in the
		// picker; the agent may ask for a vendor and a brief only.
		return coded(diag.CodeAgentOptions, errors.New("an agent may ask for a vendor, a brief, and tools only; a new session or a model for the launch is the user's choice, in aggy's picker"))
	}
	// Tools for the next launch: names only, shown to the user in the
	// confirmation, which takes or cancels the whole switch. Whether each
	// is defined, applies, and is not in the repo's tools_off is decided
	// at that launch, as for aggy run --tool.
	if len(r.Tools) > maxSwitchTools || slices.ContainsFunc(r.Tools, func(n string) bool { return !tooldef.ValidName(n) }) {
		return coded(diag.CodeUsage, fmt.Errorf("--tool takes at most %d tool names, each lowercase letters, digits, and -", maxSwitchTools))
	}
	if err := s.offered(v); err != nil && !errors.Is(err, ErrChecking) {
		return coded(diag.CodeSwitchVendor, err)
	}
	if !s.waiting.CompareAndSwap(false, true) {
		return coded(diag.CodeSwitchBusy, errors.New("a switch is already waiting"))
	}
	// The only one past the swap; the channel holds one.
	s.asks <- Choice{Vendor: v, Brief: r.Brief, Tools: slices.Clone(r.Tools)}
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

// resumable says why the vendor on screen cannot be resumed now (the
// repo's allowed_vendors, its CLI gone from PATH, the safety policy), or
// nil (T6729).
func (s *session) resumable(v adapter.Vendor) error {
	if s.o.CanSwitch == nil {
		return errors.New("this session cannot resume")
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
// too (T6754). After an @, a leading / or Ctrl+R (history search) (T6754,
// O56-3, T6770) an Enter may only accept an entry in the CLI's popup and
// leave the text, with more than was typed, in the composer: from then on
// every Enter counts as unsent until one on a line the user explicitly
// emptied, with Ctrl+U (unverified: it is the readline kill-line key and
// is assumed to clear the composer line) or with backspaces down to zero
// while aggy still knew the length. Either counts only if no cursor key
// or newline came since the @ (or since the last Ctrl+U), and a Ctrl+U
// only if it cleared text: the @ may then be on another line or after
// the cursor, and survive (T6770). A cursor key, yank, undo, or a Ctrl+U
// on an empty line after any @ of the draft may put the cursor back in
// its token, so it opens the popup mark again. A paste's @ followed by whitespace in
// the same paste leaves the cursor outside its token (unverified: assumed
// to end the token, as typing a space does), so it does not set the mark.
// When in doubt it stays dirty.
// TODO(O56-3): aggy installs no UserPromptSubmit hook yet; a turn-start
// hook could confirm a send, but any process in the CLI's tree can send
// one, so it would need care before clearing this.
// shortcut: a CLI that turns on kitty's all-keys-as-escapes mode gets
// Enter as CSI 13 u, which never clears it, so it cannot be switched
// from by aggy. Upgrade if T6684 finds one.
func (s *session) send(b []byte) {
	if terminalReport(b) {
		s.toChild.push(b) // the terminal answering the CLI, not the user
		return
	}
	if mouseOnly(b) {
		s.pointer(b)
		return
	}
	typed := s.typedOf(b)
	if len(typed) == 0 && len(b) > 0 {
		s.toChild.push(b) // the terminal's own reports: no typing at all
		return
	}
	if s.exitGap {
		s.gapKeys = append(s.gapKeys, b...) // finishExit decides where they go
		return
	}
	s.lastIn = time.Now()
	s.held = false
	if len(b) == 0 {
		return
	}
	s.inputSinceTurn = true
	enter := bytes.IndexByte(typed, '\r') >= 0
	if enter || csiEnter.Match(typed) {
		// An Enter with text, while a turn may be running, may queue a
		// message. A bare one on an empty prompt queues nothing: it answers
		// an approval or a menu, which most turns have.
		if !s.atPrompt && (s.typedSince || !bareEnter(typed)) {
			s.queued++
			s.queuedWait = false
		}
		s.enterAtPrompt = s.atPrompt && !s.askOpen()
		s.atPrompt, s.toldDirty = false, false // an Enter may start a turn
	} else if tabKey.Match(typed) && (s.dirty || string(tabKey.Find(typed)) != string(typed)) {
		// Tab with text queues a message on Codex during a turn:
		// https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/tui/src/bottom_pane/chat_composer.rs#L3531
		// It may do something else elsewhere; the quiet gap covers that. Idle on
		// Codex it may start a turn, as an Enter may. Either way the text stays
		// in doubt: idle, Tab does not submit a ! command (the same lines, and
		// the same file's tab_does_not_submit_for_bang_shell_command, L10170), a
		// $ popup takes it (L2255, read by review), and another CLI may complete
		// a word. Unsent text under /exit is the worse mistake, so a message Tab
		// did queue leaves the unsent-text note until Ctrl+U and an Enter.
		switch {
		case !s.atPrompt:
			s.queued++
			s.queuedWait = false
		case s.o.Vendor == adapter.Codex && !s.popup:
			s.atPrompt = false
		}
	}
	// Keys answering OpenCode's dialog never reach its composer. Without this
	// an Esc or an arrow there left the switch waiting on "unsent text" (live).
	if !s.askOpen() {
		s.followKeys(typed)
	}
	s.toChild.push(b)
}

// followKeys follows keys that reach the composer, past any dialog.
func (s *session) followKeys(typed []byte) {
	enter := bytes.IndexByte(typed, '\r') >= 0
	// Text after a command's Enter is no longer just the command. A key sequence
	// is not text: /resume's list takes arrows and an Enter.
	if typed[0] != esc && s.slashEntered && string(typed) != "\r" {
		s.slashOnly = false
	}
	s.sw.toldDoubt, s.sw.doubtOK = false, false
	switch {
	case enter:
		s.typedSince = false
	case typed[0] != esc || bytes.Contains(typed, pasteStart) || upKey.Match(typed):
		// Up and an Enter may send a line from history. On an approval's menu they
		// only answer it, and a pick then waits falsely, for the next turn end or
		// the queue's quiet: the safe side, and rare.
		s.typedSince = true
	}
	onlyEsc := isEsc(typed) || len(bytes.Trim(typed, "\x1b")) == 0
	switch {
	case onlyEsc:
		if s.escEmpty || s.compLen == 0 {
			s.escTwice = s.escEmpty || len(typed) > 1 && !isEsc(typed)
			s.escEmpty = true
		}
	case typed[0] != esc && typed[0] != '\r' && !s.escTwice:
		// The everyday interrupt: an Esc, then a message. After two Escs on an
		// empty composer Codex is in its backtrack preview, whose keys include
		// h, l, j, k and Ctrl+T (app_backtrack/legacy_input.rs#L195 at 6aaac694),
		// so they end nothing there.
		s.escEmpty = false
	}
	before, beforeDraft := s.compLen, s.draftLen
	for _, c := range typed {
		s.compose(c)
	}
	if onlyEsc {
		s.compLen, s.draftLen = before, beforeDraft // an Esc types nothing
	}
	if typed[0] != esc && enter && bytes.IndexByte(typed, '\r') < len(typed)-1 {
		s.slashOnly, s.localEnter = false, false // more came after the Enter, in this read
	}
	if s.localEnter {
		// It starts no turn, so the prompt it was typed at is still the prompt,
		// and the next message is an ordinary Enter there. Any doubt stays until
		// a turn end: it may have opened a dialog, and aggy cannot see one.
		s.localEnter = false
		s.atPrompt = s.enterAtPrompt
	}
	if s.escEmpty && enter {
		// On an empty composer Codex takes Esc, Esc, Enter as a backtrack, which
		// puts an earlier prompt back in the composer (tui/src/app_backtrack.rs
		// at 6aaac694, source only, not seen live), with cursor keys allowed
		// in between. Another CLI may do alike, and aggy does not read the screen.
		s.escDoubt, s.escEmpty, s.escTwice = true, false, false
	}
	s.updateDirty()
}

// knownText reports characters aggy counted and has not seen sent: any since the
// last Enter, popup or not, so `hi @bob` and a line typed after /cost are known.
// The exit command would send them, so they block a switch until sent or cleared.
// Keys typed into a dialog aggy cannot see (a /model picker's arrows) count too,
// and block falsely: the safe side, with Ctrl+U and an Enter as the way out.
func (s *session) knownText() bool {
	return s.draftLen > 0 || s.lenKnown && s.compLen > 0 && !s.popup
}

// doubt reports that the composer may hold text aggy did not count, or that it
// cannot say what its keys did: a popup may have taken the last Enter, a command
// may have left text, and an Esc on an empty composer may bring text back. The
// person can see the screen and aggy cannot, so this does not block: they are
// told, and confirm (offerSwitch).
func (s *session) doubt() bool {
	return !s.knownText() && (!s.lenKnown || s.popup || s.cleared || s.escEmpty || s.escDoubt)
}

func (s *session) updateDirty() {
	s.dirty = !s.lenKnown || s.compLen > 0 || s.popup || s.cleared || s.escDoubt
}

// settleTurnGap is how long after the last Enter a turn end must come to say
// anything about that Enter: sooner, it is more likely an interrupted turn's late
// Stop than the end of a turn the Enter started. A variable for tests.
var settleTurnGap = time.Second

// settleDoubt takes a counted turn end as evidence about the composer. With
// nothing sent since the last Enter, an Esc before that Enter brought nothing
// back that a turn did not then take. If that Enter also went at the prompt, or
// an earlier one did with a popup up and no turn end has come since, a turn ran
// from them, so they sent what was there: the composer is empty.
//
// That can be wrong: a turn the Enter did not start, such as one a background
// task starts, can end a second or more after a popup took the Enter. The
// composer then holds text aggy calls empty, and only the status line's "See its
// prompt?" is left (docs/design.md, TestTurnEndEvidence). An Enter sent while a
// turn runs is no evidence: it may have answered an approval, or queued.
func (s *session) settleDoubt() {
	if s.afterEnter != 0 || time.Since(s.enterAt) < settleTurnGap {
		return
	}
	s.escEmpty, s.escTwice, s.escDoubt = false, false, false
	if s.enterAtPrompt || s.popupEnter {
		s.compLen, s.lenKnown, s.popup, s.cleared, s.hadAt, s.moved, s.pastePop = 0, true, false, false, false, false, false
		s.slashOnly, s.slashEntered = false, false
	}
	s.updateDirty()
}

// pointer passes on a click or a drag. It may do something in the CLI, so it
// counts as input (an approval answered with the mouse, a turn started), but it
// types nothing. It may move the cursor, as a cursor key does.
//
// On OpenCode a click is no typing: its plugin reports what a click answered, so
// the click clears no hold (typedOf). In the gap between the exit command's
// letters and its Enter it waits with the keys, as it could land on a dialog or
// move the cursor out of the composer before that Enter (finishExit).
func (s *session) pointer(b []byte) {
	if s.exitGap {
		s.gapKeys = append(s.gapKeys, b...)
		return
	}
	if s.o.Vendor == adapter.OpenCode {
		s.toChild.push(b)
		return
	}
	s.lastIn = time.Now()
	s.held = false
	s.inputSinceTurn = true
	s.moved = true
	s.slashOnly = false
	s.afterEnter++
	s.sw.toldDoubt, s.sw.doubtOK = false, false
	if s.hadAt {
		s.popup = true // the cursor may be back in an @ token
	}
	// TODO(T6895): a click may start a turn or fill the composer; only
	// inputSinceTurn says so, and an open switch can still fire on it.
	s.updateDirty()
	s.toChild.push(b)
}

// splitWait is how long an escape sequence split across reads may wait for the
// rest of it. Terminals write one reply at a time, so this only has to cover
// the gap between two reads of one write. A variable for tests.
var splitWait = 50 * time.Millisecond

// csiEnter matches Enter in the kitty keyboard protocol (CSI 13 u, with any
// modifiers or event type) and in xterm's modifyOtherKeys (CSI 27 ; m ; 13 ~).
// Which encoding Claude's and Grok's terminals are put in is unverified.
var csiEnter = regexp.MustCompile(`\x1b\[((13|57414)(:[0-9]*)*(;[0-9:]*)*u|27;[0-9]+;13~)`)

// tabKey matches a plain Tab press, as a byte or in the kitty keyboard protocol
// (with no modifier but Caps or Num Lock, and a press or repeat): Shift+Tab,
// Ctrl+Tab, and a release queue nothing.
var tabKey = regexp.MustCompile(`\t|\x1b\[9(;(1|65|129|193)(:[12])?)?u`)

// upKey matches a plain Up press: CSI, application mode, or the kitty form with
// no modifier but Caps or Num Lock.
var upKey = regexp.MustCompile(`\x1b(\[(1;(1|65|129|193)(:[12])?)?|O)A`)

// bareEnter reports whether b is one Enter and nothing else.
func bareEnter(b []byte) bool {
	return string(b) == "\r" || string(csiEnter.Find(b)) == string(b)
}

// queuedQuiet is how long the CLI and the typing must be silent to release a
// turn end that had a message queued. Without it, an Enter with text that
// queued nothing (an answer to the CLI's question) would stop every later
// notice and offer, since no further turn end comes. That a running turn keeps
// drawing, so that this silence means none runs, is unverified on all three
// CLIs; if one runs silent this long a notice can hold its output. A variable
// for tests.
var queuedQuiet = 10 * time.Second

// releaseQueued ends a wait for a queued turn that shows no sign of running.
func (s *session) releaseQueued() {
	now := time.Now()
	if !s.queuedWait || now.Sub(s.lastIn) < queuedQuiet || now.Sub(time.Unix(0, s.lastOut.Load())) < queuedQuiet {
		return
	}
	s.queued, s.queuedWait = 0, false
	s.atPrompt, s.inputSinceTurn = !s.held, false
}

// mayBeRunning reports whether a turn may be running, so a notice that can wait
// must, since an open notice holds the CLI's output until a key. Only a launch
// or turn end with no input since, and no sign the CLI is busy, says none is.
func (s *session) mayBeRunning() bool {
	return !s.atPrompt || s.inputSinceTurn
}

// cursorKey reports whether c moves the cursor (or starts an escape
// sequence that may), or may bring text back: ESC, Ctrl+A, B, E, F, N,
// P, yank (Ctrl+Y) and undo (Ctrl+_).
func cursorKey(c byte) bool {
	switch c {
	case esc, 0x01, 0x02, 0x05, 0x06, 0x0e, 0x10, 0x19, 0x1f:
		return true
	}
	return false
}

// maxCmdLine bounds cmdLine: no vendor's listed command is near it.
const maxCmdLine = 32

// compose follows one byte the user sent (see send).
func (s *session) compose(c byte) {
	prev := s.lastByte
	s.lastByte = c
	s.afterEnter++
	switch {
	case c == '\r':
	case c < 0x20 || c > 0x7e || s.inPaste || len(s.cmdLine) >= maxCmdLine:
		s.cmdBad = true
	default:
		s.cmdLine = append(s.cmdLine, c)
	}
	// The paste markers, found with the bytes before, so one split
	// across reads still counts.
	s.pasteTail = append(s.pasteTail, c)
	if bytes.HasSuffix(s.pasteTail, pasteStart) {
		s.inPaste, s.pastePop = true, false
	} else if bytes.HasSuffix(s.pasteTail, pasteEnd) {
		s.inPaste = false
		if s.pastePop {
			s.popup, s.pastePop = true, false // the cursor ends in the token
		}
	}
	if n := len(pasteEnd); len(s.pasteTail) >= n {
		s.pasteTail = append(s.pasteTail[:0], s.pasteTail[len(s.pasteTail)-n+1:]...)
	}
	if !s.inPaste && cursorKey(c) {
		s.moved = true
		if s.hadAt {
			s.popup = true // the cursor may be back in an @ token
		}
	}
	switch {
	case c == '\r' && prev == esc && s.compLen == 0 && !s.inPaste:
		// An Esc and an Enter on an empty composer, or Option+Enter split across
		// reads: it may bring text back, or answer an approval. Doubt, which a
		// turn end can settle, and no counted text.
		s.escDoubt, s.moved, s.pastePop, s.slashOnly = true, true, false, false
		s.draftLen, s.afterEnter = 0, 0
		s.cmdLine, s.cmdBad = s.cmdLine[:0], false
	case c == '\r' && (prev == esc || prev == '\\' || s.inPaste) || c == '\n':
		// A newline in the composer. An Esc just before counts however long ago
		// it came, as Option+Enter split across reads looks the same.
		s.compLen++
		s.draftLen++
		s.moved, s.pastePop, s.slashOnly = true, false, false
	case c == '\r' && s.popup && s.slashLine && s.o.Vendor == adapter.OpenCode:
		// A command runs and leaves no text (/models opens a dialog, live).
		s.compLen, s.lenKnown, s.cleared, s.hadAt, s.moved, s.popup, s.slashLine = 0, true, false, false, false, false, false
		s.draftLen, s.afterEnter = 0, 0
		s.slashOnly, s.slashEntered, s.escDoubt = false, false, false
	case c == '\r' && s.popup:
		s.lenKnown = false // the popup may have taken it and added text
		s.popupEnter = s.popupEnter || s.enterAtPrompt
		s.draftLen, s.afterEnter = 0, 0
		s.slashEntered = s.slashOnly
		s.localEnter = s.slashOnly && !s.moved && !s.cmdBad && adapter.LocalCommand(s.o.Vendor, string(s.cmdLine))
		s.cmdLine, s.cmdBad = s.cmdLine[:0], false
	case c == '\r':
		// TODO(T6895): an Enter that answers an approval leaves a draft under it in
		// the composer, and aggy counts it as sent. Only "See its prompt?" is left.
		s.compLen, s.lenKnown, s.cleared, s.hadAt, s.moved = 0, true, false, false, false // sent
		s.draftLen, s.popupEnter, s.afterEnter = 0, false, 0
		s.slashOnly, s.slashEntered, s.escDoubt = false, false, false
		s.cmdLine, s.cmdBad = s.cmdLine[:0], false
	case c == ctrlU:
		// The line is empty, but it counts only once an Enter confirms
		// it: in a multiline draft Ctrl+U may clear one line only. Only
		// one that clears text, with no cursor key since, clears the
		// mark: on an empty line some composers join the line before,
		// which puts the cursor back in its @ token.
		switch {
		case s.compLen == 0:
			s.popup = s.popup || s.hadAt
		case !s.moved:
			s.popup = false
		}
		s.compLen, s.lenKnown, s.cleared, s.moved = 0, true, true, false
		s.draftLen = 0
	case c == 0x7f || c == 0x08:
		if s.draftLen > 0 {
			s.draftLen--
		}
		if s.compLen > 0 {
			s.compLen--
			if s.compLen == 0 && s.lenKnown && s.popup && !s.moved {
				s.popup, s.cleared = false, true // hadAt stays: an undo may bring it back
			}
		}
	default:
		switch {
		case c == '@' || c == ctrlR || c == '/' && s.compLen == 0:
			s.slashLine = c == '/' && !s.hadAt
			s.slashOnly = c == '/' && !s.hadAt && !s.inPaste
			s.hadAt, s.moved = true, false
			if s.inPaste {
				s.pastePop = true
			} else {
				s.popup = true
			}
		case s.inPaste && (c == ' ' || c == '\t' || c == '\n'):
			s.pastePop = false
		}
		s.compLen++
		s.draftLen++
	}
	if s.afterEnter == 0 {
		s.enterAt = time.Now()
	}
}

// offerSwitch asks the user to confirm the waiting switch once the CLI
// is at its prompt and quiet. The vendor is checked again first: the
// repo config or PATH may have changed since the pick.
func (s *session) offerSwitch() {
	now := time.Now()
	if !s.turnRan && !s.sw.picked {
		return // an agent's, a limit's, or a tool change's: only a turn end offers it
	}
	// A notice must never hold output while a turn may be running, and
	// without a turn end only a launch given no input and no brief has none.
	// The list this was picked from has already told the user (markNoTurn).
	if !s.turnRan && s.mayBeRunning() {
		return
	}
	if s.askOpen() {
		if s.askStale && !s.askTold {
			s.tellAskWaits(s.sw.to)
		}
		return
	}
	if s.turnRan && !s.atPrompt {
		return // the list said so: a notice here could hold a turn's output
	}
	if s.refuseSignalled() {
		return
	}
	if now.Sub(s.lastIn) < switchQuiet || now.Sub(time.Unix(0, s.lastOut.Load())) < switchQuiet {
		return
	}
	if s.sw.resume && s.toolsWaitTurn {
		return // one turned down at this turn end: the next one asks (L2)
	}
	if s.sw.checkSeq != 0 {
		if time.Since(s.sw.checkAt) < childCheckWait {
			return // confirmed: the picked session is being checked
		}
		// Offered again: a late answer must not type the exit command
		// past this offer.
		s.sw.checkSeq, s.sw.checkAt = 0, time.Time{}
	}
	v, on := s.sw.to, s.o.Vendor
	what := "switch to " + string(v)
	if s.sw.resume {
		what = fmt.Sprintf("resume %s with the changed tools", v)
	}
	if s.knownText() && s.turnRan {
		// S-1: the exit command would go after the unsent text and send
		// both, so the switch waits; the user is told once.
		if !s.toldDirty {
			s.toldDirty = true
			s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeUnsentText, Cause: s.turnCause(),
				Msg: "the switch waits: text the user typed is not sent", Attrs: map[string]any{"to": string(v)}})
			again := fmt.Sprintf("next turn end, or when you pick %s again after that Enter.", v)
			if s.sw.resume {
				again = "next turn end."
			}
			s.pk.tell(fmt.Sprintf("aggy is ready to %s, but you typed text you have not sent to %s.", what, on),
				"Send it, or clear it (Ctrl+U or Backspace) and press Enter on the empty prompt; aggy asks again at the",
				again)
			s.openPicker()
		}
		return
	}
	if s.turnRan && s.doubt() && !s.sw.doubtOK {
		// A tool change's resume has no re-pick: its status line says it alone.
		if !s.sw.resume {
			if !s.sw.toldDoubt {
				s.sw.toldDoubt = true
				s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodePromptDoubt, Cause: s.turnCause(),
					Msg: "the switch waits: aggy cannot tell if the prompt is empty", Attrs: map[string]any{"to": string(v)}})
				s.pk.tell(doubtLines(what, on, v)...)
				s.openPicker()
			}
			return
		}
		s.sw.doubtOK = true
	}
	check := s.offered
	if s.sw.resume {
		check = s.resumable
	}
	if err := check(v); errors.Is(err, ErrChecking) {
		return // offered once the check is done
	} else if err != nil {
		s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeSwitchVendor, Cause: s.turnCause(), Msg: refusedMsg(v, err), Attrs: map[string]any{"to": string(v), "resume": s.sw.resume}})
		s.setSwitch(pendingSwitch{})
		s.pk.tell(fmt.Sprintf("Cannot %s: %v", what, err))
		s.openPicker()
		return
	}
	if !s.turnRan {
		s.tellNoTurn(what)
		return
	}
	// One status line (S-12, O56-1): who or what asked and the switch's
	// options first, then the keys, then the check; a narrow terminal
	// cuts the end.
	cmd, verified := adapter.ExitCommand(on)
	tools := "tools " + strings.Join(s.sw.tools, " ")
	// short is how far a doubt's line is shortened (the fit below).
	short := 0
	inDoubt := s.turnRan && s.doubt()
	confirmLine := func(model, tools string) string {
		var why []string
		if inDoubt {
			why = append(why, []string{"can't tell if the prompt is empty", "can't tell if prompt is empty", "can't tell if empty"}[short])
		}
		switch {
		case s.sw.resume:
			why = append(why, tools)
		case s.sw.asked:
			why = append(why, "the agent asks")
		case s.sw.limit:
			why = append(why, limitWho(LimitHit{Vendor: on, Unverified: s.sw.unverified}))
		}
		c := s.sw.choice
		c.Model = model
		notes := choiceNote(c)
		if short == 2 && c.New {
			notes[0] = "new"
		}
		why = append(why, notes...)
		head, does, sep := "-> "+string(v), "switches", " " // the full line fits 80 columns with no origin or doubt
		if s.sw.resume {
			head, does, sep = "same session -> "+string(v), "resumes", ", "
		}
		if len(why) > 0 {
			head = strings.Join(why, ", ") + sep + head
		}
		line := fmt.Sprintf("%s. %s %s, Esc stays. See %s's prompt? It types %s.", head, keyName(s.pk.key), does, on, cmd)
		if short > 0 {
			line = fmt.Sprintf("%s. %s types %s, %s", head, keyName(s.pk.key), cmd, keysEnd)
		}
		if !verified {
			line += fmt.Sprintf(" (%s is unverified.)", cmd)
		}
		return line
	}
	line := confirmLine(s.sw.choice.Model, tools)
	lines := []string{line}
	s.sw.rec = s.log(diag.Rec{Event: diag.EvSwitchOffered, Cause: s.turnCause(), Attrs: switchAttrs(s.sw.choice, map[string]any{"to": string(v), "asked": s.sw.asked, "limit": s.sw.limit, "resume": s.sw.resume})})
	if !s.sw.resume {
		s.pk.preselect = "" // offered once (O56-5); a limit's stays past a resume
	}
	s.pk.ask(v, lines...)
	if s.sw.resume {
		// The tools' names go first when the keys would not fit.
		s.pk.fit = func(cols int) string {
			full := "aggy: " + confirmLine(s.sw.choice.Model, tools)
			if cols <= 1 || strings.Index(full, keysEnd)+len(keysEnd) <= cols-1 {
				return full
			}
			return "aggy: " + confirmLine("", "tools changed")
		}
	}
	if m := s.sw.choice.Model; m != "" {
		// T6770: a long model name (up to maxModel) is cut first, so the
		// keys stay on the line; past them a narrow terminal cuts the end.
		s.pk.fit = func(cols int) string {
			full := "aggy: " + confirmLine(m, tools)
			over := strings.Index(full, keysEnd) + len(keysEnd) - (cols - 1)
			if cols <= 1 || over <= 0 {
				return full
			}
			keep := max(len(m)-over-len("..."), 1)
			if keep >= len(m) {
				return full
			}
			return "aggy: " + confirmLine(m[:keep]+"...", tools)
		}
	}
	if inDoubt {
		// In doubt the person must see that the key types into that prompt, so the
		// command moves in front of the end of the keys, and the line shortens until
		// they fit: at 80 columns for any vendor a switch can go to (OpenCode is only
		// ever on screen), with new and brief on.
		base := s.pk.fit
		s.pk.fit = func(cols int) string {
			defer func() { short = 0 }()
			var l string
			for short = range 3 {
				l = "aggy: " + confirmLine(s.sw.choice.Model, tools)
				if base != nil {
					l = base(cols)
				}
				if cols <= 1 || short == 0 && len(l) <= cols-1 {
					return l
				}
				if end := strings.Index(l, keysEnd) + len(keysEnd); short > 0 && end <= cols-1 {
					if len(l) > cols-1 {
						l = l[:end] // no half of the note on /exit being unverified
					}
					return l
				}
			}
			return l
		}
	}
	s.openPicker()
}

// tellNoTurn repeats, once per pick, what the list said, with room to explain,
// for someone who picked without reading it. offerSwitch comes here only when
// nothing can be running, so this note holds no turn's output. what is the
// switch, as a verb phrase.
func (s *session) tellNoTurn(what string) {
	if s.sw.noteShown {
		return
	}
	s.sw.noteShown, s.sw.toldNoTurn = true, true
	if s.sw.toldAt.IsZero() {
		s.sw.toldAt = time.Now()
	}
	on := s.o.Vendor
	cmd, _ := adapter.ExitCommand(on)
	// Each line fits 80 columns: over the CLI's screen a longer one is cut.
	s.pk.tell(fmt.Sprintf("aggy is ready to %s,", what),
		fmt.Sprintf("but it has seen no turn end from %s since it opened.", on),
		"It may be showing a dialog (an update or trust prompt),",
		fmt.Sprintf("so aggy will not type %s into it.", cmd),
		fmt.Sprintf("Quit %s yourself within %d minutes and aggy will %s,", on, noTurnPickMinutes, what),
		"if it exits cleanly.",
		"If you send a message instead, aggy asks at that turn's end if it sees one;",
		"some CLIs report none until their hook is approved.",
		fmt.Sprintf("To cancel, press %s, then Esc in the list.", keyName(s.pk.key)))
	s.openPicker()
	s.noTurnOpen = true
}

// noTurnPickMinutes is how long a pick told to quit the CLI stays good. Where
// no turn end is ever counted (Codex with no approved hook) nothing else ends
// it, and a quit long after, for another reason, should not land in another CLI.
const noTurnPickMinutes = 10

// noTurnPickFresh is noTurnPickMinutes; tests shorten it.
var noTurnPickFresh = noTurnPickMinutes * time.Minute

// markNoTurn notes a pick from a list that said to quit the CLI (openList):
// with no turn ended, its clean exit now completes the switch.
func (s *session) markNoTurn() {
	if s.turnRan {
		return
	}
	s.sw.toldNoTurn, s.sw.toldAt = true, time.Now()
	s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeNoTurnYet, Cause: s.sw.rec,
		Msg: "no turn has ended since the CLI launched, so aggy does not type its exit command", Attrs: map[string]any{"to": string(s.sw.to)}})
}

// exitSwitches says whether the CLI's exit with code completes the waiting
// switch, and tells the user why not when they picked one. A typed exit
// command completes it unless aggy has got a signal. So does a clean exit
// the list said to make, with no signal, a session picked still recorded (as
// confirmSwitch checks), and the vendor still allowed (as offerSwitch checks).
func (s *session) exitSwitches(code int) bool {
	v, on := s.sw.to, s.o.Vendor
	if v == "" {
		return false
	}
	what := "switch to " + string(v)
	if s.sw.resume {
		what = "resume " + string(v)
	}
	if s.sw.typed && !s.signalled {
		return true
	}
	if !s.sw.typed && !s.sw.toldNoTurn {
		if s.sw.picked {
			s.exitLine(fmt.Sprintf("%s exited before aggy offered to %s, so it did not.", on, what))
		}
		return false
	}
	if s.signalled {
		return s.signalStops(code)
	}
	if code != 0 {
		s.log(diag.Rec{Event: diag.EvSwitchCancelled, Cause: s.sw.rec, Exit: diag.Int(code), Attrs: map[string]any{"how": "unclean_exit", "to": string(v), "signalled": false}})
		s.exitLine(fmt.Sprintf("%s exited with code %d, so aggy did not %s.", on, code, what))
		return false
	}
	if !s.sw.typed && time.Since(s.sw.toldAt) > noTurnPickFresh {
		s.log(diag.Rec{Event: diag.EvSwitchCancelled, Cause: s.sw.rec, Exit: diag.Int(code), Attrs: map[string]any{"how": "stale_pick", "to": string(v)}})
		s.exitLine(fmt.Sprintf("you picked %s over %d minutes ago, so aggy did not %s. Open aggy again to switch.", v, noTurnPickMinutes, what))
		return false
	}
	check := s.offered
	if s.sw.resume {
		check = s.resumable
	}
	if err := check(v); err != nil && !errors.Is(err, ErrChecking) {
		s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeSwitchVendor, Cause: s.sw.rec, Msg: refusedMsg(v, err), Attrs: map[string]any{"to": string(v)}})
		s.exitLine(fmt.Sprintf("cannot %s: %v", what, err))
		return false
	}
	id := s.sw.choice.Child
	if id == "" || s.o.HasChild == nil {
		return true
	}
	ans := make(chan bool, 1)
	s.goSafe(func() { ans <- s.o.HasChild(v, id) })
	select {
	case ok := <-ans:
		if !ok {
			s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeChildGone, Cause: s.sw.rec, Msg: "the session picked is no longer recorded", Attrs: map[string]any{"to": string(v)}})
			s.exitLine(fmt.Sprintf("the %s session you picked is no longer recorded, so aggy did not switch.", v))
		}
		return ok
	case <-time.After(childCheckWait):
		return true // the launch's plan checks it again
	}
}

// refuseSignalled drops a waiting switch once aggy has got a signal, before
// anything is typed, and says why. The CLI, which survived the signal, stays.
func (s *session) refuseSignalled() bool {
	if !s.signalled {
		return false
	}
	what := "switch to " + string(s.sw.to)
	if s.sw.resume {
		what = "resume " + string(s.sw.to)
	}
	s.log(diag.Rec{Event: diag.EvSwitchCancelled, Cause: s.sw.rec, Attrs: map[string]any{"how": "signalled", "to": string(s.sw.to)}})
	s.setSwitch(pendingSwitch{})
	s.pk.tell(fmt.Sprintf("aggy was sent a signal, so it will not %s.", what), "Open aggy again to switch.")
	s.openPicker()
	return true
}

// signalStops records and says that a signal stopped the waiting switch, and
// returns false, for the caller's result.
func (s *session) signalStops(code int) bool {
	v := s.sw.to
	what := "switch to " + string(v)
	if s.sw.resume {
		what = "resume " + string(v)
	}
	s.log(diag.Rec{Event: diag.EvSwitchCancelled, Cause: s.sw.rec, Exit: diag.Int(code), Attrs: map[string]any{"how": "unclean_exit", "to": string(v), "signalled": true}})
	s.exitLine(fmt.Sprintf("aggy was sent a signal, so aggy did not %s.", what))
	return false
}

// exitLine tells the user, as aggy exits, why a switch did not happen.
func (s *session) exitLine(msg string) {
	s.leaveAlt()
	fmt.Fprintf(s.o.Out, "\r\naggy: %s\r\n", msg)
}

// drainSignals takes the signals that arrived with the CLI's exit: a select
// with both ready picks either, and a signal aggy got is no clean exit
// (T6938). The CLI has gone, so there is nothing to pass them to.
func (s *session) drainSignals(sigs <-chan os.Signal) {
	for {
		select {
		case sig := <-sigs:
			if sig != syscall.SIGWINCH && sig != syscall.SIGTSTP {
				s.signalled = true
			}
		default:
			return
		}
	}
}

// limitNote is a limit's note: pre is the vendor it suggests, "" if none has
// quota.
type limitNote struct {
	h   LimitHit
	pre adapter.Vendor
}

// offerLimitNote shows a limit's note once no turn may be running and the CLI
// is quiet (T6941). Where no turn end is ever counted, it never shows: the CLI
// shows its own limit error, and the list, with the vendor preselected, says it
// (openList). A switch in the way drops it.
func (s *session) offerLimitNote() {
	if s.limitDue == nil || s.open {
		return
	}
	if s.sw.to != "" {
		s.limitDue = nil
		return
	}
	now := time.Now()
	if s.mayBeRunning() || now.Sub(s.lastIn) < switchQuiet || now.Sub(time.Unix(0, s.lastOut.Load())) < switchQuiet {
		return
	}
	n := *s.limitDue
	s.limitDue = nil
	if n.pre == "" {
		s.pk.tell(fmt.Sprintf("%s, and no other vendor aggy offers has quota left.", limitWho(n.h)))
	} else {
		s.pk.tell(fmt.Sprintf("%s. %s has quota left: press %s, then Enter, to switch to it with a brief.", limitWho(n.h), n.pre, keyName(s.pk.key)),
			fmt.Sprintf("aggy cannot tell when %s waits for an approval, so it does not offer the switch itself.", s.o.Vendor))
	}
	s.openPicker()
}

// keysEnd ends the keys of a confirmation's status line.
const keysEnd = "Esc stays."

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
// A crossing of another window that resets sooner is a new one (T6778).
func (s *session) offerWarn() {
	if !s.warnDue {
		return
	}
	if s.mayBeRunning() || s.sw.to != "" || s.open {
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
	warnChecked()
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
	// A crossing of another window that resets sooner is a new one: a
	// stale weekly reading (another rollout's, read before the session's
	// own) must not mute the 5-hour window for days. The warned reset
	// only moves sooner, so two windows never take turns warning. A
	// window of the same length whose reset moved by at most resetSlack
	// is the same window, its resets_at read again (T6811).
	sooner := r.Window != s.warnedWindow && !s.warnedReset.IsZero() && r.Reset.Before(s.warnedReset) &&
		(r.Minutes != s.warnedMinutes || s.warnedReset.Sub(r.Reset) > resetSlack)
	if s.warned && !sooner {
		return
	}
	s.warned, s.warnedWindow, s.warnedMinutes, s.warnedReset = true, r.Window, r.Minutes, r.Reset
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

// warnChecked runs each time offerWarn takes a share reading, so tests
// can wait for that rather than for the read (L60-6).
var warnChecked = func() {}

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
// the reading of the latest ask only. One read runs at a time: an ask
// while one is in flight makes that one stale, and the main loop reads
// again when it lands. A read in flight past shareStale (say, on a hung
// file system) is given up on: the ask starts its own, and the old one's
// answer is dropped when it lands (T6811).
func (s *session) askShare() {
	if s.o.Used == nil || s.o.WarnAt <= 0 {
		return
	}
	s.shareSeq++
	s.share = nil
	if !s.shareBusy || time.Since(s.shareAt) > shareStale {
		s.readShare()
	}
}

// shareStale is how long a share read may run before a new ask gives up
// on it; resetSlack is how far a window's reset may move and still be
// the same window.
var (
	shareStale = 30 * time.Second
	resetSlack = 60 * time.Second
)

// readShare starts the one read of the share for the latest ask.
func (s *session) readShare() {
	s.shareBusy, s.shareRead, s.shareAt = true, s.shareSeq, time.Now()
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
	s.warned, s.warnDue, s.warnedWindow, s.warnedMinutes, s.warnedReset = true, false, "", 0, time.Time{}
	if s.open && s.pk.warning {
		s.closePicker()
	}
	pre := s.withQuota()
	s.pk.preselect = pre
	if s.sw.resume && !s.sw.typed && pre != "" {
		// A resume for changed tools gives way to the limit's handoff (M1).
		if s.open && s.pk.confirm != "" {
			s.closePicker()
		}
		s.log(diag.Rec{Event: diag.EvSwitchCancelled, Cause: s.lastLimit, Attrs: map[string]any{"how": "limit", "to": string(s.sw.to)}})
		s.setSwitch(pendingSwitch{})
		s.toolSeen = nil // a later notice offers it again
	}
	if s.open || s.sw.to != "" {
		return // the picker is up, or a switch already waits
	}
	switch {
	case pre == "":
		s.log(diag.Rec{Event: diag.EvLimitNotOffered, Code: diag.CodeNoQuota, Cause: s.lastLimit})
		s.limitDue = &limitNote{h: h}
	case !holdSignal(v):
		s.log(diag.Rec{Event: diag.EvLimitNotOffered, Code: diag.CodeLimitNoHold, Cause: s.lastLimit, Attrs: map[string]any{"to": string(pre)}})
		s.limitDue = &limitNote{h: h, pre: pre}
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
	if len(c.Tools) > 0 {
		out = append(out, "tools "+strings.Join(c.Tools, ","))
	}
	return out
}

// childCheckWait bounds how long a confirmed switch waits for HasChild
// before the switch is offered again.
var childCheckWait = 5 * time.Second

// childCheck is one answer of Options.HasChild.
type childCheck struct {
	seq int
	ok  bool
}

// confirmSwitch types the exit command for the confirmed switch, once
// HasChild, read off the main loop, says the session picked is still
// recorded (T6770): the picker's list may be stale, and launching a
// session that is gone would fail after the CLI on screen has exited.
func (s *session) confirmSwitch() {
	if s.refuseSignalled() {
		return
	}
	id, v := s.sw.choice.Child, s.sw.to
	if id == "" || s.o.HasChild == nil {
		s.typeExit()
		return
	}
	s.childSeq++
	seq := s.childSeq
	s.sw.checkSeq, s.sw.checkAt = seq, time.Now()
	s.goSafe(func() {
		ok := s.o.HasChild(v, id)
		select {
		case s.childChecks <- childCheck{seq: seq, ok: ok}:
		case <-s.quit:
		}
	})
}

// childChecked takes HasChild's answer for the confirmed switch. An
// answer for another switch or ask is dropped; so is one that lands with
// the picker open or a turn started since, and the switch is offered
// again once it may go on.
func (s *session) childChecked(r childCheck) {
	if r.seq != s.sw.checkSeq || s.sw.typed {
		return
	}
	s.sw.checkSeq, s.sw.checkAt = 0, time.Time{}
	switch {
	case s.open || !s.atPrompt:
		return
	case r.ok:
		s.typeExit()
		return
	}
	v := s.sw.to
	s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeChildGone, Cause: s.sw.rec, Msg: "the session picked is no longer recorded", Attrs: map[string]any{"to": string(v)}})
	s.setSwitch(pendingSwitch{})
	s.pk.tell(fmt.Sprintf("The %s session you picked is no longer recorded, so aggy did not switch.", v),
		fmt.Sprintf("Press %s to pick again.", keyName(s.pk.key)))
	s.openPicker()
}

// typeExit types the on-screen CLI's exit command, through the input
// queue, after anything the user typed before it.
//
// It sends no Esc first to clear a menu: what Esc does at each CLI's
// prompt and approval menus is not in docs/verification.md (S-3).
//
// TODO(T6917): Codex can open a rate-limit popup at turn completion whose
// first item is "Switch to <model>" (unverified live), and turnRan does not
// guard that: the Enter after the exit command could land on it.
func (s *session) typeExit() {
	if s.refuseSignalled() {
		return
	}
	switch {
	case !s.turnRan:
		s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeNoTurnYet, Cause: s.sw.rec, Msg: "the switch key was pressed, but no turn has ended since the CLI launched"})
		return
	case s.doubt() && !s.sw.doubtOK:
		s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodePromptDoubt, Cause: s.sw.rec, Msg: "the switch key was pressed, but the prompt has changed since the person confirmed", Attrs: map[string]any{"to": string(s.sw.to)}})
		return
	case s.knownText():
		s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeUnsentText, Cause: s.sw.rec, Msg: "the switch key was pressed, but text the user typed is not sent"})
		return
	case s.held, s.askOpen():
		s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeSwitchHeld, Cause: s.sw.rec, Msg: "the switch key was pressed, but the CLI waits for the user"})
		return
	}
	s.typeExitCommand()
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
	s.pk.waiting, s.pk.checking, s.pk.resume = s.sw.to, false, s.sw.resume
	s.pk.noTurn, s.pk.on = !s.turnRan, s.o.Vendor
	s.pk.noHooks = s.o.HooksMissing && !s.hookSeen.Load()
	s.pk.moveRefused = s.moveRefused
	// A pick would wait: section 5's rule is to say so here, in the list the
	// person opened, and never in a notice that could hold a turn's output.
	s.pk.mayRun = s.turnRan && !s.atPrompt && !s.held
	s.pk.doubtSaid = s.pk.mayRun && s.doubt()
	s.pk.stalePick = s.sw.toldNoTurn && !s.sw.typed && time.Since(s.sw.toldAt) > noTurnPickFresh
	s.pk.limitLine = ""
	if s.limitDue != nil {
		// The person came to the list first: it says what the note would.
		s.pk.limitLine = limitWho(s.limitDue.h) + "."
		s.limitDue = nil
	}
	// After a limit, Enter takes the suggested vendor, with a brief. A switch
	// that waits shows its own options, so picking it again keeps them; they are
	// that vendor's, so a pick of another drops them (picker.choose).
	s.pk.opts.Brief = s.pk.preselect != ""
	if s.sw.to != "" && !s.sw.resume {
		s.pk.seed = Choice{Brief: s.sw.choice.Brief && !s.pk.opts.Brief, New: s.sw.choice.New, Model: s.sw.choice.Model}
		s.pk.opts.Brief = s.pk.opts.Brief || s.sw.choice.Brief
		s.pk.opts.New, s.pk.opts.Model = s.sw.choice.New, s.sw.choice.Model
	}
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
	if s.open {
		// outMu is held while the picker is up: never lock it twice.
		s.closePicker()
	}
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
	s.noTurnOpen = false
	if s.statusRows > 0 {
		s.pk.eraseStatus(s.o.Out, s.statusRows, s.alt.on)
	} else {
		s.pk.erase(s.o.Out, s.pickerOverCLI)
	}
	s.open = false
	// A lone Esc still pending was for this prompt: it must not close
	// the next one (a limit's note that replaced a warning).
	s.escBy = nil
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
	if terminalReport(b) {
		s.toChild.push(b) // no key of the user's: it closes nothing
		return
	}
	if b = s.typedOf(b); len(b) == 0 {
		return // the mouse moved: aggy's prompts take keys only
	}
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
		if s.askOpen() {
			// Only an ask a turn end outlived can be a lost reply. Before one
			// the dialog may well be up, and the exit command would land on it.
			if !s.askStale || !s.askTold {
				s.tellAskWaits(picked)
				return
			}
			s.releaseAsks() // told, and picked again: the user says none is waiting
		}
		if s.held {
			s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeSwitchHeld, Cause: s.sw.rec, Msg: "picked again, but the CLI waits for the user"})
			s.tellHeld(picked)
			return
		}
		// Without a turn end, no pick can say no turn is running: a turn
		// may be, and atPrompt would let a notice hold it.
		s.atPrompt, s.toldDirty = s.atPrompt || s.turnRan, false
		if s.turnRan { // the user says no turn is running
			s.queued, s.queuedWait = 0, false
		}
		c.Tools = s.sw.choice.Tools // the list cannot set them, so an agent's stay
		s.sw.choice, s.sw.asked, s.sw.picked, s.sw.noteShown = c, false, true, false
		s.sw.doubtOK = s.sw.toldDoubt || s.pk.doubtSaid // told, and picked again: the person has looked
		s.sw.rec = s.log(diag.Rec{Event: diag.EvSwitchQueued, Cause: s.sw.rec, Attrs: switchAttrs(c, map[string]any{"origin": "user", "to": string(picked), "again": true})})
		s.markNoTurn()
	case picked != "":
		if err := s.offered(picked); err != nil && !errors.Is(err, ErrChecking) {
			s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeSwitchVendor, Msg: refusedMsg(picked, err), Attrs: map[string]any{"to": string(picked)}})
			s.pk.tell(fmt.Sprintf("Cannot switch to %s: %v", picked, err))
			s.openPicker()
			return
		}
		s.setSwitch(pendingSwitch{to: picked, choice: c, picked: true, toldDoubt: s.pk.doubtSaid})
		s.sw.rec = s.log(diag.Rec{Event: diag.EvSwitchQueued, Attrs: switchAttrs(c, map[string]any{"origin": "user", "to": string(picked)})})
		s.markNoTurn()
		switch {
		case s.askOpen():
			s.tellAskWaits(picked) // its own words, and the way past a stale ask
		case s.held && s.turnRan:
			// Without a turn end the list has already said what to do.
			s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeSwitchHeld, Cause: s.sw.rec, Msg: "picked, but the CLI waits for the user"})
			s.tellHeld(picked)
		}
	default: // Esc on the list cancels a waiting switch
		s.toolsWaitTurn = s.toolsWaitTurn || s.sw.resume
		s.setSwitch(pendingSwitch{})
	}
}

// tellHeld says why a switch to v waits for a hold, and the way out that costs
// no turn: an Enter on the empty prompt clears the hold, and a pick then says no
// turn is running. That Enter would answer an open prompt, so it is only for
// when none is on screen. Each line fits 80 columns.
func (s *session) tellHeld(v adapter.Vendor) {
	s.pk.tell(fmt.Sprintf("%s said it is waiting for you (a permission prompt or another notice),", s.o.Vendor),
		fmt.Sprintf("so the switch to %s waits. If it asks you something, answer it:", v),
		"aggy goes on at that turn's end.",
		"If nothing is waiting on screen, Esc here, press Enter on the empty prompt,",
		fmt.Sprintf("and pick %s again. That spends no turn; sending a message would.", v))
	s.openPicker()
}

// doubtLines are the note when aggy cannot tell if the prompt is empty. Each fits
// 80 columns with the longest what, a switch's.
func doubtLines(what string, on, to adapter.Vendor) []string {
	return []string{fmt.Sprintf("aggy is ready to %s, but cannot tell if %s's prompt is empty:", what, on),
		"a command, a popup, or Esc may have left text there that aggy cannot see.",
		fmt.Sprintf("Look at it. If nothing there matters, pick %s again, and aggy asks first.", to)}
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
		s.confirmSwitch()
	case s.pk.isKey(b) && s.pk.resume:
		// A tool change's note on a vendor with no hold signal: the user
		// queues the resume, still confirmed at the turn end (T6729). A
		// key meant for the CLI as the note appears does not count.
		if time.Since(s.openedAt) < confirmGuard {
			return
		}
		s.closePicker()
		if s.sw.to == "" { // an agent's ask may have come in meanwhile
			s.queueResume(s.toolsNote, true)
		}
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

// exitNote says what a late exit does, and how to cancel it.
func exitNote(sw pendingSwitch) string {
	if sw.resume {
		return fmt.Sprintf("If it exits, aggy resumes %s on the same session. To cancel, press the switch key, then Esc in the list.", sw.to)
	}
	return fmt.Sprintf("If it exits, aggy switches to %s. To cancel, press the switch key, then Esc in the list.", sw.to)
}

// promptEsc is a lone Esc at a prompt aggy opened, once nothing followed
// it within escTimeout.
func (s *session) promptEsc() {
	if !s.open || (s.pk.confirm == "" && !s.pk.note) {
		return
	}
	confirming, toolsNote := s.pk.confirm != "", s.pk.note && s.pk.resume
	s.closePicker()
	if toolsNote {
		s.toolsWaitTurn = true // a tools note turned down, as an offer (L2)
	}
	if confirming {
		s.log(diag.Rec{Event: diag.EvSwitchCancelled, Cause: s.sw.rec, Attrs: map[string]any{"how": "esc_confirm", "to": string(s.sw.to)}})
		s.toolsWaitTurn = s.toolsWaitTurn || s.sw.resume
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

// ctrlU clears the line in a readline-style composer; ctrlR starts a
// history search (unverified: the readline key, assumed in each CLI).
const (
	ctrlU = 0x15
	ctrlR = 0x12
)
