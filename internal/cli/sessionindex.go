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
// id. Any process the CLI starts inherits AGGY_SOCKET and AGGY_VENDOR (a
// nested claude -p or codex exec run by the agent's shell tool, or the
// agent piping JSON into aggy hook), and aggy cannot keep them from its
// tool subprocesses, so a hook event alone never sets or replaces the
// id:
//
//   - Claude and Grok get a pre-assigned --session-id (adapters.md), and
//     that is the only id recorded. A later /clear starts another
//     session the index does not follow.
//   - Codex cannot be given an id. The first thread id a Codex hook
//     reports is the claim (the on-screen CLI's SessionStart comes before
//     its agent can run anything), and it is recorded only once the
//     sessions folder shows a rollout of that id that was not there at
//     launch. A new rollout alone (another Codex in another terminal, or
//     a borrow sharing CODEX_HOME) is never recorded, and neither is a
//     later claim. Once recorded, the id is not replaced.
//
// TODO(T6661): record the parent and child rows; until then the id goes
// to the events log under the parent id.
type sessionIndex struct {
	parentID string
	vendor   adapter.Vendor
	// record writes one event; tests replace it.
	record func(store.Event) error

	mu    sync.Mutex
	id    string          // recorded
	claim string          // Codex: the first id a hook reported
	seen  map[string]bool // Codex: new rollouts the watch has seen
	known chan struct{}   // closed once an id is recorded
}

func newSessionIndex(parentID string, v adapter.Vendor) *sessionIndex {
	return &sessionIndex{parentID: parentID, vendor: v, known: make(chan struct{}),
		seen: map[string]bool{}, record: appendEvent}
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

// hook is supervisor.Options.OnHook. Only Codex ids come from hooks, as
// a claim (see sessionIndex).
func (x *sessionIndex) hook(r supervisor.Request) {
	if x.vendor != adapter.Codex || r.Vendor != string(x.vendor) {
		return
	}
	// TODO(T6616): that Codex's SessionStart and Stop payloads carry the
	// thread id as session_id is source-verified, not yet seen live.
	ev := hook.Parse(x.vendor, r.Payload)
	if adapter.ValidSessionID(x.vendor, ev.SessionID) != nil {
		return
	}
	x.mu.Lock()
	if x.claim == "" {
		x.claim = ev.SessionID
	}
	confirmed := x.seen[x.claim]
	claim := x.claim
	x.mu.Unlock()
	if confirmed {
		x.set(claim, ev.Source, "hook and sessions folder")
	}
}

// saw records rollouts the watch found new, and the claim once it is
// among them.
func (x *sessionIndex) saw(ids []string) {
	x.mu.Lock()
	for _, id := range ids {
		x.seen[id] = true
	}
	confirmed := x.claim != "" && x.seen[x.claim]
	claim := x.claim
	x.mu.Unlock()
	if confirmed {
		x.set(claim, "", "hook and sessions folder")
	}
}

// set records id, once.
func (x *sessionIndex) set(id, source, via string) {
	x.mu.Lock()
	if x.id != "" {
		x.mu.Unlock()
		return
	}
	x.id = id
	close(x.known)
	x.mu.Unlock()
	d := map[string]any{"vendor": string(x.vendor), "session": id, "via": via}
	if source != "" {
		d["source"] = source
	}
	// A failed write loses one index line; it must not disturb the CLI.
	_ = x.record(store.Event{At: time.Now(), ParentID: x.parentID, Type: store.EventSession, Detail: d})
}

// startIndex returns the OnHook for a session of v under parentID. A
// pre-assigned id (Claude, Grok) is recorded at once; for Codex it
// starts the sessions folder watch. stop ends the watch once the CLI has
// exited.
func startIndex(parentID string, v adapter.Vendor, assigned string) (onHook func(supervisor.Request), stop func()) {
	x := newSessionIndex(parentID, v)
	done := make(chan struct{})
	if assigned != "" {
		x.set(assigned, "", "assigned")
	}
	if v == adapter.Codex {
		if dir, err := codexSessionsDir(); err == nil {
			start := time.Now()
			before := codexRollouts(dir, start, start)
			go x.watchCodex(dir, start, before, done)
		}
	}
	return x.hook, func() { close(done) }
}

// codexSessionsDir is the Codex home's sessions folder.
func codexSessionsDir() (string, error) {
	ch, err := codexHome(os.Getenv("HOME"))
	if err != nil {
		return "", err
	}
	return filepath.Join(ch, "sessions"), nil
}

// codexPoll is how often watchCodex looks at the sessions folder.
var codexPoll = 500 * time.Millisecond

// watchCodex polls the date folders for Codex rollouts that were not
// there at launch (before, codexRollouts at start, taken before the CLI
// started) and hands them to saw, until an id is recorded or stop
// closes. Codex creates a rollout lazily, at the first persist, so it
// may appear only after the first turn. It reads folder listings only,
// never a rollout's content.
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
			x.saw(fresh)
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
