package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/safefs"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// The diagnostic log, ~/.aggy/diag.db (docs/logging.md). It is a database
// of its own, apart from the index, so heavy logging never waits on the
// index's writers and is pruned on its own. internal/diag writes it;
// aggy diag reads it.

// DiagFileName is the diagnostic log inside the state directory.
const DiagFileName = "diag.db"

// DiagPath returns the diagnostic log's path in the state directory.
func DiagPath() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, DiagFileName), nil
}

// DiagRecord is one row of the diagnostic log's records table. Its JSON
// form is what aggy diag --json and export print.
type DiagRecord struct {
	Row             int64           `json:"-"`
	ID              string          `json:"id"`
	Schema          int             `json:"schema"`
	TS              time.Time       `json:"ts"`
	Level           string          `json:"level"`
	Component       string          `json:"component"`
	Event           string          `json:"event"`
	Code            string          `json:"code,omitempty"`
	Msg             string          `json:"msg,omitempty"`
	Attrs           json.RawMessage `json:"attrs,omitempty"`
	DurationMS      *int64          `json:"duration_ms,omitempty"`
	ExitCode        *int64          `json:"exit_code,omitempty"`
	Signal          string          `json:"signal,omitempty"`
	AggyVersion     string          `json:"aggy_version,omitempty"`
	OS              string          `json:"os,omitempty"`
	RunID           string          `json:"run_id"`
	TraceID         string          `json:"trace_id,omitempty"`
	CauseID         string          `json:"cause_id,omitempty"`
	ParentID        string          `json:"parent_id,omitempty"`
	ChildID         string          `json:"child_id,omitempty"`
	BorrowID        string          `json:"borrow_id,omitempty"`
	WorktreeID      string          `json:"worktree_id,omitempty"`
	Vendor          string          `json:"vendor,omitempty"`
	VendorSessionID string          `json:"vendor_session_id,omitempty"`
}

// diagCols are the records columns in DiagRecord's order after seq.
const diagCols = `id, schema, ts, level, component, event, code, msg, attrs, duration_ms, exit_code, signal,
	aggy_version, os, run_id, trace_id, cause_id, parent_id, child_id, borrow_id, worktree_id, vendor, vendor_session_id`

// DiagDB is the diagnostic log opened for writing.
type DiagDB struct{ s *Store }

// OpenDiag opens or creates the diagnostic log at path and migrates it,
// as Open does the index: 0600, in a 0700 directory it creates, checked
// through safefs, with its own user_version migrations.
func OpenDiag(ctx context.Context, path string) (*DiagDB, error) {
	s, err := openRW(ctx, path, "diagmigrations")
	if err != nil {
		return nil, err
	}
	return &DiagDB{s}, nil
}

// Close closes the log.
func (d *DiagDB) Close() error { return d.s.Close() }

// Insert adds one record.
func (d *DiagDB) Insert(ctx context.Context, r DiagRecord) error {
	attrs := string(r.Attrs)
	if attrs == "" {
		attrs = "{}"
	}
	_, err := d.s.db.ExecContext(ctx,
		`INSERT INTO records (id, schema, ts, level, component, event, code, msg, attrs, duration_ms, exit_code, signal,
		  aggy_version, os, run_id, trace_id, cause_id, parent_id, child_id, borrow_id, worktree_id, vendor, vendor_session_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Schema, millis(r.TS), r.Level, r.Component, r.Event, r.Code, r.Msg, attrs, r.DurationMS, r.ExitCode, r.Signal,
		r.AggyVersion, r.OS, r.RunID, r.TraceID, r.CauseID, r.ParentID, r.ChildID, r.BorrowID, r.WorktreeID, r.Vendor, r.VendorSessionID)
	return err
}

// BorrowTrace returns the trace of the earliest record about borrow id,
// or "" when there is none: an accept, check, or gc acting on a borrow
// joins the flow that started it.
func (d *DiagDB) BorrowTrace(ctx context.Context, id string) (string, error) {
	var t string
	err := d.s.db.QueryRowContext(ctx,
		`SELECT trace_id FROM records WHERE borrow_id = ? AND trace_id <> '' ORDER BY ts, seq LIMIT 1`, id).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return t, err
}

// Prune deletes the records older than before; then, when the log's
// pages in use are over maxBytes (0: no size cap), the oldest records:
// the share of them that holds the excess, worked out from the size of
// the rows (the pages in use less the empty schema's), in one pass. It
// compacts the file after each delete and returns how many it deleted.
// Should the compacted file still be over (pages are not filled evenly),
// it deletes again, at most sizePasses times in all.
func (d *DiagDB) Prune(ctx context.Context, before time.Time, maxBytes int64) (int64, error) {
	res, err := d.s.db.ExecContext(ctx, `DELETE FROM records WHERE ts < ?`, millis(before))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		// Compacted first, so the size measured next is the data's.
		if _, err := d.s.db.ExecContext(ctx, `VACUUM`); err != nil {
			return n, err
		}
	}
	for pass := 0; maxBytes > 0 && pass < sizePasses; pass++ {
		drop, err := d.overBy(ctx, maxBytes)
		if err != nil || drop == 0 {
			return n, err
		}
		res, err := d.s.db.ExecContext(ctx,
			`DELETE FROM records WHERE seq IN (SELECT seq FROM records ORDER BY ts, seq LIMIT ?)`, drop)
		if err != nil {
			return n, err
		}
		k, _ := res.RowsAffected()
		if k == 0 {
			break
		}
		n += k
		if _, err := d.s.db.ExecContext(ctx, `VACUUM`); err != nil {
			return n, err
		}
	}
	return n, nil
}

// sizePasses bounds Prune's deletes by size.
const sizePasses = 3

// overBy is how many of the oldest records to delete to bring the log's
// pages in use down to maxBytes; 0 when it fits.
func (d *DiagDB) overBy(ctx context.Context, maxBytes int64) (int64, error) {
	used, err := d.used(ctx)
	if err != nil || used <= maxBytes {
		return 0, err
	}
	var count, trees, size int64
	if err := d.s.db.QueryRowContext(ctx, `SELECT count(*) FROM records`).Scan(&count); err != nil {
		return 0, err
	}
	// An empty log is one page for the schema and one per table and index.
	if err := d.s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE rootpage > 0`).Scan(&trees); err != nil {
		return 0, err
	}
	if err := d.s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size); err != nil {
		return 0, err
	}
	rows := used - (trees+1)*size
	if rows <= 0 || maxBytes <= (trees+1)*size {
		return count, nil
	}
	drop := int64(math.Ceil(float64(count) * float64(used-maxBytes) / float64(rows)))
	return min(max(drop, 1), count), nil
}

// used is the bytes of the log's pages that hold data.
func (d *DiagDB) used(ctx context.Context) (int64, error) {
	var pages, free, size int64
	if err := d.s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := d.s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return 0, err
	}
	if err := d.s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size); err != nil {
		return 0, err
	}
	return (pages - free) * size, nil
}

// DiagView reads the diagnostic log and the index together, read-only:
// an in-memory main database with diag.db attached as "diag" and the
// index as "idx", each only when its file exists. It never creates
// either file. Every connection has query_only on and the same
// select-only authorizer as OpenReadOnly, installed after the attaches,
// so a query cannot write, ATTACH, change a PRAGMA, or VACUUM.
type DiagView struct {
	db           *sql.DB
	diag, index  bool
	indexVersion int
}

// HasDiag reports whether diag.db exists and is attached.
func (v *DiagView) HasDiag() bool { return v.diag }

// HasIndex reports whether the index exists and is attached.
func (v *DiagView) HasIndex() bool { return v.index }

// IndexVersion is the attached index's schema version, 0 without one.
func (v *DiagView) IndexVersion() int { return v.indexVersion }

// Close closes the view.
func (v *DiagView) Close() error { return v.db.Close() }

// attachSpec is one file a view attaches.
type attachSpec struct{ name, uri string }

// OpenDiagView opens diagPath and indexPath for reading as DiagView
// documents. A path that is "" or whose file does not exist is left out; with neither,
// the view reads nothing and says so (HasDiag, HasIndex).
func OpenDiagView(ctx context.Context, diagPath, indexPath string) (*DiagView, error) {
	v := &DiagView{}
	var specs []attachSpec
	var pins []dbPin
	for _, a := range []struct {
		name, path string
		has        *bool
	}{{"diag", diagPath, &v.diag}, {"idx", indexPath, &v.index}} {
		if a.path == "" {
			continue
		}
		p, err := filepath.Abs(a.path)
		if err != nil {
			return nil, err
		}
		if _, err := safefs.Stat(p); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		for _, f := range []string{p, p + "-wal", p + "-shm"} {
			if err := safefs.Check(f); err != nil {
				return nil, err
			}
		}
		want, err := pinDB(p)
		if err != nil {
			return nil, err
		}
		writable, err := safefs.DirWritable(filepath.Dir(p))
		if err != nil {
			return nil, err
		}
		specs = append(specs, attachSpec{a.name, attachURI(p, !writable)})
		pins = append(pins, dbPin{p, want})
		*a.has = true
	}
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "query_only(1)")
	base, err := sqlite.NewConnector("file::memory:?" + q.Encode())
	if err != nil {
		return nil, err
	}
	// Every connection confirms SQLite attached the pinned files and that
	// safefs clears them and their WAL files (pin.go).
	v.db = sql.OpenDB(pinned{attaching{selectOnly{base}, specs}, pins})
	if err := v.db.PingContext(ctx); err != nil {
		_ = v.db.Close()
		return nil, fmt.Errorf("%w: %w", ErrUnreadable, ReadError(err))
	}
	if v.diag {
		// A diag.db its writer has made but not yet migrated has no
		// records table: the view reads it as not there yet.
		var n int
		if err := v.db.QueryRowContext(ctx, `SELECT count(*) FROM diag.sqlite_master WHERE type = 'table' AND name = 'records'`).Scan(&n); err != nil {
			_ = v.db.Close()
			return nil, fmt.Errorf("%w: %w", ErrUnreadable, ReadError(err))
		}
		v.diag = n == 1
	}
	if v.index {
		if err := v.db.QueryRowContext(ctx, `PRAGMA idx.user_version`).Scan(&v.indexVersion); err != nil {
			_ = v.db.Close()
			return nil, fmt.Errorf("%w: %w", ErrUnreadable, ReadError(err))
		}
	}
	return v, nil
}

// attachURI names exactly path for ATTACH, never creating it, the way
// roDSN does for OpenReadOnly: immutable when no writer can exist, else
// mode=rw under the connection's query_only.
func attachURI(path string, immutable bool) string {
	q := url.Values{}
	if immutable {
		q.Add("mode", "ro")
		q.Add("immutable", "1")
	} else {
		q.Add("mode", "rw")
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}
	return u.String()
}

// attaching attaches its files to each new connection before the
// select-only authorizer goes on, which then refuses any other ATTACH.
type attaching struct {
	base  selectOnly
	specs []attachSpec
}

// Driver returns the base connector's driver.
func (c attaching) Driver() driver.Driver { return c.base.Driver() }

// Connect opens a connection, attaches the files, and installs the
// authorizer.
func (c attaching) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	ex, ok := conn.(driver.ExecerContext)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("store: the SQLite driver cannot exec")
	}
	for _, s := range c.specs {
		// The schema name is one of two constants, never input.
		if _, err := ex.ExecContext(ctx, "ATTACH DATABASE ? AS "+s.name, []driver.NamedValue{{Ordinal: 1, Value: s.uri}}); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	reg, ok := conn.(sqlite.AuthorizerRegisterer)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("store: the SQLite driver has no authorizer")
	}
	if err := reg.RegisterAuthorizer(readOnlyAuthorizer); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// DiagFilter selects records. Zero values select everything.
type DiagFilter struct {
	Since, Until time.Time
	// Levels are the levels to keep; empty keeps all.
	Levels []string
	// Event and Code are GLOB patterns ("borrow.*"); a plain name
	// matches itself.
	Event, Code                           string
	Component, Vendor, Run, Trace, Parent string
	Child, Borrow                         string
	// AfterRow keeps records written after this seq (aggy diag --follow).
	AfterRow int64
	// Limit keeps the latest Limit records; 0 keeps all.
	Limit int
}

// Records returns the records f selects, oldest first. Without diag.db
// there are none.
func (v *DiagView) Records(ctx context.Context, f DiagFilter) ([]DiagRecord, error) {
	if !v.diag {
		return nil, nil
	}
	var where []string
	var args []any
	add := func(cond string, a any) {
		where = append(where, cond)
		args = append(args, a)
	}
	if !f.Since.IsZero() {
		add("ts >= ?", millis(f.Since))
	}
	if !f.Until.IsZero() {
		add("ts <= ?", millis(f.Until))
	}
	if len(f.Levels) > 0 {
		where = append(where, "level IN (?"+strings.Repeat(", ?", len(f.Levels)-1)+")")
		for _, l := range f.Levels {
			args = append(args, l)
		}
	}
	for _, c := range []struct{ col, val string }{
		{"component", f.Component}, {"vendor", f.Vendor}, {"run_id", f.Run}, {"trace_id", f.Trace},
		{"parent_id", f.Parent}, {"child_id", f.Child}, {"borrow_id", f.Borrow},
	} {
		if c.val != "" {
			add(c.col+" = ?", c.val)
		}
	}
	if f.Event != "" {
		add("event GLOB ?", f.Event)
	}
	if f.Code != "" {
		add("code GLOB ?", f.Code)
	}
	seq, err := v.seqCol(ctx)
	if err != nil {
		return nil, err
	}
	if f.AfterRow > 0 {
		add(seq+" > ?", f.AfterRow)
	}
	q := `SELECT ` + seq + `, ` + diagCols + ` FROM diag.records`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY ts DESC, ` + seq + ` DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	out, err := v.scanRecords(ctx, q, args...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, err
}

// RecordsAbout returns, oldest first, every record whose id, run, trace,
// parent, child, borrow, or worktree id is id.
func (v *DiagView) RecordsAbout(ctx context.Context, id string) ([]DiagRecord, error) {
	if !v.diag {
		return nil, nil
	}
	seq, err := v.seqCol(ctx)
	if err != nil {
		return nil, err
	}
	return v.scanRecords(ctx, `SELECT `+seq+`, `+diagCols+` FROM diag.records
		WHERE ? IN (id, run_id, trace_id, parent_id, child_id, borrow_id, worktree_id) ORDER BY ts, `+seq, id)
}

// seqCol is the records' sequence column: seq from diag.db version 2,
// and before it (a log no aggy has written since the upgrade, which a
// read-only view cannot migrate) the rowid, which version 2 keeps as
// seq. It is read per query: a writer may migrate the log meanwhile.
func (v *DiagView) seqCol(ctx context.Context) (string, error) {
	n, err := v.DiagVersion(ctx)
	if err != nil {
		return "", err
	}
	if n < 2 {
		return "rowid", nil
	}
	return "seq", nil
}

func (v *DiagView) scanRecords(ctx context.Context, q string, args ...any) ([]DiagRecord, error) {
	rows, err := v.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, ReadError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []DiagRecord
	for rows.Next() {
		var r DiagRecord
		var ts int64
		var attrs string
		var dur, exit sql.NullInt64
		if err := rows.Scan(&r.Row, &r.ID, &r.Schema, &ts, &r.Level, &r.Component, &r.Event, &r.Code, &r.Msg, &attrs,
			&dur, &exit, &r.Signal, &r.AggyVersion, &r.OS, &r.RunID, &r.TraceID, &r.CauseID, &r.ParentID, &r.ChildID,
			&r.BorrowID, &r.WorktreeID, &r.Vendor, &r.VendorSessionID); err != nil {
			return nil, err
		}
		r.TS = fromMillis(ts)
		if json.Valid([]byte(attrs)) && attrs != "{}" {
			r.Attrs = json.RawMessage(attrs)
		}
		if dur.Valid {
			r.DurationMS = &dur.Int64
		}
		if exit.Valid {
			r.ExitCode = &exit.Int64
		}
		out = append(out, r)
	}
	return out, ReadError(rows.Err())
}

// CountBefore counts the records older than t.
func (v *DiagView) CountBefore(ctx context.Context, t time.Time) (int64, error) {
	if !v.diag {
		return 0, nil
	}
	var n int64
	err := v.db.QueryRowContext(ctx, `SELECT count(*) FROM diag.records WHERE ts < ?`, millis(t)).Scan(&n)
	return n, ReadError(err)
}

// DiagVersion is diag.db's schema version, 0 without one.
func (v *DiagView) DiagVersion(ctx context.Context) (int, error) {
	if !v.diag {
		return 0, nil
	}
	var n int
	err := v.db.QueryRowContext(ctx, `PRAGMA diag.user_version`).Scan(&n)
	return n, ReadError(err)
}

// QueryMaxValue is the most bytes one string or blob may hold in a
// Query: SQLite's length limit on its connection, so a statement that
// builds or reads a bigger value (randomblob(5e8)) fails with "string or
// blob too big" before allocating it. It also caps a row SQLite builds,
// such as a sorter record. aggy's own rows are far smaller.
const QueryMaxValue = 16 << 20

// Query runs one read-only statement (aggy diag sql) and calls row for
// each result row as it is read, at most limit rows (0: no cap), each
// value as SQLite gave it, none longer than QueryMaxValue; cols is called
// first with the column names. It reports whether it stopped at limit.
// ctx bounds the query: SQLite is interrupted when it ends.
func (v *DiagView) Query(ctx context.Context, q string, limit int, cols func([]string) error, row func([]any) error) (bool, error) {
	conn, err := v.db.Conn(ctx)
	if err != nil {
		return false, ReadError(err)
	}
	defer func() { _ = conn.Close() }()
	old, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_LENGTH, QueryMaxValue)
	if err != nil {
		return false, err
	}
	// The connection goes back to the pool: restore its limit after.
	defer func() { _, _ = sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_LENGTH, old) }()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return false, ReadError(err)
	}
	defer func() { _ = rows.Close() }()
	names, err := rows.Columns()
	if err != nil {
		return false, err
	}
	if err := cols(names); err != nil {
		return false, err
	}
	n := 0
	for rows.Next() {
		if limit > 0 && n == limit {
			return true, nil
		}
		vals := make([]any, len(names))
		ptrs := make([]any, len(names))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return false, err
		}
		if err := row(vals); err != nil {
			return false, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return false, ReadError(err)
	}
	return false, ctx.Err()
}

// EventsAbout returns, oldest first, the index's events about any of the
// children or parents named, any borrow named in an event's detail, or
// any refusal event that names one of runs. Without the index there are
// none.
func (v *DiagView) EventsAbout(ctx context.Context, children, parents, borrows, runs []string) ([]Event, error) {
	if !v.index {
		return nil, nil
	}
	in := func(col string, ids []string, args *[]any) string {
		if len(ids) == 0 {
			return "0"
		}
		for _, id := range ids {
			*args = append(*args, id)
		}
		return col + " IN (?" + strings.Repeat(", ?", len(ids)-1) + ")"
	}
	var args []any
	q := `SELECT id, at, parent_id, child_id, type, detail FROM idx.events WHERE ` +
		in("child_id", children, &args) + ` OR ` + in("parent_id", parents, &args) +
		` OR (json_valid(detail) AND (` + in("json_extract(detail, '$.borrow')", borrows, &args) +
		` OR ` + in("json_extract(detail, '$.run')", runs, &args) + `)) ORDER BY at, id`
	rows, err := v.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, ReadError(err)
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
	return out, ReadError(rows.Err())
}

// BorrowRow is a borrow as a timeline shows it.
type BorrowRow struct {
	ID, ChildID, ParentID, Kind, Vendor, Model, Status, Outcome string
	Tokens                                                      int64
	StartedAt, EndedAt                                          time.Time
}

// BorrowRows returns the index's borrows with these ids, or whose child
// is one of children, with each child's parent.
func (v *DiagView) BorrowRows(ctx context.Context, ids, children []string) ([]BorrowRow, error) {
	if !v.index || len(ids)+len(children) == 0 {
		return nil, nil
	}
	var args []any
	var conds []string
	for _, set := range []struct {
		col string
		ids []string
	}{{"b.id", ids}, {"b.child_id", children}} {
		if len(set.ids) == 0 {
			continue
		}
		conds = append(conds, set.col+" IN (?"+strings.Repeat(", ?", len(set.ids)-1)+")")
		for _, id := range set.ids {
			args = append(args, id)
		}
	}
	rows, err := v.db.QueryContext(ctx,
		`SELECT b.id, COALESCE(b.child_id, ''), COALESCE(c.parent_id, ''), b.kind, b.vendor, b.model, b.status, b.outcome,
		        b.tokens, b.started_at, b.ended_at
		 FROM idx.borrows b LEFT JOIN idx.children c ON c.id = b.child_id
		 WHERE `+strings.Join(conds, " OR ")+` ORDER BY b.started_at, b.id`, args...)
	if err != nil {
		return nil, ReadError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []BorrowRow
	for rows.Next() {
		var b BorrowRow
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&b.ID, &b.ChildID, &b.ParentID, &b.Kind, &b.Vendor, &b.Model, &b.Status, &b.Outcome,
			&b.Tokens, &started, &ended); err != nil {
			return nil, err
		}
		b.StartedAt, b.EndedAt = fromMillis(started), fromNull(ended)
		out = append(out, b)
	}
	return out, ReadError(rows.Err())
}

// IndexKind says which index table holds id: "borrow", "child",
// "parent", or "" for none.
func (v *DiagView) IndexKind(ctx context.Context, id string) (string, error) {
	if !v.index {
		return "", nil
	}
	var kind string
	err := v.db.QueryRowContext(ctx,
		`SELECT 'borrow' FROM idx.borrows WHERE id = ?1
		 UNION ALL SELECT 'child' FROM idx.children WHERE id = ?1
		 UNION ALL SELECT 'parent' FROM idx.parents WHERE id = ?1 LIMIT 1`, id).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return kind, ReadError(err)
}
