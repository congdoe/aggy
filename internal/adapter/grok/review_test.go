package grok

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// Regression tests for the PR #5 review. Each fails on ed38103.

// TestSessionIDsCannotBeFlags (1, 7).
func TestSessionIDsCannotBeFlags(t *testing.T) {
	a := New()
	for _, id := range []string{"--dangerously-skip", "-x", "r-1"} {
		if _, err := a.Headless(adapter.HeadlessRequest{Kind: adapter.Review, PromptFile: "/p", Cwd: "/w", Resume: id}); err == nil {
			t.Errorf("headless resume %q accepted", id)
		}
		if _, err := a.Interactive(adapter.InteractiveRequest{Resume: id}); err == nil {
			t.Errorf("interactive resume %q accepted", id)
		}
	}
	other := "7c4f1e2a-3b5d-4a6c-9e8f-1a2b3c4d5e6f"
	res, err := a.ParseHeadless(adapter.HeadlessRequest{Kind: adapter.Image, SessionID: sid},
		adapter.Output{Stdout: []byte(`{"sessionId":"` + other + `"}`)})
	if err == nil || res.SessionID != sid {
		t.Errorf("other id = %+v, %v", res, err)
	}
	res, err = a.ParseHeadless(adapter.HeadlessRequest{Kind: adapter.Image}, adapter.Output{Stdout: []byte(`{"sessionId":"--x"}`)})
	if err == nil || res.SessionID != "" {
		t.Errorf("flag-shaped id = %+v, %v", res, err)
	}
}

// TestToolAllowlists (3, 5): research, news, and review take only their
// own tools, compared case-insensitively; edit has no shell; every run
// also passes the verified deny list.
func TestToolAllowlists(t *testing.T) {
	a := New()
	for _, tc := range []struct {
		kind  adapter.Kind
		tools []string
	}{
		{adapter.Research, []string{"mcp__filesystem__read_file"}},
		{adapter.News, []string{"bash"}},
		{adapter.Research, []string{"Run_Terminal_Cmd"}},
		{adapter.Research, []string{"Read_File"}},
		{adapter.Review, []string{"Write"}},
		{adapter.Review, []string{"bash"}},
		{adapter.Review, []string{"mcp__filesystem__write_file"}},
		{adapter.Edit, []string{"run_terminal_cmd"}},
	} {
		req := adapter.HeadlessRequest{Kind: tc.kind, PromptFile: "/p", Cwd: "/w", AllowedTools: tc.tools, IsolatedCwd: true}
		if c, err := a.Headless(req); err == nil {
			t.Errorf("%s with %v accepted: %q", tc.kind, tc.tools, c.Args)
		}
	}
	c, err := a.Headless(adapter.HeadlessRequest{Kind: adapter.Edit, PromptFile: "/p", Cwd: "/w"})
	if err != nil || strings.Contains(strings.Join(c.Args, " "), "--tools read_file,grep,list_dir,write,search_replace,run_terminal_cmd") {
		t.Errorf("edit = %q, %v", c.Args, err)
	}
	i := slices.Index(c.Args, "--disallowed-tools")
	if i < 0 || !strings.Contains(c.Args[i+1], "run_terminal_cmd") {
		t.Errorf("edit deny list = %q", c.Args)
	}
	c, err = a.Headless(adapter.HeadlessRequest{Kind: adapter.Review, PromptFile: "/p", Cwd: "/w", AllowedTools: []string{"GREP"}})
	if err != nil || !slices.Contains(c.Args, "read_file,grep,list_dir") {
		t.Errorf("review with GREP = %q, %v", c.Args, err)
	}
}

// TestWebSearchOnlyForResearch (5).
func TestWebSearchOnlyForResearch(t *testing.T) {
	for _, k := range []adapter.Kind{adapter.Review, adapter.Image, adapter.Video, adapter.Edit} {
		_, err := New().Headless(adapter.HeadlessRequest{Kind: k, PromptFile: "/p", Cwd: "/w", WebSearch: true})
		if !errors.Is(err, adapter.ErrUnsupported) {
			t.Errorf("%s with web search: %v", k, err)
		}
	}
	if _, err := New().Headless(adapter.HeadlessRequest{Kind: adapter.News, PromptFile: "/p", Cwd: "/w"}); err == nil {
		t.Error("news without IsolatedCwd accepted")
	}
}

// TestErrorClassFromStructuredFields (8): a successful reply whose text
// mentions rate_limit, and stderr on a clean exit, are not a limit.
func TestErrorClassFromStructuredFields(t *testing.T) {
	res, _ := New().ParseHeadless(adapter.HeadlessRequest{Kind: adapter.News},
		adapter.Output{Stdout: []byte(`{"sessionId":"` + sid + `","is_error":true,"result":"a post about rate_limit"}`), Stderr: []byte("note: retried after rate_limit\n")})
	if res.Error != adapter.ErrUnknown {
		t.Errorf("class = %q, want unknown", res.Error)
	}
}
