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
	// EventBorrow: a borrow finished. Detail: kind, vendor, model,
	// outcome.
	EventBorrow = "borrow"
	// EventFiles: files changed during a turn. Detail: files.
	EventFiles = "files"
	// EventSession: the on-screen CLI reported its session id. Detail:
	// vendor, session, source, via ("hook" or "sessions folder").
	EventSession = "session"
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
