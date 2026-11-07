//go:build unix

package borrow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestEditWarnsOnConfigChange: an edit child that leaves the user's Codex
// config changed gets a warning in the result and the report; one that does
// not gets none.
func TestEditWarnsOnConfigChange(t *testing.T) {
	for _, change := range []bool{false, true} {
		w := newWorld(t)
		home := filepath.Join(w.home, ".codex")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		cfg := filepath.Join(home, "config.toml")
		if err := os.WriteFile(cfg, []byte("a = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		w.scenario[adapter.Codex] = `{"extra_values":{"--sandbox":["workspace-write"]},"events":[{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}]}`
		b := w.borrower()
		real, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		b.Worktrees = linkedWorktrees{link: real}
		run := b.Runner
		b.Runner = func(d time.Duration) adapter.Runner {
			return func(c context.Context, cmd adapter.Command) (adapter.Output, error) {
				out, err := run(d)(c, cmd)
				if change {
					_ = os.WriteFile(cfg, []byte("a = 2\n"), 0o600)
				}
				return out, err
			}
		}
		req := w.req(adapter.Edit, "x")
		req.Vendor = "codex"
		res, err := b.Run(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(res[0].Warnings) == 1; got != change {
			t.Errorf("change %v: warnings %q", change, res[0].Warnings)
		}
		report, err := os.ReadFile(res[0].ReportPath)
		if err != nil || strings.Contains(string(report), "- Warning: ") != change {
			t.Errorf("change %v: report %v\n%s", change, err, report)
		}
	}
}
