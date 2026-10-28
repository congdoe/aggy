package cli

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/store"
)

// seedBorrows records two research borrows on grok, the second the same
// question asked again, and a failed news borrow on codex.
func seedBorrows(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	t0 := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	if err := s.InsertParent(ctx, store.Parent{ID: "p", Dir: "/repo", CreatedAt: t0, LastActive: t0}); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		id, kind, vendor, status, outcome string
		tokens                            int64
		start, secs                       int
	}{
		{"b1", "research", "grok", "done", "ok", 1000, 0, 90},
		{"b2", "research", "grok", "done", "ok", 3000, 600, 150},
		{"b3", "news", "codex", "failed", "rate_limit", 0, 1200, 5},
	}
	for _, r := range rows {
		start := t0.Add(time.Duration(r.start) * time.Second)
		l, err := s.StartBorrow(ctx,
			store.Child{ID: "c" + r.id, ParentID: "p", Vendor: r.vendor, Kind: r.kind, CreatedAt: start, LastActive: start},
			store.Borrow{ID: r.id, Kind: r.kind, Vendor: r.vendor, TaskHash: "h-" + r.kind, StartedAt: start}, store.BorrowCaps{})
		if err != nil {
			t.Fatal(err)
		}
		end := start.Add(time.Duration(r.secs) * time.Second)
		if err := l.Finish(ctx, store.BorrowEnd{Status: r.status, Outcome: r.outcome, Tokens: r.tokens, EndedAt: end}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestStats (T6700): aggy stats per kind and vendor, read only.
func TestStats(t *testing.T) {
	table := "KIND      VENDOR  RUNS  OK  AVG TOKENS  AVG TIME  REDONE\n" +
		"news      codex   1     0   -           -         -\n" +
		"research  grok    2     2   2000        2m0s      1 of 2 (50%)\n" +
		"\n" + statsNote
	tests := []struct {
		name       string
		store      string // "", "empty", or "seeded"
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no database", args: []string{"stats"}, wantStdout: "No borrows yet.\n"},
		{name: "empty store", store: "empty", args: []string{"stats"}, wantStdout: "No borrows yet.\n"},
		{name: "seeded", store: "seeded", args: []string{"stats"}, wantStdout: table},
		{name: "extra arg", args: []string{"stats", "x"}, wantCode: 2, wantStderr: "unexpected arguments"},
		{name: "bad flag", args: []string{"stats", "--json"}, wantCode: 2, wantStderr: "flag provided but not defined"},
		{name: "help", args: []string{"stats", "-h"}, wantStdout: statsUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			state := t.TempDir()
			t.Setenv(home.Env, state)
			db := filepath.Join(state, store.FileName)
			switch tt.store {
			case "empty":
				s, err := store.OpenDefault(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				_ = s.Close()
			case "seeded":
				seedBorrows(t)
			}
			var before []byte
			if tt.store != "" {
				var err error
				if before, err = os.ReadFile(db); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			code := Run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if tt.wantStdout != "" && stdout.String() != tt.wantStdout {
				t.Errorf("stdout =\n%s\nwant\n%s", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
			}
			after, err := os.ReadFile(db)
			switch {
			case tt.store == "" && !os.IsNotExist(err):
				t.Errorf("aggy stats created the database: %v", err)
			case tt.store != "" && !bytes.Equal(before, after):
				t.Error("aggy stats changed the database")
			}
		})
	}
}

// TestStatsSchema (L48-3): an index older than aggy stats reads, or newer
// than this binary knows, gets a message and is left byte for byte.
func TestStatsSchema(t *testing.T) {
	newer := func(t *testing.T, path string) {
		s, err := store.OpenDefault(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name       string
		make       func(t *testing.T, path string)
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"older", schemaV1, 0, "schema version 1, older than aggy stats reads", ""},
		{"newer", newer, 1, "", "schema version 999, newer than this aggy knows"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			state := t.TempDir()
			t.Setenv(home.Env, state)
			path := filepath.Join(state, store.FileName)
			tt.make(t, path)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := Run([]string{"stats"}, &stdout, &stderr); code != tt.wantCode {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) || !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Errorf("aggy stats changed the index (%v)", err)
			}
		})
	}
}
