package store

import (
	"context"
	"encoding/json"
	"time"
)

// Event types written to the events table and shown by aggy log.
const (
	// EventScreen: a harness came on screen. Detail: vendor, model.
	EventScreen = "screen"
	// EventSwitch: the on-screen harness changed. Detail: from, to,
	// model, reason.
	EventSwitch = "switch"
	// EventBorrow: a borrow finished. Detail: borrow, kind, vendor,
	// model, tokens, ms, outcome, and group for a --vendor both run.
	EventBorrow = "borrow"
	// EventFiles: files changed during a turn. Detail: files.
	EventFiles = "files"
	// EventSession: the on-screen CLI reported its session id. Detail:
	// vendor, session, source, via ("hook" or "sessions folder").
	EventSession = "session"
	// EventLimit: a vendor reported a plan limit. Detail: vendor, until
	// (unix ms, when aggy expects it back), via (hook-unverified, or
	// "hook" in older rows; rollout; or borrow). internal/limits writes and reads it.
	EventLimit = "limit"
	// EventRefusal: aggy refused something for a security reason (a
	// lockdown rejection, a borrow from inside a borrow, a protected path
	// in an edit). Detail: code, event, run, record (the diag.db record
	// id), and borrow, vendor, and trace when known. internal/diag
	// writes it.
	EventRefusal = "refusal"
)

// Event is a row of events.
type Event struct {
	ID       int64
	At       time.Time
	ParentID string
	ChildID  string
	Type     string
	Detail   map[string]any // stored as JSON
}

// AppendEvent adds an event. The table is append-only.
func (s *Store) AppendEvent(ctx context.Context, e Event) error {
	detail := []byte("{}")
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return err
		}
		detail = b
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (at, parent_id, child_id, type, detail) VALUES (?, ?, ?, ?, ?)`,
		millis(e.At), e.ParentID, e.ChildID, e.Type, string(detail))
	return err
}

// EventFilter selects events. Zero values select everything.
type EventFilter struct {
	ParentID string
	Limit    int // the latest Limit events; 0 means all
}

// Events returns events oldest first.
func (s *Store) Events(ctx context.Context, f EventFilter) ([]Event, error) {
	q := `SELECT id, at, parent_id, child_id, type, detail FROM events`
	var args []any
	if f.ParentID != "" {
		q += ` WHERE parent_id = ?`
		args = append(args, f.ParentID)
	}
	q += ` ORDER BY at DESC, id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		var detail string
		if err := rows.Scan(&e.ID, &at, &e.ParentID, &e.ChildID, &e.Type, &detail); err != nil {
			return nil, err
		}
		e.At = fromMillis(at)
		if err := json.Unmarshal([]byte(detail), &e.Detail); err != nil {
			e.Detail = map[string]any{"raw": detail}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// LimitEvents returns up to limit limit events of vendor at or after
// since, either those from a hook (hook true) or the others, latest
// "until" first. Hook rows are kept apart so that any number of them
// cannot push a confirmed row out of the window.
func (s *Store) LimitEvents(ctx context.Context, vendor string, hook bool, since time.Time, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, parent_id, child_id, type, detail FROM events
		 WHERE type = ? AND at >= ? AND json_valid(detail)
		   AND json_extract(detail, '$.vendor') = ?
		   AND (coalesce(json_extract(detail, '$.via'), '') IN ('hook', 'hook-unverified')) = ?
		 ORDER BY json_extract(detail, '$.until') DESC, id DESC LIMIT ?`,
		EventLimit, millis(since), vendor, hook, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		var detail string
		if err := rows.Scan(&e.ID, &at, &e.ParentID, &e.ChildID, &e.Type, &detail); err != nil {
			return nil, err
		}
		e.At = fromMillis(at)
		if json.Unmarshal([]byte(detail), &e.Detail) != nil {
			e.Detail = nil
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
