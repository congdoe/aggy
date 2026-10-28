package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
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
//   - Codex cannot be given an id. A thread id a Codex hook reports is a
//     claim, kept only when the socket found the hook's process in the
//     on-screen CLI's tree (supervisor.Request.FromSession, peer-pid
//     lineage). That tree still holds the agent's own tool subprocesses,
//     so a nested codex exec's hooks pass it too (D20-7, D23-8). A claim
//     is recorded only once the sessions folder shows a rollout of that
//     id that was not there at launch, and that rollout's first line, its
//     session_meta, has source "cli": the interactive TUI's, where codex
//     exec writes "exec" (verifyRollout). A new rollout alone (another
//     Codex in another terminal, or a borrow sharing CODEX_HOME) is never
//     recorded. Once recorded, the id is not replaced.
//
// The id goes to the events log under the parent id, and to onID, which
// records it on the child row (childIDs.set): first wins there too, so
// after a /clear the child still names the conversation it started with.
type sessionIndex struct {
	parentID string
	vendor   adapter.Vendor
	// record writes one event; tests replace it.
	record func(store.Event) error
	// onID gets the recorded id, once; nil means nothing more to do.
	onID func(id string)

	// verify checks a claimed rollout; tests replace it.
	verify func(path, id string) rolloutVerdict

	mu       sync.Mutex
	id       string            // recorded
	claims   []string          // Codex: ids hooks in the CLI's tree reported, in order
	rejected map[string]bool   // Codex: claims whose rollout is not the TUI's
	seen     map[string]string // Codex: new rollouts the watch has seen, id to path
	known    chan struct{}     // closed once an id is recorded
}

// maxClaims bounds the claims kept; a session sees one, plus one per
// nested codex exec that ran before the first rollout showed.
const maxClaims = 16

func newSessionIndex(parentID string, v adapter.Vendor) *sessionIndex {
	return &sessionIndex{parentID: parentID, vendor: v, known: make(chan struct{}),
		seen: map[string]string{}, rejected: map[string]bool{}, record: appendEvent, verify: verifyRollout}
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
// a claim, and only from the CLI's process tree (see sessionIndex).
func (x *sessionIndex) hook(r supervisor.Request) {
	if x.vendor != adapter.Codex || r.Vendor != string(x.vendor) || !r.FromSession {
		return
	}
	// TODO(T6616): that Codex's SessionStart and Stop payloads carry the
	// thread id as session_id is source-verified, not yet seen live.
	ev := hook.Parse(x.vendor, r.Payload)
	if adapter.ValidSessionID(x.vendor, ev.SessionID) != nil {
		return
	}
	x.mu.Lock()
	if !slices.Contains(x.claims, ev.SessionID) && len(x.claims) < maxClaims {
		x.claims = append(x.claims, ev.SessionID)
	}
	x.mu.Unlock()
	x.check()
}

// saw records rollouts the watch found new (id to path), and checks the
// claims against them.
func (x *sessionIndex) saw(rollouts map[string]string) {
	x.mu.Lock()
	for id, p := range rollouts {
		x.seen[id] = p
	}
	x.mu.Unlock()
	x.check()
}

// check records the first claim, in claim order, whose new rollout is
// the interactive TUI's. A rollout not written yet is tried again on the
// next poll; one that is another kind of session rules its claim out.
func (x *sessionIndex) check() {
	type cand struct{ id, path string }
	var cs []cand
	x.mu.Lock()
	if x.id != "" {
		x.mu.Unlock()
		return
	}
	for _, id := range x.claims {
		if p, ok := x.seen[id]; ok && !x.rejected[id] {
			cs = append(cs, cand{id, p})
		}
	}
	x.mu.Unlock()
	for _, c := range cs {
		switch x.verify(c.path, c.id) {
		case rolloutTUI:
			x.set(c.id, "", "hook and sessions folder")
			return
		case rolloutOther:
			x.mu.Lock()
			x.rejected[c.id] = true
			x.mu.Unlock()
		}
	}
}

// rolloutVerdict is what verifyRollout found.
type rolloutVerdict int

const (
	rolloutPending rolloutVerdict = iota // not readable or not written yet
	rolloutTUI                           // the interactive TUI's own session
	rolloutOther                         // another session: codex exec, a sub-agent
)

// maxMetaLine bounds the session_meta line read; it carries the base
// instructions, tens of KiB.
const maxMetaLine = 1 << 20

// verifyRollout reads the rollout's first line, which Codex writes as its
// session_meta before anything else (rollout/src/recorder.rs#L1936 at
// rust-v0.164.0), and checks its payload: id the claimed thread, and
// source "cli", which the interactive TUI sets (tui/src/lib.rs#L753)
// and codex exec does not ("exec", exec/src/lib.rs#L744; the field is
// SessionSource, lowercase, protocol/src/protocol.rs#L2959). Source
// only: the TUI's session_source reaching the recorder's source
// (recorder.rs#L968) is traced through the client, not seen live.
// TODO(T6616): confirm a live TUI rollout's source is "cli".
func verifyRollout(path, id string) rolloutVerdict {
	f, err := safefs.OpenRegular(path)
	if err != nil {
		return rolloutPending
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReaderSize(io.LimitReader(f, maxMetaLine), 64<<10).ReadBytes('\n')
	if err != nil {
		if len(line) >= maxMetaLine {
			return rolloutOther
		}
		return rolloutPending // no whole line yet
	}
	var m struct {
		Type    string `json:"type"`
		Payload struct {
			ID     string          `json:"id"`
			Source json.RawMessage `json:"source"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &m) != nil || m.Type != "session_meta" || m.Payload.ID != id ||
		string(m.Payload.Source) != `"cli"` {
		return rolloutOther
	}
	return rolloutTUI
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
	if x.onID != nil {
		x.onID(id)
	}
}

// startIndex returns the OnHook for a session of v under parentID. An id
// known at launch (Claude's and Grok's pre-assigned or resumed one, a
// resumed Codex thread) is recorded at once; otherwise, for Codex, it
// starts the sessions folder watch. onID (may be nil) gets the recorded
// id. stop ends the watch once the CLI has exited.
func startIndex(parentID string, v adapter.Vendor, assigned string, onID func(string)) (onHook func(supervisor.Request), stop func()) {
	x := newSessionIndex(parentID, v)
	x.onID = onID
	done := make(chan struct{})
	if assigned != "" {
		x.set(assigned, "", "assigned")
	}
	if v == adapter.Codex && assigned == "" {
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
// may appear only after the first turn. It reads folder listings; only a
// claimed rollout's first line (session_meta) is read, by check.
func (x *sessionIndex) watchCodex(dir string, start time.Time, before map[string]string, stop <-chan struct{}) {
	t := time.NewTicker(codexPoll)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-x.known:
			return
		case now := <-t.C:
			fresh := map[string]string{}
			for id, p := range codexRollouts(dir, start, now) {
				if _, old := before[id]; !old {
					fresh[id] = p
				}
			}
			x.saw(fresh)
		}
	}
}

// codexRollouts maps the thread ids of the rollouts in the date folders a
// session between start and now could use to their paths. Missing
// folders are empty.
func codexRollouts(dir string, start, now time.Time) map[string]string {
	ids := map[string]string{}
	for _, d := range codex.RolloutDirs(dir, start, now) {
		es, err := safefs.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range es {
			if id, ok := codex.RolloutThreadID(e.Name()); ok && e.Type().IsRegular() {
				ids[id] = filepath.Join(d, e.Name())
			}
		}
	}
	return ids
}
