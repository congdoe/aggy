package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func diagRec(id string, ts time.Time) DiagRecord {
	return DiagRecord{ID: id, Schema: 1, TS: ts, Level: "info", Component: "borrow", Event: "borrow.child.started", RunID: "r1", TraceID: "t1", BorrowID: "b1"}
}

func TestDiagOpenInsertPrune(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", DiagFileName)
	d, err := OpenDiag(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("diag.db: %v, %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir: %v, %v", fi, err)
	}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		r := diagRec(fmt.Sprintf("id%03d", i), base.Add(time.Duration(i)*time.Hour))
		r.Msg = strings.Repeat("x", 2000)
		if err := d.Insert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if tr, err := d.BorrowTrace(ctx, "b1"); err != nil || tr != "t1" {
		t.Errorf("BorrowTrace = %q, %v", tr, err)
	}
	if tr, err := d.BorrowTrace(ctx, "nope"); err != nil || tr != "" {
		t.Errorf("BorrowTrace(nope) = %q, %v", tr, err)
	}
	// Age: the first 10 hours go.
	n, err := d.Prune(ctx, base.Add(10*time.Hour), 0)
	if err != nil || n != 10 {
		t.Fatalf("Prune by age = %d, %v", n, err)
	}
	// Size: down to what fits in 64 KB, oldest first.
	n, err = d.Prune(ctx, base, 64<<10)
	if err != nil || n == 0 {
		t.Fatalf("Prune by size = %d, %v", n, err)
	}
	used, err := d.used(ctx)
	if err != nil || used > 64<<10 {
		t.Errorf("used = %d, %v", used, err)
	}
	v, err := OpenDiagView(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v.Close() }()
	recs, err := v.Records(ctx, DiagFilter{})
	if err != nil || len(recs) == 0 || recs[len(recs)-1].ID != "id099" || len(recs) >= 90 {
		t.Errorf("after prune: %d records, %v", len(recs), err)
	}
}

// TestDiagViewReadOnly: the view creates neither file, refuses every
// write, ATTACH, and PRAGMA change, and reads both databases.
func TestDiagViewReadOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dp, ip := filepath.Join(dir, DiagFileName), filepath.Join(dir, FileName)

	v, err := OpenDiagView(ctx, dp, ip)
	if err != nil {
		t.Fatal(err)
	}
	if v.HasDiag() || v.HasIndex() {
		t.Error("view says missing files exist")
	}
	if recs, err := v.Records(ctx, DiagFilter{}); err != nil || recs != nil {
		t.Errorf("Records without diag.db = %v, %v", recs, err)
	}
	_ = v.Close()
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("the view made files: %v", ents)
	}

	d, err := OpenDiag(ctx, dp)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Insert(ctx, diagRec("r1", time.Now())); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	s, err := Open(ctx, ip)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, Event{At: time.Now(), ChildID: "c1", Type: EventBorrow, Detail: map[string]any{"borrow": "b1"}}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	v, err = OpenDiagView(ctx, dp, ip)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v.Close() }()
	if !v.HasDiag() || !v.HasIndex() || v.IndexVersion() == 0 {
		t.Fatalf("view: diag %v index %v version %d", v.HasDiag(), v.HasIndex(), v.IndexVersion())
	}
	cols, rows, err := queryAll(ctx, v, `SELECT r.id, e.type FROM records r JOIN idx.events e ON json_extract(e.detail, '$.borrow') = r.borrow_id`)
	if err != nil || len(cols) != 2 || len(rows) != 1 {
		t.Fatalf("join = %v %v, %v", cols, rows, err)
	}
	for _, q := range []string{
		`INSERT INTO records (id, schema, ts, level, component, event, run_id) VALUES ('x', 1, 0, 'info', 'c', 'e', 'r')`,
		`DELETE FROM records`,
		`DELETE FROM idx.events`,
		`UPDATE idx.borrows SET status = 'done'`,
		`DROP TABLE records`,
		`ATTACH DATABASE '` + filepath.Join(dir, "other.db") + `' AS other`,
		`PRAGMA query_only = 0`,
		`PRAGMA diag.user_version = 9`,
		`VACUUM`,
		`CREATE TEMP TABLE t (x)`,
	} {
		if _, _, err := queryAll(ctx, v, q); err == nil {
			t.Errorf("%s: allowed", q)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "other.db")); err == nil {
		t.Error("ATTACH made a file")
	}
	if kind, err := v.IndexKind(ctx, "nope"); err != nil || kind != "" {
		t.Errorf("IndexKind = %q, %v", kind, err)
	}
	evs, err := v.EventsAbout(ctx, nil, nil, []string{"b1"}, nil)
	if err != nil || len(evs) != 1 {
		t.Errorf("EventsAbout = %v, %v", evs, err)
	}
	if n, err := v.CountBefore(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Errorf("CountBefore = %d, %v", n, err)
	}
}

// TestDiagViewUnmigrated: a diag.db its writer made but has not migrated
// yet reads as not there, not as an error.
func TestDiagViewUnmigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), DiagFileName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := OpenDiagView(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v.Close() }()
	if recs, err := v.Records(context.Background(), DiagFilter{}); v.HasDiag() || err != nil || recs != nil {
		t.Errorf("HasDiag %v, Records %v, %v", v.HasDiag(), recs, err)
	}
}

func TestDiagFilter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), DiagFileName)
	d, err := OpenDiag(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i, r := range []DiagRecord{
		{ID: "a", Level: "info", Component: "borrow", Event: "borrow.child.started", RunID: "r1", BorrowID: "b1"},
		{ID: "b", Level: "error", Component: "accept", Event: "accept.request.refused", Code: "accept.protected_path", RunID: "r2", BorrowID: "b1"},
		{ID: "c", Level: "warn", Component: "gc", Event: "gc.item.failed", Code: "gc.remove_failed", RunID: "r3", Vendor: "grok"},
	} {
		r.Schema, r.TS = 1, base.Add(time.Duration(i)*time.Hour)
		if err := d.Insert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.Close()
	v, err := OpenDiagView(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v.Close() }()
	for _, tt := range []struct {
		name string
		f    DiagFilter
		want string
	}{
		{"all", DiagFilter{}, "abc"},
		{"limit keeps latest", DiagFilter{Limit: 2}, "bc"},
		{"levels", DiagFilter{Levels: []string{"error", "warn"}}, "bc"},
		{"event glob", DiagFilter{Event: "accept.*"}, "b"},
		{"code glob", DiagFilter{Code: "*.remove_failed"}, "c"},
		{"borrow", DiagFilter{Borrow: "b1"}, "ab"},
		{"vendor", DiagFilter{Vendor: "grok"}, "c"},
		{"since", DiagFilter{Since: base.Add(time.Hour)}, "bc"},
		{"until", DiagFilter{Until: base.Add(time.Hour)}, "ab"},
		{"after row", DiagFilter{AfterRow: 2}, "c"},
		{"run", DiagFilter{Run: "r2"}, "b"},
	} {
		recs, err := v.Records(ctx, tt.f)
		got := ""
		for _, r := range recs {
			got += r.ID
		}
		if err != nil || got != tt.want {
			t.Errorf("%s: %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
	about, err := v.RecordsAbout(ctx, "b1")
	if err != nil || len(about) != 2 {
		t.Errorf("RecordsAbout = %v, %v", about, err)
	}
}

// queryAll collects every row of q through DiagView.Query.
func queryAll(ctx context.Context, v *DiagView, q string) ([]string, [][]any, error) {
	var cols []string
	var rows [][]any
	_, err := v.Query(ctx, q, 0, func(c []string) error { cols = c; return nil },
		func(r []any) error { rows = append(rows, r); return nil })
	return cols, rows, err
}

// TestDiagPruneNearTarget (P58-7): the size prune deletes in one pass
// about the oldest share of rows that holds the excess, landing under
// the cap and near it, not far below.
func TestDiagPruneNearTarget(t *testing.T) {
	ctx := context.Background()
	d, err := OpenDiag(ctx, filepath.Join(t.TempDir(), DiagFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	const total = 2000
	for i := 0; i < total; i++ {
		r := diagRec(fmt.Sprintf("id%05d", i), base.Add(time.Duration(i)*time.Second))
		r.Msg = strings.Repeat("y", 1000)
		if err := d.Insert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	before, err := d.used(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := before / 2
	n, err := d.Prune(ctx, base, target)
	if err != nil {
		t.Fatal(err)
	}
	used, err := d.used(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if used > target || used < target*9/10 {
		t.Errorf("used %d of target %d (from %d), %d deleted", used, target, before, n)
	}
	if n < total/3 || n > total*2/3 {
		t.Errorf("deleted %d of %d", n, total)
	}
	var oldest string
	if err := d.s.db.QueryRowContext(ctx, `SELECT id FROM records ORDER BY ts, seq LIMIT 1`).Scan(&oldest); err != nil || oldest != fmt.Sprintf("id%05d", n) {
		t.Errorf("oldest left = %q, %v; want the %d oldest gone", oldest, err, n)
	}
}

// TestDiagSeqSurvivesVacuum (P58-8): a record's seq is fixed when it is
// written: a prune's VACUUM keeps it, and a later record gets a higher
// one even after the newest were deleted, so --follow's AfterRow holds.
func TestDiagSeqSurvivesVacuum(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), DiagFileName)
	d, err := OpenDiag(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		if err := d.Insert(ctx, diagRec(fmt.Sprintf("id%d", i), base.Add(time.Duration(i)*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	seqs := func() map[string]int64 {
		v, err := OpenDiagView(ctx, path, "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = v.Close() }()
		recs, err := v.Records(ctx, DiagFilter{})
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]int64{}
		for _, r := range recs {
			m[r.ID] = r.Row
		}
		return m
	}
	was := seqs()
	// The oldest three go, and the file is compacted.
	if n, err := d.Prune(ctx, base.Add(3*time.Hour), 0); err != nil || n != 3 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	now := seqs()
	if len(now) != 3 {
		t.Fatalf("left %v", now)
	}
	for id, seq := range now {
		if was[id] != seq {
			t.Errorf("%s: seq %d became %d", id, was[id], seq)
		}
	}
	// The newest goes too; the next record still follows it.
	if _, err := d.s.db.ExecContext(ctx, `DELETE FROM records WHERE id = 'id5'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.s.db.ExecContext(ctx, `VACUUM`); err != nil {
		t.Fatal(err)
	}
	if err := d.Insert(ctx, diagRec("id6", base.Add(6*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if got := seqs()["id6"]; got <= was["id5"] {
		t.Errorf("id6 seq %d, not after id5's %d", got, was["id5"])
	}
	v, err := OpenDiagView(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v.Close() }()
	recs, err := v.Records(ctx, DiagFilter{AfterRow: was["id4"]})
	if err != nil || len(recs) != 1 || recs[0].ID != "id6" {
		t.Errorf("after id4: %+v, %v", recs, err)
	}
}

// TestDiagVersion1 (P58-8): a diag.db still at version 1 (no aggy has
// written it since the upgrade) reads through the view by rowid, and its
// migration keeps each rowid as the record's seq, so a follow begun on
// version 1 goes on.
func TestDiagVersion1(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), DiagFileName)
	d, err := OpenDiag(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := migrationFS.ReadFile("diagmigrations/0001_records.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE records`, string(v1), `PRAGMA user_version = 1`} {
		if _, err := d.s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for i, rowid := range []int{5, 9, 12} {
		if _, err := d.s.db.ExecContext(ctx, `INSERT INTO records (rowid, id, schema, ts, level, component, event, run_id)
			VALUES (?, ?, 1, ?, 'info', 'gc', 'gc.item.removed', 'r')`, rowid, fmt.Sprintf("id%d", i), int64(1000+i)); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.Close()
	rows := func(after int64) []int64 {
		t.Helper()
		v, err := OpenDiagView(ctx, path, "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = v.Close() }()
		recs, err := v.Records(ctx, DiagFilter{AfterRow: after})
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, r := range recs {
			out = append(out, r.Row)
		}
		return out
	}
	if got := rows(0); fmt.Sprint(got) != "[5 9 12]" {
		t.Errorf("version 1: rows %v", got)
	}
	d, err = OpenDiag(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Insert(ctx, diagRec("id9", time.UnixMilli(2000))); err != nil {
		t.Fatal(err)
	}
	if got := rows(9); fmt.Sprint(got) != "[12 13]" {
		t.Errorf("version 2, after 9: rows %v", got)
	}
}
