package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
)

// sessionIndex keeps the index's record of the on-screen CLI's session
// id, from its hooks and, for Codex, from its sessions folder.
//
// TODO(T6661): record the parent and child rows; until then the id goes
// to the events log under the parent id.
type sessionIndex struct {
	parentID string
	vendor   adapter.Vendor
	// record writes one event; tests replace it.
	record func(store.Event) error

	mu    sync.Mutex
	id    string
	known chan struct{} // closed once an id is known
}

func newSessionIndex(parentID string, v adapter.Vendor) *sessionIndex {
	return &sessionIndex{parentID: parentID, vendor: v, known: make(chan struct{}), record: appendEvent}
}

// appendEvent opens the index, appends e, and closes it.
func appendEvent(e store.Event) error {
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	return s.AppendEvent(ctx, e)
}

// hook is supervisor.Options.OnHook. A SessionStart sets the id (a
// /clear or resume starts another session); any other event with an id
// sets it only while none is known (Codex's notify carries thread-id,
// and needs no hook approval). Events of another vendor are ignored.
func (x *sessionIndex) hook(r supervisor.Request) {
	if r.Vendor != string(x.vendor) {
		return
	}
	ev := hook.Parse(x.vendor, r.Payload)
	if adapter.ValidSessionID(x.vendor, ev.SessionID) != nil {
		return
	}
	// TODO(T6616): whether Codex's SessionStart payload carries the
	// thread id is source-verified only; the sessions folder watch
	// (watchCodex) is the path that does not depend on it.
	x.set(ev.SessionID, ev.Source, "hook", r.Event == "SessionStart")
}

// set records id unless it is the one already known, or, unless
// replace, another id is known.
func (x *sessionIndex) set(id, source, via string, replace bool) {
	x.mu.Lock()
	if id == x.id || (x.id != "" && !replace) {
		x.mu.Unlock()
		return
	}
	if x.id == "" {
		close(x.known)
	}
	x.id = id
	x.mu.Unlock()
	d := map[string]any{"vendor": string(x.vendor), "session": id, "via": via}
	if source != "" {
		d["source"] = source
	}
	// A failed write loses one index line; it must not disturb the CLI.
	_ = x.record(store.Event{At: time.Now(), ParentID: x.parentID, Type: store.EventSession, Detail: d})
}

// codexSessionsDir is the Codex home's sessions folder.
func codexSessionsDir() (string, error) {
	ch, err := codexHome(os.Getenv("HOME"))
	if err != nil {
		return "", err
	}
	return filepath.Join(ch, "sessions"), nil
}

// startIndex returns the OnHook for a session of v under parentID and,
// for Codex, starts the sessions folder watch; stop ends it once the CLI
// has exited.
func startIndex(parentID string, v adapter.Vendor) (onHook func(supervisor.Request), stop func()) {
	x := newSessionIndex(parentID, v)
	done := make(chan struct{})
	if v == adapter.Codex {
		if dir, err := codexSessionsDir(); err == nil {
			start := time.Now()
			before := codexRollouts(dir, start, start)
			go x.watchCodex(dir, start, before, done)
		}
	}
	return x.hook, func() { close(done) }
}

// codexPoll is how often watchCodex looks at the sessions folder.
var codexPoll = 500 * time.Millisecond

// watchCodex finds the thread id of a Codex session started at start
// from its rollout file, which Codex creates lazily, at the first
// persist: it polls the date folders for a rollout name that was not
// there before, until an id is known (from a hook too), stop closes, or
// two new rollouts appear at once (another Codex started alongside, so
// which is ours cannot be told: nothing is recorded). It reads folder
// listings only, never a rollout's content.
//
// before is codexRollouts at start, taken before the CLI was started.
func (x *sessionIndex) watchCodex(dir string, start time.Time, before map[string]bool, stop <-chan struct{}) {
	t := time.NewTicker(codexPoll)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-x.known:
			return
		case now := <-t.C:
			var fresh []string
			for id := range codexRollouts(dir, start, now) {
				if !before[id] {
					fresh = append(fresh, id)
				}
			}
			switch len(fresh) {
			case 0:
				continue
			case 1:
				x.set(fresh[0], "", "sessions folder", false)
			}
			return
		}
	}
}

// codexRollouts lists the thread ids of the rollouts in the date folders
// a session between start and now could use. Missing folders are empty.
func codexRollouts(dir string, start, now time.Time) map[string]bool {
	ids := map[string]bool{}
	for _, d := range codex.RolloutDirs(dir, start, now) {
		es, err := safefs.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range es {
			if id, ok := codex.RolloutThreadID(e.Name()); ok && e.Type().IsRegular() {
				ids[id] = true
			}
		}
	}
	return ids
}
