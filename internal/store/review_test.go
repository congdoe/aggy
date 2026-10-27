package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/home"
)

// Regression tests for the PR #2 review.

// TestDSNOpensExactPath: SQLite percent-decodes the URI path and splits it
// at '?' and '#', so the opened file must be exactly the checked one.
func TestDSNOpensExactPath(t *testing.T) {
	for _, name := range []string{"a%6Eb", "q?x", "h#x", "sp ace", "pct%25"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			dir := filepath.Join(t.TempDir(), name)
			path := filepath.Join(dir, FileName)
			s, err := Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			var file string
			if err := s.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&file); err != nil {
				t.Fatal(err)
			}
			_ = s.Close()
			real, _ := filepath.EvalSymlinks(path)
			if file != path && file != real {
				t.Errorf("SQLite opened %q, want %q", file, path)
			}
			fi, err := os.Stat(path)
			if err != nil || fi.Size() == 0 {
				t.Errorf("database not written at the checked path: %v", err)
			}
			entries, _ := os.ReadDir(filepath.Dir(dir))
			if len(entries) != 1 {
				t.Errorf("files created outside %q: %v", dir, entries)
			}
		})
	}
}

// TestDSNCannotReachCredentialFile: $HOME/.codex/auth.jso%6E passed the
// path check and SQLite then wrote a database into auth.json.
func TestDSNCannotReachCredentialFile(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("CODEX_HOME", "")
	auth := filepath.Join(h, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(auth), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), filepath.Join(h, ".codex", "auth.jso%6E"))
	if err == nil {
		_ = s.Close()
	}
	fi, err := os.Stat(auth)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Fatalf("auth.json was written: %d bytes", fi.Size())
	}
}

// TestDSNAggyHomeWithQuery: AGGY_HOME=/x/state?x opened /x/state with
// journal_mode=delete.
func TestDSNAggyHomeWithQuery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := t.TempDir()
	t.Setenv(home.Env, filepath.Join(base, "state?x"))
	s, err := OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var mode string
	if err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	if _, err := os.Stat(filepath.Join(base, "state")); err == nil {
		t.Error("opened a database at the truncated path")
	}
}

// TestConcurrentFirstOpen: goroutines racing the first open of a fresh
// path hit SQLITE_BUSY during the WAL switch.
func TestConcurrentFirstOpen(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	var failures []string
	var mu sync.Mutex
	for round := range 20 {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("r%d", round), FileName)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				s, err := Open(ctx, path)
				if err == nil {
					err = s.Close()
				}
				if err != nil {
					mu.Lock()
					failures = append(failures, err.Error())
					mu.Unlock()
				}
			})
		}
		wg.Wait()
	}
	if len(failures) > 0 {
		t.Fatalf("%d of 160 first opens failed, e.g. %s", len(failures), failures[0])
	}
}

// TestEventsNoReplace: REPLACE deletes without firing delete triggers.
func TestEventsNoReplace(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO events (id, at, type) VALUES (1, 1, 'switch')`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT OR REPLACE INTO events (id, at, type) VALUES (1, 2, 'forged')`,
		`REPLACE INTO events (id, at, type) VALUES (1, 2, 'forged')`,
		`INSERT INTO events (id, at, type) VALUES (1, 2, 'forged') ON CONFLICT (id) DO UPDATE SET type = 'forged'`,
	} {
		if _, err := s.DB().ExecContext(ctx, q); err == nil {
			t.Errorf("%s: succeeded, want refusal", q)
		}
	}
	var typ string
	if err := s.DB().QueryRowContext(ctx, `SELECT type FROM events WHERE id = 1`).Scan(&typ); err != nil || typ != "switch" {
		t.Errorf("event 1 = %q, %v, want switch", typ, err)
	}
	// Ordinary appends still work.
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO events (at, type) VALUES (3, 'borrow')`); err != nil {
		t.Errorf("append: %v", err)
	}
}

// TestExistingDirModeKept: AGGY_HOME=$HOME must not chmod the home
// directory to 0700.
func TestExistingDirModeKept(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Errorf("existing dir mode = %o, want 0755 kept", got)
	}
}

// TestStatusCheck: status columns only take the documented values.
func TestStatusCheck(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertParent(ctx, Parent{ID: "p", Dir: "/x", CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"open", "running", "closed", "gone"} {
		if err := s.InsertChild(ctx, Child{ID: "c-" + st, ParentID: "p", Vendor: "grok", Cwd: "/x", Status: st, CreatedAt: now, LastActive: now}); err != nil {
			t.Errorf("child status %q: %v", st, err)
		}
	}
	err := s.InsertChild(ctx, Child{ID: "bad", ParentID: "p", Vendor: "grok", Cwd: "/x", Status: "zombie", CreatedAt: now, LastActive: now})
	if err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("child status zombie: err = %v, want CHECK failure", err)
	}
	for st, ok := range map[string]bool{"running": true, "done": true, "failed": true, "killed": true, "lost": false} {
		_, err := s.DB().ExecContext(ctx, `INSERT INTO borrows (id, kind, vendor, status, started_at) VALUES (?, 'news', 'grok', ?, 1)`, "b-"+st, st)
		if (err == nil) != ok {
			t.Errorf("borrow status %q: err = %v, want ok=%v", st, err, ok)
		}
	}
}
