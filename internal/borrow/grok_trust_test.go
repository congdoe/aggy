package borrow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestGrokTrustGate (C17-1): a Grok review is refused, and Grok passed
// over, when the repo holds a hook or other config Grok loads and runs
// once the folder is trusted. Instructions alone do not refuse it.
func TestGrokTrustGate(t *testing.T) {
	tests := []struct {
		name   string
		path   string // relative to the repo root; a trailing / makes a dir
		refuse bool
	}{
		{"nothing", "", false},
		{"AGENTS.md only", "AGENTS.md", false},
		{"claude settings hooks", ".claude/settings.json", true},
		{"claude local settings", ".claude/settings.local.json", true},
		{"grok hooks dir", ".grok/hooks/", true},
		{"cursor hooks", ".cursor/hooks.json", true},
		{"mcp.json", ".mcp.json", true},
		{"grok config", ".grok/config.toml", true},
		{"grok lsp", ".grok/lsp.json", true},
		{"cursor mcp", ".cursor/mcp.json", true},
		{"envrc", ".envrc", true},
		{"grok plugins", ".grok/plugins/", true},
		{"claude plugins", ".claude/plugins/", true},
		{"grok agents", ".grok/agents/", true},
		{"claude agents", ".claude/agents/", true},
		{"grok workflows", ".grok/workflows/", true},
		{"grok roles", ".grok/roles/", true},
		{"grok personas", ".grok/personas/", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			for _, v := range adapter.Vendors() {
				w.connected[v] = v == adapter.Grok || v == adapter.Claude
			}
			w.scenario[adapter.Grok] = `{"json":{"result":"grok reviewed"}}`
			w.scenario[adapter.Claude] = `{"json":{"type":"result","result":"claude reviewed"}}`
			if tt.path != "" {
				p := filepath.Join(w.repo, filepath.FromSlash(strings.TrimSuffix(tt.path, "/")))
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				var err error
				if strings.HasSuffix(tt.path, "/") {
					err = os.Mkdir(p, 0o700)
				} else {
					err = os.WriteFile(p, []byte(`{"hooks":{}}`), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			res, err := w.borrower().Run(context.Background(), w.req(adapter.Review, "review it"))
			if err != nil || len(res) != 1 {
				t.Fatalf("results %+v, %v", res, err)
			}
			if tt.refuse {
				if res[0].Vendor != adapter.Claude || len(res[0].Tried) != 1 || res[0].Tried[0].Vendor != adapter.Grok ||
					!strings.Contains(res[0].Tried[0].Reason, filepath.FromSlash(strings.TrimSuffix(tt.path, "/"))) {
					t.Errorf("want grok passed over for %s: %+v", tt.path, res[0])
				}
				for _, r := range w.records() {
					if r.Vendor == "grok" {
						t.Error("grok ran")
					}
				}
				return
			}
			if res[0].Vendor != adapter.Grok {
				t.Errorf("grok was passed over: %+v", res[0])
			}
		})
	}
}

// TestGrokProjectSourcesChain: sources in a dir between the cwd and the
// git root count; ones above the git root do not.
func TestGrokProjectSourcesChain(t *testing.T) {
	top := t.TempDir()
	repo := filepath.Join(top, "repo")
	sub := filepath.Join(repo, "a", "b")
	for _, d := range []string{filepath.Join(repo, ".git"), sub, filepath.Join(repo, "a", ".grok", "agents")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(top, ".mcp.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := grokProjectSources(sub)
	if len(got) != 1 || got[0] != filepath.Join("..", ".grok", "agents") {
		t.Errorf("sources = %q", got)
	}
}
