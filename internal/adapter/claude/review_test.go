package claude

import (
	"errors"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
)

// Regression tests for the PR #5 review. Each fails on ed38103.

// TestSessionIDsCannotBeFlags (1): a resume id that looks like a flag is
// refused, in Headless and Interactive.
func TestSessionIDsCannotBeFlags(t *testing.T) {
	a := New()
	for _, id := range []string{"--permission-mode=bypassPermissions", "-x", "abc def", "not-a-uuid", "0b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e;x"} {
		if c, err := a.Headless(adapter.HeadlessRequest{Kind: adapter.Review, PromptFile: "/p", Cwd: "/w", Resume: id}); err == nil {
			t.Errorf("headless resume %q accepted: %q", id, c.Args)
		}
		if c, err := a.Interactive(adapter.InteractiveRequest{Resume: id}); err == nil {
			t.Errorf("interactive resume %q accepted: %q", id, c.Args)
		}
	}
	if _, err := a.Headless(adapter.HeadlessRequest{Kind: adapter.Review, PromptFile: "/p", Cwd: "/w", Model: "--dangerously-skip-permissions"}); err == nil {
		t.Error("a flag-shaped model was accepted")
	}
}

// TestFakeResumeTakesAnOptionalValue (1): the fake, like Claude, does not
// take a following flag as the value of --resume.
func TestFakeResumeTakesAnOptionalValue(t *testing.T) {
	h := fakecli.NewHarness(t, fakecli.Build(t))
	c := adapter.Command{Path: Bin, Args: []string{"-p", "--resume", "--permission-mode", "plan"}, Dir: h.Cwd,
		Stdin: adapter.StdinFile, StdinPath: h.Prompt}
	out, rec := h.Run(t, c, "")
	if out.ExitCode != 0 || len(rec.Flags["--resume"]) != 1 || rec.Flags["--resume"][0] != "" ||
		len(rec.Flags["--permission-mode"]) != 1 || rec.Flags["--permission-mode"][0] != "plan" {
		t.Errorf("exit %d %q, flags %v", out.ExitCode, out.Stderr, rec.Flags)
	}
}

// TestParsedSessionIDs (7): a pre-assigned id is kept, and a reply naming
// another, or a flag-shaped one, is an error.
func TestParsedSessionIDs(t *testing.T) {
	a := New()
	req := adapter.HeadlessRequest{Kind: adapter.Review, SessionID: sid}
	res, err := a.ParseHeadless(req, adapter.Output{Stdout: []byte(`{"type":"result","result":"ok","session_id":"` + rid + `"}`)})
	if err == nil || res.SessionID != sid {
		t.Errorf("other id = %+v, %v; want the assigned id and an error", res, err)
	}
	res, err = a.ParseHeadless(adapter.HeadlessRequest{Kind: adapter.Review}, adapter.Output{Stdout: []byte(`{"type":"result","result":"ok","session_id":"--resume=x"}`)})
	if err == nil || res.SessionID != "" {
		t.Errorf("flag-shaped id = %+v, %v", res, err)
	}
}

// TestErrorClassFromStructuredFields (8): the model's text saying "hit
// your limit" does not make a max-turns failure a rate limit, and a
// successful reply that mentions it is a success.
func TestErrorClassFromStructuredFields(t *testing.T) {
	a := New()
	req := adapter.HeadlessRequest{Kind: adapter.Review, SessionID: sid}
	res, _ := a.ParseHeadless(req, adapter.Output{ExitCode: 1, Stdout: []byte(
		`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"they said they hit your limit"}`)})
	if res.Error != adapter.ErrMaxTurns {
		t.Errorf("class = %q, want max_turns", res.Error)
	}
	res, _ = a.ParseHeadless(req, adapter.Output{ExitCode: 1, Stdout: []byte(
		`{"type":"result","subtype":"success","is_error":false,"result":"you hit your limit, says the page"}`)})
	if res.Error != adapter.ErrUnknown {
		t.Errorf("class = %q, want unknown", res.Error)
	}
}

// TestWebSearchRefused (5): no web search for a kind that reads files.
func TestWebSearchRefused(t *testing.T) {
	for _, k := range []adapter.Kind{adapter.Review, adapter.Edit} {
		_, err := New().Headless(adapter.HeadlessRequest{Kind: k, PromptFile: "/p", Cwd: "/w", WebSearch: true})
		if !errors.Is(err, adapter.ErrUnsupported) {
			t.Errorf("%s: %v", k, err)
		}
	}
}
