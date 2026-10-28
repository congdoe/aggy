package store

import (
	"context"
	"database/sql"
	"sort"
	"time"
)

// RedoWindow is how long after a borrow ends a re-ask still counts as a
// redo of it (docs/design.md, section 8).
const RedoWindow = 24 * time.Hour

// StatsSchema is the oldest schema BorrowStats reads: it needs the
// children's worktree state (version 12).
const StatsSchema = 12

// BorrowStat is what aggy recorded of one vendor's finished borrows of
// one kind. Tokens and time are summed over the OK borrows only.
type BorrowStat struct {
	Kind     string
	Vendor   string
	Runs     int // finished borrows: done, failed, or killed
	OK       int // outcome "ok"
	Redone   int // OK borrows that were redone
	OKTokens int64
	OKTime   time.Duration
}

// statRow is one finished borrow, as the redo rule sees it.
type statRow struct {
	kind, vendor, outcome string
	taskHash              string
	parent                string // "" when the child is gone
	group                 string // the --vendor both run it was part of, or ""
	rejected              bool   // an edit whose worktree was rejected
	tokens                int64
	start, end            time.Time
}

// BorrowStats returns the finished borrows per kind and vendor, sorted by
// kind then vendor, with how many of the OK ones were redone (see
// markRedone). It only reads, so it works on a read-only store.
func (s *Store) BorrowStats(ctx context.Context) ([]BorrowStat, error) {
	groups, err := s.borrowGroups(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT b.kind, b.vendor, b.outcome, b.task_hash, COALESCE(c.parent_id, ''), COALESCE(b.child_id, ''),
		        COALESCE(c.worktree_state, '') = ?, b.tokens, b.started_at, COALESCE(b.ended_at, b.started_at)
		 FROM borrows b LEFT JOIN children c ON c.id = b.child_id
		 WHERE b.status <> 'running'
		 ORDER BY b.started_at, b.id`, WorktreeRejected)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var all []statRow
	for rows.Next() {
		var r statRow
		var child string
		var start, end int64
		if err := rows.Scan(&r.kind, &r.vendor, &r.outcome, &r.taskHash, &r.parent, &child,
			&r.rejected, &r.tokens, &start, &end); err != nil {
			return nil, err
		}
		r.start, r.end, r.group = fromMillis(start), fromMillis(end), groups[child]
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	redone := markRedone(all, RedoWindow)
	byKey := map[[2]string]*BorrowStat{}
	var out []*BorrowStat
	for i, r := range all {
		k := [2]string{r.kind, r.vendor}
		st := byKey[k]
		if st == nil {
			st = &BorrowStat{Kind: r.kind, Vendor: r.vendor}
			byKey[k] = st
			out = append(out, st)
		}
		st.Runs++
		if r.outcome != "ok" {
			continue
		}
		st.OK++
		st.OKTokens += r.tokens
		st.OKTime += max(r.end.Sub(r.start), 0)
		if redone[i] {
			st.Redone++
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Vendor < out[j].Vendor
	})
	res := make([]BorrowStat, len(out))
	for i, st := range out {
		res[i] = *st
	}
	return res, nil
}

// borrowGroups maps a child id to the --vendor both run its borrow was
// part of, from the "group" of its borrow event.
func (s *Store) borrowGroups(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT child_id, json_extract(detail, '$.group') FROM events
		 WHERE type = ? AND json_valid(detail) AND json_type(detail, '$.group') = 'text'`, EventBorrow)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var child string
		var group sql.NullString
		if err := rows.Scan(&child, &group); err != nil {
			return nil, err
		}
		if child != "" && group.String != "" {
			out[child] = group.String
		}
	}
	return out, rows.Err()
}

// markRedone says which borrows were redone. rows are sorted by start. An
// OK borrow A is redone when a later borrow B of the same kind in the same
// parent starts after A ended and at most window later, B is not from the
// same --vendor both run, and either B has A's task hash (the same
// question, re-asked of any vendor) or A is an edit whose worktree was
// rejected. A borrow with no parent left is never matched. A failed
// borrow is never redone: it already counts as a failure.
func markRedone(rows []statRow, window time.Duration) []bool {
	out := make([]bool, len(rows))
	for i, a := range rows {
		if a.outcome != "ok" || a.parent == "" {
			continue
		}
		// B starts at or after A's end, so at or after A's start: only
		// later rows can match.
		for _, b := range rows[i+1:] {
			if b.start.Sub(a.end) > window {
				break
			}
			if b.start.Before(a.end) || b.parent != a.parent || b.kind != a.kind ||
				(a.group != "" && b.group == a.group) {
				continue
			}
			if (a.taskHash != "" && b.taskHash == a.taskHash) || (a.kind == "edit" && a.rejected) {
				out[i] = true
				break
			}
		}
	}
	return out
}
