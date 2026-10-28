package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/store"
)

func seed(t *testing.T) {
	t.Helper()
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	base := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	events := []store.Event{
		{At: base, ParentID: "p1", Type: store.EventScreen, Detail: map[string]any{"vendor": "claude", "model": "opus"}},
		{At: base.Add(time.Minute), ParentID: "p1", ChildID: "c2", Type: store.EventBorrow,
			Detail: map[string]any{"kind": "image", "vendor": "codex", "outcome": "done"}},
		{At: base.Add(2 * time.Minute), ParentID: "p1", Type: store.EventFiles, Detail: map[string]any{"files": []string{"a.go", "b.go"}}},
		{At: base.Add(3 * time.Minute), ParentID: "p1", Type: store.EventSwitch,
			Detail: map[string]any{"from": "claude", "to": "grok", "model": "grok-4.7", "reason": "rate_limit"}},
		{At: base.Add(4 * time.Minute), ParentID: "p2", Type: store.EventScreen, Detail: map[string]any{"vendor": "codex"}},
	}
	for _, e := range events {
		if err := s.AppendEvent(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLog(t *testing.T) {
	header := "TIME (UTC)           SESSION  EVENT   DETAIL\n"
	all := header +
		"2026-10-27 09:00:00  p1       screen  claude (opus) on screen\n" +
		"2026-10-27 09:01:00  p1       borrow  image on codex: done\n" +
		"2026-10-27 09:02:00  p1       files   a.go, b.go\n" +
		"2026-10-27 09:03:00  p1       switch  claude -> grok (grok-4.7) [rate_limit]\n" +
		"2026-10-27 09:04:00  p2       screen  codex on screen\n"
	tests := []struct {
		name       string
		seeded     bool
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no database", args: []string{"log"}, wantStdout: "No events yet.\n"},
		{name: "empty store", seeded: false, args: []string{"log", "-n", "3"}, wantStdout: "No events yet.\n"},
		{name: "seeded", seeded: true, args: []string{"log"}, wantStdout: all},
		{name: "latest two", seeded: true, args: []string{"log", "-n", "2"}, wantStdout: header +
			"2026-10-27 09:03:00  p1       switch  claude -> grok (grok-4.7) [rate_limit]\n" +
			"2026-10-27 09:04:00  p2       screen  codex on screen\n"},
		{name: "one parent", seeded: true, args: []string{"log", "--parent", "p2"}, wantStdout: "TIME (UTC)           SESSION  EVENT   DETAIL\n" +
			"2026-10-27 09:04:00  p2       screen  codex on screen\n"},
		{name: "bad flag", args: []string{"log", "--bogus"}, wantCode: 2, wantStderr: "flag provided but not defined"},
		{name: "extra arg", args: []string{"log", "x"}, wantCode: 2, wantStderr: "unexpected arguments"},
		{name: "help", args: []string{"log", "-h"}, wantStdout: logUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			state := t.TempDir()
			t.Setenv(home.Env, state)
			if tt.name == "empty store" {
				s, err := store.OpenDefault(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				_ = s.Close()
			}
			if tt.seeded {
				seed(t)
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
			if tt.name == "no database" {
				if _, err := os.Stat(filepath.Join(state, store.FileName)); !os.IsNotExist(err) {
					t.Errorf("aggy log created the database: %v", err)
				}
			}
		})
	}
}

// TestDescribeBorrow (T6630): a borrow event shows its time and tokens.
func TestDescribeBorrow(t *testing.T) {
	tests := []struct {
		detail map[string]any
		want   string
	}{
		{map[string]any{"kind": "news", "vendor": "grok", "model": "grok-4.7", "outcome": "ok", "ms": float64(42400), "tokens": float64(150)},
			"news on grok (grok-4.7): ok (42s, 150 tokens)"},
		{map[string]any{"kind": "image", "vendor": "codex", "outcome": "timeout", "ms": float64(300000), "tokens": float64(0)},
			"image on codex: timeout (5m0s)"},
		{map[string]any{"kind": "review", "vendor": "claude"}, "review on claude"},
	}
	for _, tt := range tests {
		if got := describe(store.Event{Type: store.EventBorrow, Detail: tt.detail}); got != tt.want {
			t.Errorf("describe = %q, want %q", got, tt.want)
		}
	}
}
