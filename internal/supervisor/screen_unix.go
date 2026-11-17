//go:build unix

package supervisor

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/screen"
)

// The words a switch's offered and confirmed records carry as attrs.evidence
// (T7016): what said the CLI was at its prompt when aggy typed its exit
// command. screen: before Claude's first turn end, its screen read as the
// empty prompt (screenOneStep); person: the person's yes on the status line;
// keys: a switch typed in full after a turn end (T6908).
const (
	evidenceScreen = "screen"
	evidencePerson = "person"
	evidenceKeys   = "keys"
)

// screenQueue bounds the output chunks waiting to be fed to the screen.
const screenQueue = 64

// screenFeed keeps a copy of the CLI's screen (internal/screen) off the output
// path: copyOut hands each chunk over without waiting, and one goroutine feeds
// them in order. A chunk that finds the queue full is lost, and the screen is
// Unsure from that point (a gap) until a reset (RIS): the lost bytes may have
// held a mode or hidden the cursor, which no clear restores. Reading it says nothing
// (known false) while chunks wait or a gap is not yet fed.
type screenFeed struct {
	ch chan screenItem
	// pmu orders the producers (copyOut, and resizes from the main loop) and
	// guards gap: a chunk was lost since the last one queued.
	pmu  sync.Mutex
	gap  bool
	sent atomic.Uint64
	done atomic.Uint64
	// mu guards scr.
	mu  sync.Mutex
	scr *screen.Screen
}

// screenItem is a chunk of output, or a resize when data is nil.
type screenItem struct {
	data       []byte
	rows, cols int
	gap        bool
}

func newScreenFeed(rows, cols int) *screenFeed {
	return &screenFeed{ch: make(chan screenItem, screenQueue), scr: screen.New(rows, cols)}
}

// put queues an item without waiting; a full queue loses it (gap).
func (f *screenFeed) put(it screenItem) {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	it.gap = f.gap
	select {
	case f.ch <- it:
		f.gap = false
		f.sent.Add(1)
	default:
		f.gap = true
	}
}

// feed queues a copy of b, output the CLI wrote.
func (f *screenFeed) feed(b []byte) { f.put(screenItem{data: append([]byte(nil), b...)}) }

// resize queues a new size.
func (f *screenFeed) resize(rows, cols int) { f.put(screenItem{rows: rows, cols: cols}) }

// run feeds the queue to the screen until quit closes.
func (f *screenFeed) run(quit <-chan struct{}) {
	for {
		select {
		case it := <-f.ch:
			f.apply(it)
		case <-quit:
			return
		}
	}
}

func (f *screenFeed) apply(it screenItem) {
	f.mu.Lock()
	if it.gap {
		f.scr.SetUnsure()
	}
	if it.data != nil {
		f.scr.Feed(it.data)
	} else {
		f.scr.Resize(it.rows, it.cols)
	}
	f.mu.Unlock()
	f.done.Add(1)
}

// snapshot is a copy of the screen, or nil when what is fed lags behind
// what the CLI wrote.
func (f *screenFeed) snapshot() *screen.Snapshot {
	f.pmu.Lock()
	gap, sent := f.gap, f.sent.Load()
	f.pmu.Unlock()
	if gap || f.done.Load() != sent {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scr.Snapshot()
}

// screenSays is what the adapter's classifier (Options.ReadScreen) reads off
// the CLI's screen now; adapter.ScreenOther when there is none to read, or
// the copy lags behind.
func (s *session) screenSays() adapter.ScreenState {
	if s.scr == nil || s.o.ReadScreen == nil {
		return adapter.ScreenOther
	}
	p := s.scr.snapshot()
	if p == nil {
		return adapter.ScreenOther
	}
	return s.o.ReadScreen(p)
}

// screenOneStep reports whether a switch before Claude's first turn end may
// go in one step, with no status-line question (T7016): every check the hooks
// and the keys give holds, the CLI's output is quiet, and its screen reads as
// the empty prompt. The hooks stay the cross-check: a hold or a turn that may
// run, whatever the screen says, asks. Only a switch the person asked for,
// typed in full and caught from the keys or picked in aggy's list: never one
// a hook reported (hooked), nor its repeat (hookAgain).
//
// It returns the CLI's last output time it read the screen against (out):
// taken before the read and checked again after it, so output that lands
// while the screen is read (copyOut stamps lastOut before it queues the
// bytes) is never taken as read (#149 review, L1).
func (s *session) screenOneStep() (ok bool, out int64) {
	out = s.lastOut.Load()
	if s.turnRan || !s.firstAsk() || s.sw.noScreen || s.sw.hooked || s.hookAgain || s.sw.resume || s.sw.asked || s.sw.limit || !s.sw.picked {
		return false, out
	}
	if s.enterBeforeTurn || s.held || s.askOpen() || s.hookAsk != nil || s.open || s.doubt() || s.knownText() || s.turnMayRun() {
		return false, out
	}
	if time.Since(time.Unix(0, out)) < switchQuiet || s.altOn() {
		return false, out
	}
	return s.screenSays() == adapter.ScreenPromptEmpty && s.lastOut.Load() == out, out
}

// screenStill reports whether what let a switch go in one step still holds
// as aggy is about to type the exit command: no key and no output since the
// offer, and the same checks, the screen's among them. The output time is
// checked again after the screen is read, so output that lands meanwhile
// counts.
func (s *session) screenStill() bool {
	if !s.lastIn.Equal(s.sw.offerIn) || s.lastOut.Load() != s.sw.offerOut {
		return false
	}
	if s.enterBeforeTurn || s.held || s.askOpen() || s.hookAsk != nil || s.open || s.doubt() || s.knownText() || s.turnMayRun() || s.altOn() {
		return false
	}
	return s.screenSays() == adapter.ScreenPromptEmpty && s.lastOut.Load() == s.sw.offerOut
}

// altOn reports whether the CLI is on its alternate screen, as the output's
// tracker sees it.
func (s *session) altOn() bool {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	return s.alt.on
}

// goByScreen sends the waiting switch on in one step, on the screen's word
// (screenOneStep): the launch check, then the exit command, where typeExit
// checks it all again (screenStill).
func (s *session) goByScreen(out int64) {
	v := s.sw.to
	s.sw.byScreen, s.sw.evidence = true, evidenceScreen
	s.sw.offerIn, s.sw.offerOut = s.lastIn, out
	s.sw.rec = s.log(diag.Rec{Event: diag.EvSwitchOffered, Cause: s.turnCause(), Attrs: switchAttrs(s.sw.choice, map[string]any{"to": string(v), "first_turn": true, "evidence": evidenceScreen})})
	s.pk.preselect = ""
	s.confirmSwitch()
}

// screenChanged takes back a one-step switch whose screen or state changed
// before the exit command went: the status line asks instead, at the next
// quiet moment, as it would have without the screen.
func (s *session) screenChanged() {
	s.sw.byScreen, s.sw.noScreen, s.sw.evidence = false, true, ""
	s.log(diag.Rec{Event: diag.EvSwitchRefused, Code: diag.CodeScreenChanged, Cause: s.sw.rec,
		Msg: "the screen or the session changed before aggy typed the exit command, so the status line asks", Attrs: map[string]any{"to": string(s.sw.to)}})
}
