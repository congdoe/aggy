package claude

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/probe"
)

// TestLockdown (T6621): every headless run needs a run dir holding the
// empty MCP config, and a CLI that rejects a lockdown flag fails the
// borrow closed.
func TestLockdown(t *testing.T) {
	a := Adapter{Caps: []string{probe.CapSettingSources}}
	files, err := a.ChildFiles(adapter.HeadlessRequest{Kind: adapter.Review})
	if err != nil || len(files) != 1 || files[0].Name != MCPConfigFile {
		t.Fatalf("ChildFiles = %v, %v", files, err)
	}
	var cfg map[string]map[string]any
	if err := json.Unmarshal(files[0].Data, &cfg); err != nil || len(cfg) != 1 || cfg["mcpServers"] == nil || len(cfg["mcpServers"]) != 0 {
		t.Errorf("MCP config %s holds servers or is not JSON: %v", files[0].Data, err)
	}
	if _, err := a.Headless(adapter.HeadlessRequest{Kind: adapter.Review, PromptFile: "/p", Cwd: "/w"}); err == nil {
		t.Error("a review without a run dir was built")
	}

	h := fakecli.NewHarness(t, fakecli.Build(t))
	tests := []struct {
		name     string
		scenario string
		wantErr  error
		class    adapter.ErrorClass
	}{
		// A CLI without --strict-mcp-config: the fake refuses it as an
		// unknown option, exit 2, as commander does.
		{"rejected flag", `{"help":"x"}`, adapter.ErrLockdownRejected, adapter.ErrLockdown},
		{"accepted", `{"json":{"type":"result","result":"ok"}}`, nil, adapter.ErrNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := adapter.HeadlessRequest{Kind: adapter.Review, PromptFile: h.Prompt, Cwd: h.Cwd, RunDir: h.RunDir}
			c, err := a.Headless(req)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != nil {
				// Stand in for an older CLI: an option it does not know.
				c.Args = append(c.Args, "--no-such-lockdown-flag")
			}
			out, _ := h.Run(t, c, tt.scenario)
			res, err := a.ParseHeadless(req, out)
			if !errors.Is(err, tt.wantErr) || res.Error != tt.class {
				t.Errorf("ParseHeadless = %+v, %v", res, err)
			}
		})
	}
}
