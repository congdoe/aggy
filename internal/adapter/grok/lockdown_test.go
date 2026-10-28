package grok

import (
	"errors"
	"slices"
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
	a := sandboxed()
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
	a := sandboxed()
	for _, k := range []adapter.Kind{adapter.Research, adapter.News, adapter.Image, adapter.Video, adapter.Review, adapter.Edit} {
		c, err := a.Headless(adapter.HeadlessRequest{Kind: k, PromptFile: "/p", Cwd: "/w", IsolatedCwd: true, FileTools: k == adapter.Video})
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

// TestWebSearchOnlyForSearchKinds (#10-4): every kind but research and
// news denies web_search, and the deny list never names a tool Grok
// cannot remove by name.
func TestWebSearchOnlyForSearchKinds(t *testing.T) {
	for _, k := range []adapter.Kind{adapter.Research, adapter.News, adapter.Image, adapter.Video, adapter.Review, adapter.Edit} {
		c, err := sandboxed().Headless(adapter.HeadlessRequest{Kind: k, PromptFile: "/p", Cwd: "/w", IsolatedCwd: true, FileTools: k == adapter.Video})
		if err != nil {
			t.Fatal(err)
		}
		var tools, deny []string
		for i, a := range c.Args {
			switch a {
			case "--tools":
				tools = strings.Split(c.Args[i+1], ",")
			case "--disallowed-tools":
				deny = strings.Split(c.Args[i+1], ",")
			}
		}
		search := k == adapter.Research || k == adapter.News
		if slices.Contains(tools, "web_search") != search || slices.Contains(deny, "web_search") == search {
			t.Errorf("%s: tools %q deny %q", k, tools, deny)
		}
		if slices.Contains(deny, "open_page") || slices.Contains(deny, "open_page_with_find") {
			t.Errorf("%s denies a tool Grok cannot remove by name: %q", k, deny)
		}
	}
}
