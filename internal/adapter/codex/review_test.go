package codex

import (
	"errors"
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/probe"
)

// Regression tests for the PR #5 review. Each fails on ed38103.

// TestSessionIDsCannotBeFlags (1): resume ids that are flags are refused,
// and so is a flag-shaped thread id reported by the child, which would
// otherwise flow into the next run's argv.
func TestSessionIDsCannotBeFlags(t *testing.T) {
	a := New()
	for _, id := range []string{"--dangerously-bypass-approvals-and-sandbox", "--last", `-cmcp_servers.x.command="sh"`, "t-1"} {
		if c, err := a.Interactive(adapter.InteractiveRequest{Resume: id}); err == nil {
			t.Errorf("interactive resume %q accepted: %q", id, c.Args)
		}
		if c, err := a.Headless(adapter.HeadlessRequest{Kind: adapter.Edit, PromptFile: "/p", Cwd: "/w", Resume: id}); err == nil {
			t.Errorf("headless resume %q accepted: %q", id, c.Args)
		}
	}
	out := adapter.Output{Stdout: []byte(`{"type":"thread.started","thread_id":"-cmcp_servers.x.command=\"sh\""}` + "\n")}
	res, err := a.ParseHeadless(adapter.HeadlessRequest{Kind: adapter.Image}, out)
	if err == nil || res.SessionID != "" {
		t.Errorf("flag-shaped thread id = %+v, %v", res, err)
	}
	if _, err := a.Artifacts(adapter.HeadlessRequest{}, adapter.HeadlessResult{SessionID: "--x"}); err == nil {
		t.Error("Artifacts accepted a flag-shaped id")
	}
}

// TestFakeExecResumeOwnFlags (4): like codex, the fake's exec resume takes
// only its own flags, and -i takes several values, so "-" after it is an
// image, not the prompt marker.
func TestFakeExecResumeOwnFlags(t *testing.T) {
	h := fakecli.NewHarness(t, fakecli.Build(t))
	run := func(args ...string) (adapter.Output, fakecli.Record) {
		return h.Run(t, adapter.Command{Path: Bin, Args: args, Dir: h.Cwd, Stdin: adapter.StdinFile, StdinPath: h.Prompt}, "")
	}
	if out, _ := run("exec", "resume", tid, "--json", "--sandbox", "read-only", "-"); out.ExitCode != 2 {
		t.Errorf("exec resume with exec flags: exit %d", out.ExitCode)
	}
	if out, rec := run("exec", "-i", "a.png", "-"); out.ExitCode != 0 || !slices.Equal(rec.Flags["-i"], []string{"a.png", "-"}) {
		t.Errorf("-i a.png - : exit %d, -i %q", out.ExitCode, rec.Flags["-i"])
	}
	if out, rec := run("exec", "--json", "--sandbox", "read-only", "resume", tid, "--", "-"); out.ExitCode != 0 || rec.SessionID != tid {
		t.Errorf("exec flags before resume: exit %d %q, session %q", out.ExitCode, out.Stderr, rec.SessionID)
	}
}

// TestResearchIsolation (6): research needs an isolated cwd, and passes
// --ignore-user-config when the probe found it.
func TestResearchIsolation(t *testing.T) {
	req := adapter.HeadlessRequest{Kind: adapter.Research, PromptFile: "/p", Cwd: "/w"}
	if _, err := New().Headless(req); err == nil {
		t.Error("research without IsolatedCwd accepted")
	}
	req.IsolatedCwd = true
	c, err := New().Headless(req)
	if err != nil || slices.Contains(c.Args, IgnoreUserConfig) {
		t.Errorf("without the capability: %q, %v", c.Args, err)
	}
	c, err = Adapter{Caps: []string{probe.CapIgnoreUserConfig}}.Headless(req)
	if err != nil || !slices.Contains(c.Args, IgnoreUserConfig) {
		t.Errorf("with the capability: %q, %v", c.Args, err)
	}
	h := fakecli.NewHarness(t, fakecli.Build(t))
	req.PromptFile, req.Cwd = h.Prompt, h.Cwd
	c, _ = Adapter{Caps: []string{probe.CapIgnoreUserConfig}}.Headless(req)
	if out, _ := h.Run(t, c, `{"extra_switches":["--ignore-user-config"]}`); out.ExitCode != 0 {
		t.Errorf("fake: exit %d %q", out.ExitCode, out.Stderr)
	}
}

// TestWebSearchOnlyForResearch (5).
func TestWebSearchOnlyForResearch(t *testing.T) {
	for _, k := range []adapter.Kind{adapter.Review, adapter.Image, adapter.Edit} {
		_, err := New().Headless(adapter.HeadlessRequest{Kind: k, PromptFile: "/p", Cwd: "/w", WebSearch: true})
		if !errors.Is(err, adapter.ErrUnsupported) {
			t.Errorf("%s with web search: %v", k, err)
		}
	}
}

// TestErrorThenTurnCompleted (8): an error event a completed turn follows
// is a success with a warning.
func TestErrorThenTurnCompleted(t *testing.T) {
	out := adapter.Output{Stdout: []byte(`{"type":"thread.started","thread_id":"` + tid + `"}
{"type":"error","message":"reconnecting"}
{"type":"item.completed","item":{"type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}
`)}
	res, err := New().ParseHeadless(adapter.HeadlessRequest{Kind: adapter.Review}, out)
	if err != nil || res.Error != adapter.ErrNone || res.Text != "done" || res.ErrorDetail == "" {
		t.Errorf("ParseHeadless = %+v, %v", res, err)
	}
}
