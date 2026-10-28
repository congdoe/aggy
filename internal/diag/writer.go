// Package diag is aggy's diagnostic log: the record writer, the event and
// reason-code registry, and the correlation ids (docs/logging.md). Records
// go to ~/.aggy/diag.db through internal/store. A failed write never fails
// the command: the writer warns once on stderr and goes on.
//
// Records hold ids, codes, aggy-state paths, exit codes, and durations.
// Every free-text value (msg and string attrs) passes through
// internal/redact and internal/termsafe, and text the caller marks with
// Forbid (a borrow's task and constraints) is replaced wherever it shows.
package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/redact"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

// Schema is the record schema version written to each record.
const Schema = 1

// TraceEnv carries the trace id to the children aggy starts, so their
// records join the flow that started them.
const TraceEnv = "AGGY_TRACE"

// DebugEnv, set to 1, stores debug records.
const DebugEnv = "AGGY_DEBUG"

// Caps on free text, in runes.
const (
	maxMsg  = 300
	maxAttr = 200
)

// Rec is one record to write. The level and component come from the
// event's registry entry.
type Rec struct {
	Event Event
	Code  Code
	// Msg is a short message: aggy's own text, or an error aggy printed.
	// It is scrubbed (see the package doc).
	Msg string
	// Attrs are small typed values: strings (scrubbed), bools, and
	// numbers. Never prompt or task text, file contents, diffs, model
	// output, or environment values.
	Attrs map[string]any
	// Cause is the id of the record that led to this one.
	Cause string
	// Entity ids.
	Parent, Child, Borrow, Worktree, Vendor, VendorSession string
	Duration                                               time.Duration
	Exit                                                   *int
	Signal                                                 string
	// Trace overrides the trace id. Without it a record about a borrow
	// takes the trace of that borrow's first record, and any other the
	// writer's own.
	Trace string
	// id and at are set by Queue.Log: the record's id, and when it was
	// queued.
	id string
	at time.Time
}

// Writer writes records for one aggy process. Its zero value is not
// usable; a nil *Writer writes nothing.
type Writer struct {
	version string
	// Paths and settings, replaceable by tests.
	diagPath  func() (string, error)
	indexPath func() (string, error)
	logConfig func() (config.Log, error)
	getenv    func(string) string
	now       func() time.Time
	warn      io.Writer

	run, trace string
	ids        ulids

	mu     sync.Mutex
	warned bool
	forbid []string

	// The open log and its path, under dbMu.
	dbMu   sync.Mutex
	db     *store.DiagDB
	dbPath string
}

// New returns a writer for this process: a new run id, and the trace from
// AGGY_TRACE when it holds a valid id, else the run id (a new flow).
// Warnings go to warn.
func New(version string, warn io.Writer) *Writer {
	w := &Writer{
		version: version, diagPath: store.DiagPath, indexPath: store.DefaultPath,
		logConfig: func() (config.Log, error) {
			c, err := config.LoadDefault()
			return c.Log, err
		},
		getenv: os.Getenv, now: time.Now, warn: warn,
	}
	w.run = w.ids.next(w.now())
	w.trace = w.run
	if t := w.getenv(TraceEnv); ValidID(t) {
		w.trace = t
	}
	return w
}

// RunID is this process's run id; "" for a nil writer.
func (w *Writer) RunID() string {
	if w == nil {
		return ""
	}
	return w.run
}

// Trace is the writer's trace id; "" for a nil writer.
func (w *Writer) Trace() string {
	if w == nil {
		return ""
	}
	return w.trace
}

// Tag is the suffix of a printed refusal or error: "[code · run id]".
// It is "" for a nil writer.
func (w *Writer) Tag(c Code) string {
	if w == nil {
		return ""
	}
	return fmt.Sprintf("[%s · run %s]", c, w.run)
}

// Forbid marks text that must never be stored, such as a borrow's task:
// each text whole, each of its lines of 8 or more bytes, and each of its
// words of 12 or more (long enough not to be aggy's own vocabulary) is
// replaced in every message and attr written after.
func (w *Writer) Forbid(texts ...string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, t := range texts {
		for _, s := range append([]string{t}, strings.Split(t, "\n")...) {
			if s = strings.TrimSpace(s); len(s) >= 8 {
				w.forbid = append(w.forbid, s)
			}
		}
		for _, s := range strings.Fields(t) {
			if len(s) >= 12 {
				w.forbid = append(w.forbid, s)
			}
		}
	}
	// Longest first, so a line goes whole before a word inside it.
	sort.Slice(w.forbid, func(i, j int) bool { return len(w.forbid[i]) > len(w.forbid[j]) })
}

// scrub makes free text safe to store: forbidden text replaced, secret
// lines removed (redact.Lines), escaped (termsafe), and capped at max
// runes.
func (w *Writer) scrub(s string, max int) string {
	w.mu.Lock()
	for _, f := range w.forbid {
		s = strings.ReplaceAll(s, f, "[task text]")
	}
	w.mu.Unlock()
	s, n := redact.Lines(s)
	if n > 0 {
		s = strings.TrimSpace(s + " [redacted]")
	}
	s = termsafe.Field(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

func (w *Writer) attrs(m map[string]any) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case string:
			out[k] = w.scrub(x, maxAttr)
		case bool, int, int64, float64:
			out[k] = x
		case []string:
			ss := make([]string, len(x))
			for i, s := range x {
				ss[i] = w.scrub(s, maxAttr)
			}
			out[k] = ss
		default:
			out[k] = w.scrub(fmt.Sprint(x), maxAttr)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}

// stores reports whether records at level l are kept: at or above
// [log] level (info by default), and debug with AGGY_DEBUG=1 too.
func (w *Writer) stores(l Level) bool {
	if l == LevelDebug && w.getenv(DebugEnv) == "1" {
		return true
	}
	min := LevelInfo
	if c, err := w.logConfig(); err == nil && c.Level != "" {
		min = Level(c.Level)
	}
	for _, x := range AtLeast(min) {
		if x == l {
			return true
		}
	}
	return false
}

// Log writes r and returns its record id, or "" when it was not stored
// (below the level, or the write failed; a failure warns once). It never
// fails the command. A record with a security code also goes to the
// index's events table as a refusal (aggy log).
func (w *Writer) Log(ctx context.Context, r Rec) string {
	if w == nil {
		return ""
	}
	ev, ok := LookupEvent(r.Event)
	if !ok {
		w.warnOnce(fmt.Errorf("unregistered event %q", r.Event))
		return ""
	}
	code, ok := LookupCode(r.Code)
	if !r.Code.IsZero() && !ok {
		w.warnOnce(fmt.Errorf("unregistered code %q", r.Code))
		return ""
	}
	if !w.stores(ev.Level) {
		return ""
	}
	now, id := r.at, r.id
	if id == "" {
		now = w.now()
		id = w.ids.next(now)
	}
	now = now.UTC()
	rec := store.DiagRecord{
		ID: id, Schema: Schema, TS: now, Level: string(ev.Level), Component: ev.Component,
		Event: ev.Name.String(), Code: r.Code.String(), Msg: w.scrub(r.Msg, maxMsg), Attrs: w.attrs(r.Attrs),
		Signal: r.Signal, AggyVersion: w.version, OS: runtime.GOOS + "/" + runtime.GOARCH,
		RunID: w.run, TraceID: r.Trace, CauseID: r.Cause, ParentID: r.Parent, ChildID: r.Child,
		BorrowID: r.Borrow, WorktreeID: r.Worktree, Vendor: r.Vendor, VendorSessionID: r.VendorSession,
	}
	if r.Duration > 0 {
		ms := r.Duration.Milliseconds()
		rec.DurationMS = &ms
	}
	if r.Exit != nil {
		x := int64(*r.Exit)
		rec.ExitCode = &x
	}
	// The command may be ending on a signal; the record still goes out.
	ctx = context.WithoutCancel(ctx)
	if err := w.insert(ctx, &rec); err != nil {
		if !errors.Is(err, errNoState) {
			w.warnOnce(err)
		}
		return ""
	}
	if code.Security {
		w.refusal(ctx, rec)
	}
	return rec.ID
}

// insert fills in the trace and writes rec to diag.db. The log is opened
// once and kept open for the process (opening migrates it), and opened
// again when its path changes or a write fails. A WAL left by a process
// that never closed it is recovered by the next open.
func (w *Writer) insert(ctx context.Context, rec *store.DiagRecord) (err error) {
	path, err := w.diagPath()
	if err != nil {
		return err
	}
	// The log never makes the state dir: without one, aggy has kept no
	// state here (a first run's refusal, or a borrow child whose HOME is
	// not aggy's), and a record is not worth leaving one behind.
	if _, err := safefs.Stat(filepath.Dir(path)); errors.Is(err, fs.ErrNotExist) {
		return errNoState
	}
	w.dbMu.Lock()
	defer w.dbMu.Unlock()
	if w.db != nil && w.dbPath != path {
		_ = w.db.Close()
		w.db = nil
	}
	if w.db == nil {
		if w.db, err = store.OpenDiag(ctx, path); err != nil {
			return err
		}
		w.dbPath = path
	}
	db := w.db
	defer func() {
		if err != nil {
			_ = db.Close()
			w.db = nil
		}
	}()
	if rec.TraceID == "" && rec.BorrowID != "" {
		if rec.TraceID, err = db.BorrowTrace(ctx, rec.BorrowID); err != nil {
			return err
		}
	}
	if rec.TraceID == "" {
		rec.TraceID = w.trace
	}
	return db.Insert(ctx, *rec)
}

// errNoState is a record not written because the state dir does not
// exist; it is not warned about.
var errNoState = errors.New("diag: no state dir")

// refusal appends rec to the index's events as a refusal, only when the
// index exists: it is opened without being made or migrated
// (store.OpenExisting). Any other failure is warned once, like a diag
// write.
func (w *Writer) refusal(ctx context.Context, rec store.DiagRecord) {
	path, err := w.indexPath()
	if err == nil {
		var s *store.Store
		if s, err = store.OpenExisting(ctx, path); errors.Is(err, store.ErrNoIndex) {
			return
		} else if err == nil {
			detail := map[string]any{"code": rec.Code, "event": rec.Event, "run": rec.RunID, "record": rec.ID}
			for k, v := range map[string]string{"borrow": rec.BorrowID, "vendor": rec.Vendor, "trace": rec.TraceID} {
				if v != "" {
					detail[k] = v
				}
			}
			err = s.AppendEvent(ctx, store.Event{At: rec.TS, ParentID: rec.ParentID, ChildID: rec.ChildID, Type: store.EventRefusal, Detail: detail})
			_ = s.Close()
		}
	}
	if err != nil {
		w.warnOnce(fmt.Errorf("the refusal was not added to aggy log: %w", err))
	}
}

func (w *Writer) warnOnce(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.warned || w.warn == nil {
		return
	}
	w.warned = true
	fmt.Fprintf(w.warn, "aggy: warning: the diagnostic log was not written: %s %s\n", termsafe.Field(err.Error()), w.Tag(CodeDiagWrite))
}

// LockdownEvent is vendor's lockdown_rejected event; "" for another name.
func LockdownEvent(vendor string) Event {
	switch vendor {
	case "claude":
		return EvClaudeLockdown
	case "codex":
		return EvCodexLockdown
	case "grok":
		return EvGrokLockdown
	}
	return Event{}
}

// Int returns a pointer to n, for Rec.Exit.
func Int(n int) *int { return &n }
