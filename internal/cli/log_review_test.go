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

	_ "modernc.org/sqlite"
)

// Regression tests for the PR #4 review (C10, C11). Each fails on 522a431.

// schemaV1 builds a database at schema version 1 with one event, the way
// an older aggy left it.
func schemaV1(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	b, err := os.ReadFile(filepath.Join("..", "store", "migrations", "0001_init.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{string(b), `PRAGMA user_version = 1`,
		`INSERT INTO events (at, parent_id, type, detail) VALUES (0, 'p1', 'screen', '{"vendor":"codex"}')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestLogDoesNotMigrate (C10): aggy log reads an old index as it is.
func TestLogDoesNotMigrate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	state := t.TempDir()
	t.Setenv(home.Env, state)
	path := filepath.Join(state, store.FileName)
	schemaV1(t, path)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"log"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "codex on screen") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if v := userVersion(t, path); v != 1 {
		t.Errorf("aggy log migrated the index to version %d", v)
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Errorf("aggy log took the setup lock: %v", err)
	}
}

// TestLogOnUnsetUpIndex (C10): an index with no schema gets a message,
// not an upgrade.
func TestLogOnUnsetUpIndex(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	state := t.TempDir()
	t.Setenv(home.Env, state)
	path := filepath.Join(state, store.FileName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"log"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "schema version 0") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if v := userVersion(t, path); v != 0 {
		t.Errorf("aggy log migrated the index to version %d", v)
	}
}

// TestLogEscapesControls (C11): text from vendor sessions cannot drive
// the terminal.
func TestLogEscapesControls(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(home.Env, t.TempDir())
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = s.AppendEvent(context.Background(), store.Event{
		At: time.Unix(0, 0), ParentID: "p\x1b]0;pwned\x07", Type: "x\u202e",
		Detail: map[string]any{"text": "a\x1b[2Jb\rc\nd\u0085e\x7f\tf"},
	})
	_ = s.Close()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"log"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, raw := range []string{"\x1b", "\x07", "\r", "\u0085", "\x7f", "\u202e"} {
		if strings.Contains(out, raw) {
			t.Errorf("output holds raw %q: %q", raw, out)
		}
	}
	for _, esc := range []string{`p\x1b]0;pwned\x07`, `x\u202e`, `a\x1b[2Jb\rc\nd\u0085e\x7f`} {
		if !strings.Contains(out, esc) {
			t.Errorf("output lacks %s: %q", esc, out)
		}
	}
	if strings.Count(out, "\n") != 2 {
		t.Errorf("an event spans more than one line: %q", out)
	}
}
