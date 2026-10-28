package codex

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
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

// TestResearchUnsupported (T6621): research and news are refused on
// Codex with the reason, since apply_patch cannot be removed.
func TestResearchUnsupported(t *testing.T) {
	for _, k := range []adapter.Kind{adapter.Research, adapter.News} {
		_, err := New().Headless(adapter.HeadlessRequest{Kind: k, PromptFile: "/p", Cwd: "/w", IsolatedCwd: true, WebSearch: true})
		if !errors.Is(err, adapter.ErrUnsupported) || !strings.Contains(err.Error(), "apply_patch") {
			t.Errorf("%s: %v", k, err)
		}
	}
}

// TestLockdownRejected (T6621): a CLI that rejects a lockdown flag fails
// the borrow closed, whatever else it printed.
func TestLockdownRejected(t *testing.T) {
	out := adapter.Output{ExitCode: 2, Stderr: []byte("error: unexpected argument '--ignore-user-config' found\n")}
	res, err := New().ParseHeadless(adapter.HeadlessRequest{Kind: adapter.Review}, out)
	if !errors.Is(err, adapter.ErrLockdownRejected) || res.Error != adapter.ErrLockdown {
		t.Errorf("ParseHeadless = %+v, %v", res, err)
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

// TestRefImageCommas (second review, N2): -i splits on commas, so a path
// holding one is refused, and the fake splits as codex does.
func TestRefImageCommas(t *testing.T) {
	if _, err := New().Headless(adapter.HeadlessRequest{Kind: adapter.Image, PromptFile: "/p", Cwd: "/w", RefImages: []string{"/b,c.png"}}); err == nil {
		t.Error("a reference path with a comma was accepted")
	}
	h := fakecli.NewHarness(t, fakecli.Build(t))
	out, rec := h.Run(t, adapter.Command{Path: Bin, Args: []string{"exec", "-i", "/b,c.png", "--json", "--", "-"}, Dir: h.Cwd,
		Stdin: adapter.StdinFile, StdinPath: h.Prompt}, "")
	if out.ExitCode != 0 || !slices.Equal(rec.Flags["-i"], []string{"/b", "c.png"}) {
		t.Errorf("fake -i = %q (exit %d)", rec.Flags["-i"], out.ExitCode)
	}
}

// TestStrictConfig (#10-5): a fake Codex that, like the real one,
// ignores an unknown --disable feature still fails the borrow, because
// the lockdown passes --strict-config.
func TestStrictConfig(t *testing.T) {
	h := fakecli.NewHarness(t, fakecli.Build(t))
	req := adapter.HeadlessRequest{Kind: adapter.Review, PromptFile: h.Prompt, Cwd: h.Cwd, GitRepo: true}
	c, err := New().Headless(req)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Args, "--strict-config") {
		t.Fatalf("no --strict-config: %q", c.Args)
	}
	// A feature this Codex no longer knows, say a renamed shell_tool.
	renamed := func(args []string) []string {
		out := slices.Clone(args)
		for i := range out {
			if out[i] == "shell_tool" {
				out[i] = "shell_tool_v1"
			}
		}
		return out
	}
	strict := c
	strict.Args = renamed(c.Args)
	out, _ := h.Run(t, strict, "")
	res, err := New().ParseHeadless(req, out)
	if !errors.Is(err, adapter.ErrLockdownRejected) || res.Error != adapter.ErrLockdown {
		t.Errorf("strict: %+v, %v (stderr %q)", res, err, out.Stderr)
	}
	// Without --strict-config the same run would pass silently.
	lax := c
	lax.Args = slices.DeleteFunc(renamed(c.Args), func(a string) bool { return a == "--strict-config" })
	if out, _ := h.Run(t, lax, ""); out.ExitCode != 0 {
		t.Errorf("lax run failed: %s", out.Stderr)
	}
}
