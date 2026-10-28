package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/store"
)

// TestUsage (T6698): Codex's allowance from its rollout (the session's
// quotas only, no credits), consumption from aggy's own borrows for every
// vendor, and a signed-out vendor left out.
func TestUsage(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		seeded   bool
		rollout  bool
		want     []string
		wantNot  []string
		wantCode int
	}{
		{name: "no database, no rollout", want: []string{
			"  claude  not written locally; aggy cannot see what is left\n",
			"  codex   nothing written in the last 8 days\n",
			"  claude  0        0       0s\n",
			"  grok    0        0       0s\n",
		}},
		{name: "seeded", seeded: true, rollout: true, want: []string{
			"Remaining allowance, as each vendor's CLI wrote it on this machine:\n",
			"  claude  not written locally; aggy cannot see what is left\n",
			"  codex   ordinary quota, 5h window: 70% left, resets " + now.Add(2*time.Hour).Local().Format("2006-01-02 15:04"),
			"  codex   ordinary quota, 7d window: reset at ",
			"; what is left now is unknown, as of ",
			"Not the vendor's count, bill, or allowance",
			"  claude  2        100 (1 of 2 borrows reported tokens)  3m0s\n",
			"  codex   0        0                                     0s\n",
		}, wantNot: []string{"grok", "spark", "12.50", "$", "7d window: 0%"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			state := t.TempDir()
			t.Setenv(home.Env, state)
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			if tt.rollout {
				day := filepath.Join(codexHome, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
				if err := os.MkdirAll(day, 0o700); err != nil {
					t.Fatal(err)
				}
				at := now.UTC().Format(time.RFC3339Nano)
				body := fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.5"}}`+"\n", at) +
					fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":30,"window_minutes":300,"resets_at":%d},"secondary":{"used_percent":100,"window_minutes":10080,"resets_at":%d},"credits":{"has_credits":true,"unlimited":false,"balance":"12.50"}}}}`+"\n",
						at, now.Add(2*time.Hour).Unix(), now.Add(-time.Hour).Unix()) +
					fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex_bengalfox","limit_name":"gpt-5.3-codex-spark","primary":{"used_percent":100,"window_minutes":300,"resets_at":%d}}}}`+"\n",
						at, now.Add(time.Hour).Unix())
				if err := os.WriteFile(filepath.Join(day, "rollout-2026-10-28T12-00-00-0199a213-81c0-7800-8aa1-bbab2a035a53.jsonl"), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.seeded {
				seedUsage(t, now)
			}
			var stdout, stderr bytes.Buffer
			if code := Run([]string{"usage"}, &stdout, &stderr); code != tt.wantCode {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			out := stdout.String()
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(out, w) {
					t.Errorf("output has %q:\n%s", w, out)
				}
			}
			if !tt.seeded {
				if _, err := os.Stat(filepath.Join(state, store.FileName)); !os.IsNotExist(err) {
					t.Errorf("aggy usage created the database: %v", err)
				}
			}
		})
	}
}

// seedUsage records grok as signed out and two finished claude borrows.
func seedUsage(t *testing.T, now time.Time) {
	t.Helper()
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.PutVendorStatus(ctx, store.VendorStatus{Vendor: "grok", Connected: "not_connected", CheckedAt: now}); err != nil {
		t.Fatal(err)
	}
	p, err := s.ParentForDir(ctx, "/repo", "usage", "p1", now)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range []struct {
		took   time.Duration
		tokens int64
	}{{2 * time.Minute, 100}, {time.Minute, 0}} {
		id := fmt.Sprintf("b%d", i)
		start := now.Add(-time.Hour)
		l, err := s.StartBorrow(ctx,
			store.Child{ID: "c-" + id, ParentID: p.ID, Vendor: "claude", Kind: "news", Title: "t", Cwd: "/tmp/w", CreatedAt: start, LastActive: start},
			store.Borrow{ID: id, Kind: "news", Vendor: "claude", StartedAt: start}, store.BorrowCaps{})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Finish(ctx, store.BorrowEnd{Status: "done", Outcome: "ok", Tokens: b.tokens, EndedAt: start.Add(b.took)}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUsageArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv(home.Env, t.TempDir())
	for _, args := range [][]string{{"usage", "x"}, {"usage", "--bogus"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, code)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"usage", "-h"}, &stdout, &stderr); code != 0 || stdout.String() != usageUsage {
		t.Errorf("help: exit %d, %q", code, stdout.String())
	}
}

func TestWindowLabel(t *testing.T) {
	for _, c := range []struct {
		m    int64
		want string
	}{{300, "5h"}, {10080, "7d"}, {90, "90m"}, {0, "unnamed"}} {
		m, want := c.m, c.want
		if got := windowLabel(m); got != want {
			t.Errorf("windowLabel(%d) = %q, want %q", m, got, want)
		}
	}
}

// TestShareLeft (L49-3): a window not used up never reads 0%, and the
// share is never rounded up.
func TestShareLeft(t *testing.T) {
	for _, c := range []struct {
		used float64
		want string
	}{
		{30, "70% left"}, {99.6, "0.4% left"}, {99.96, "under 0.1% left"}, {100, "0% left"},
		{120, "0% left"}, {-5, "100% left"}, {0.05, "99.9% left"}, {42.25, "57.7% left"},
	} {
		if got := shareLeft(c.used); got != c.want {
			t.Errorf("shareLeft(%v) = %q, want %q", c.used, got, c.want)
		}
	}
}

// TestUsageLeavesIndex (L49-4): aggy usage leaves a seeded index
// byte-identical, and reads one older than the vendors table (schema 1)
// as it is, listing every vendor.
func TestUsageLeavesIndex(t *testing.T) {
	for _, old := range []bool{false, true} {
		t.Run(fmt.Sprintf("old=%v", old), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			t.Setenv("CODEX_HOME", t.TempDir())
			state := t.TempDir()
			t.Setenv(home.Env, state)
			path := filepath.Join(state, store.FileName)
			if old {
				schemaV1(t, path)
			} else {
				seedUsage(t, time.Now())
			}
			snapshot := func() map[string]string {
				out := map[string]string{}
				es, err := os.ReadDir(state)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range es {
					if b, err := os.ReadFile(filepath.Join(state, e.Name())); err == nil {
						out[e.Name()] = string(b)
					}
				}
				return out
			}
			before := snapshot()
			var stdout, stderr bytes.Buffer
			if code := Run([]string{"usage"}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			after := snapshot()
			if len(before) != len(after) {
				t.Errorf("files in the state folder changed: %d before, %d after", len(before), len(after))
			}
			for name, b := range before {
				if after[name] != b {
					t.Errorf("aggy usage changed %s", name)
				}
			}
			if old {
				if v := userVersion(t, path); v != 1 {
					t.Errorf("aggy usage migrated the index to version %d", v)
				}
				for _, v := range []string{"claude", "codex", "grok"} {
					if !strings.Contains(stdout.String(), "  "+v+" ") {
						t.Errorf("an old index hides %s:\n%s", v, stdout.String())
					}
				}
			}
		})
	}
}
