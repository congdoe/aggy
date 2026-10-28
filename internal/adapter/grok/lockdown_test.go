package grok

import (
	"errors"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
)

// TestUnknownToolFailsClosed (T6621): Grok ignores the whole --tools
// allowlist when one name is unknown and only warns "name no tool" on
// stderr, with a successful exit. The borrow must fail, not succeed with
// every tool on.
func TestUnknownToolFailsClosed(t *testing.T) {
	h := fakecli.NewHarness(t, fakecli.Build(t))
	a := New()
	req := adapter.HeadlessRequest{Kind: adapter.News, PromptFile: h.Prompt, Cwd: h.Cwd, IsolatedCwd: true}
	c, err := a.Headless(req)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		mutate  func([]string) []string
		wantErr error
		class   adapter.ErrorClass
	}{
		{"exact names", func(a []string) []string { return a }, nil, adapter.ErrNone},
		{"an unknown name", func(a []string) []string {
			for i := range a {
				if a[i] == "--tools" {
					a[i+1] += ",x_keyword_search"
				}
			}
			return a
		}, adapter.ErrLockdownRejected, adapter.ErrLockdown},
		{"a rejected flag", func(a []string) []string { return append(a, "--no-such-flag") }, adapter.ErrLockdownRejected, adapter.ErrLockdown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := c
			cmd.Args = tt.mutate(append([]string{}, c.Args...))
			out, _ := h.Run(t, cmd, `{"json":{"result":"news"}}`)
			res, err := a.ParseHeadless(req, out)
			if !errors.Is(err, tt.wantErr) || res.Error != tt.class {
				t.Fatalf("ParseHeadless = %+v, %v (stderr %q)", res, err, out.Stderr)
			}
			if tt.wantErr != nil && tt.name == "an unknown name" && !strings.Contains(err.Error(), "ignored the --tools allowlist") {
				t.Errorf("error does not say why: %v", err)
			}
		})
	}
}

// TestMetaToolsAlwaysDenied: search_tool and use_tool, the MCP
// meta-tools, are denied for every kind.
func TestMetaToolsAlwaysDenied(t *testing.T) {
	a := New()
	for _, k := range []adapter.Kind{adapter.Research, adapter.News, adapter.Image, adapter.Video, adapter.Review, adapter.Edit} {
		c, err := a.Headless(adapter.HeadlessRequest{Kind: k, PromptFile: "/p", Cwd: "/w", IsolatedCwd: true})
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Join(c.Args, " ")
		deny := c.Args[len(c.Args)-2]
		if !strings.Contains(args, "--no-subagents") || !strings.Contains(deny, "search_tool") || !strings.Contains(deny, "use_tool") || !strings.Contains(deny, "Agent") {
			t.Errorf("%s: %q", k, c.Args)
		}
	}
}
