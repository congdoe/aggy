package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/home"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "state", FileName)
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestFreshDB(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()

	v, err := s.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := migrations()
	if v != len(ms) || v < 1 {
		t.Errorf("user_version = %d, want %d", v, len(ms))
	}

	rows, err := s.DB().QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(tables)
	want := "borrows,changes,children,events,parents,probes,vendors"
	if got := strings.Join(tables, ","); got != want {
		t.Errorf("tables = %s, want %s", got, want)
	}

	for p, mode := range map[string]os.FileMode{filepath.Dir(path): 0o700, path: 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != mode {
			t.Errorf("%s mode = %o, want %o", filepath.Base(p), got, mode)
		}
	}
}

func TestMigrateIdempotent(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertParent(ctx, Parent{ID: "p1", Dir: "/x", CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("second Migrate: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening runs the migrations again, concurrently, without error and
	// without losing data.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			s2, err := Open(ctx, path)
			if err != nil {
				errs <- err
				return
			}
			errs <- s2.Close()
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent reopen: %v", err)
		}
	}

	s3, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s3.Close() }()
	if _, err := s3.GetParent(ctx, "p1"); err != nil {
		t.Errorf("GetParent after reopen: %v", err)
	}
}

func TestPragmas(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	// Pin several connections so each one is checked, not only the first.
	conns := make([]interface{ Close() error }, 0, 3)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for range 3 {
		c, err := s.DB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var mode string
		var busy, fk int
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" {
			t.Errorf("journal_mode = %q, want wal", mode)
		}
		if busy != int(busyTimeout.Milliseconds()) {
			t.Errorf("busy_timeout = %d", busy)
		}
		if fk != 1 {
			t.Errorf("foreign_keys = %d, want 1", fk)
		}
	}
}

func TestForeignKeys(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	err := s.InsertChild(ctx, Child{ID: "c1", ParentID: "missing", Vendor: "codex", Cwd: "/x", CreatedAt: now, LastActive: now})
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("child with no parent: err = %v, want FOREIGN KEY failure", err)
	}

	if err := s.InsertParent(ctx, Parent{ID: "p1", Dir: "/x", CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChild(ctx, Child{ID: "c1", ParentID: "p1", Vendor: "codex", Cwd: "/x", CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
	// Deleting the parent cascades to its children.
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM parents WHERE id = 'p1'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM children`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("children after parent delete = %d, want 0", n)
	}
}

func TestEventsAppendOnly(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO events (at, type) VALUES (1, 'switch')`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE events SET type = 'x'`, `DELETE FROM events`} {
		if _, err := s.DB().ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want append-only refusal", q, err)
		}
	}
}

func TestGetParent(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	now := time.UnixMilli(time.Now().UnixMilli()).UTC()
	want := Parent{ID: "p1", Title: "auth redirect", Dir: "/proj", LastVendor: "claude", CreatedAt: now, LastActive: now}
	if err := s.InsertParent(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetParent(ctx, "p1")
	if err != nil || got != want {
		t.Errorf("GetParent = %+v, %v, want %+v", got, err, want)
	}
	if _, err := s.GetParent(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetParent(nope) = %v, want ErrNotFound", err)
	}
}

func TestDefaultPath(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv(home.Env, "")
	if p, err := DefaultPath(); err != nil || p != filepath.Join(h, ".aggy", "aggy.db") {
		t.Errorf("DefaultPath = %q, %v", p, err)
	}
	override := t.TempDir()
	t.Setenv(home.Env, override)
	s, err := OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := os.Stat(filepath.Join(override, FileName)); err != nil {
		t.Errorf("OpenDefault did not use AGGY_HOME: %v", err)
	}
}
