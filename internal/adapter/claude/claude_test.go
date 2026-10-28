package claude

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
)

const (
	sid = "0b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e"
	rid = "7c4f1e2a-3b5d-4a6c-9e8f-1a2b3c4d5e6f"
)

// scenario reads testdata/<name>.json, filling {hook} with the fake hook.
func scenario(t *testing.T, h *fakecli.Harness, name string) string {
	t.Helper()
	b, err := safefs.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "{hook}", h.Fakes.Hook)
}

func TestHeadlessGoldenArgv(t *testing.T) {
	h := fakecli.NewHarness(t, fakecli.Build(t))
	a := Adapter{Caps: []string{probe.CapSettingSources}}
	mcp := filepath.Join(h.RunDir, MCPConfigFile)
	lock := []string{"--strict-mcp-config", "--mcp-config", mcp}
	review := append([]string{"-p", "--output-format", "json", "--permission-mode", "plan"}, append(lock, "--tools", "Read,Grep,Glob", "--setting-sources", "user")...)
	cat := func(parts ...[]string) []string { return slices.Concat(parts...) }
	tests := []struct {
		name     string
		req      adapter.HeadlessRequest
		want     []string
		scenario string
	}{
		{"review", adapter.HeadlessRequest{Kind: adapter.Review}, review, ""},
		{"edit", adapter.HeadlessRequest{Kind: adapter.Edit}, cat([]string{"-p", "--output-format", "json", "--permission-mode", "acceptEdits"}, lock),
			`{"extra_values":{"--permission-mode":["acceptEdits"]}}`},
		{"review with a session id", adapter.HeadlessRequest{Kind: adapter.Review, SessionID: sid},
			cat(review, []string{"--session-id", sid}), ""},
		{"review resumed with a model", adapter.HeadlessRequest{Kind: adapter.Review, Resume: rid, Model: "opus"},
			cat([]string{"-p", "--output-format", "json", "--model", "opus", "--permission-mode", "plan"}, lock,
				[]string{"--tools", "Read,Grep,Glob", "--setting-sources", "user", "--resume", rid}),
			`{"extra_flags":["--model"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.PromptFile, tt.req.Cwd, tt.req.RunDir = h.Prompt, h.Cwd, h.RunDir
			c, err := a.Headless(tt.req)
			if err != nil {
				t.Fatal(err)
			}
			if c.Path != Bin || !slices.Equal(c.Args, tt.want) {
				t.Errorf("argv = %s %v, want %v", c.Path, c.Args, tt.want)
			}
			if c.Stdin != adapter.StdinFile || c.StdinPath != h.Prompt || c.Dir != h.Cwd {
				t.Errorf("stdin %v %q dir %q", c.Stdin, c.StdinPath, c.Dir)
			}
			out, rec := h.Run(t, c, tt.scenario)
			if out.ExitCode != 0 || rec.Mode != "headless" || rec.Stdin != "the task\n" || rec.Cwd != h.Cwd {
				t.Errorf("fake: exit %d %q, record %+v", out.ExitCode, out.Stderr, rec)
			}
		})
	}
}

func TestHeadlessUnsupported(t *testing.T) {
	a := New()
	base := adapter.HeadlessRequest{PromptFile: "/p/prompt.md", Cwd: "/w", RunDir: "/r"}
	for _, mod := range []func(*adapter.HeadlessRequest){
		func(r *adapter.HeadlessRequest) { r.Kind = adapter.Image },
		func(r *adapter.HeadlessRequest) { r.Kind = adapter.Video },
		func(r *adapter.HeadlessRequest) { r.Kind = adapter.Research },
		func(r *adapter.HeadlessRequest) { r.Kind = adapter.News },
		func(r *adapter.HeadlessRequest) { r.Kind, r.AllowedTools = adapter.Review, []string{} },
		func(r *adapter.HeadlessRequest) { r.Kind, r.WebSearch = adapter.Review, true },
		func(r *adapter.HeadlessRequest) { r.Kind, r.RefImages = adapter.Edit, []string{"/x.png"} },
	} {
		r := base
		mod(&r)
		if _, err := a.Headless(r); !errors.Is(err, adapter.ErrUnsupported) {
			t.Errorf("%+v: err = %v, want ErrUnsupported", r, err)
		}
	}
	for _, r := range []adapter.HeadlessRequest{
		{Kind: adapter.Review, PromptFile: "prompt.md", Cwd: "/w"},
		{Kind: adapter.Review, PromptFile: "/p", Cwd: ""},
		{Kind: adapter.Review, PromptFile: "/p", Cwd: "/w", SessionID: "not-a-uuid"},
		{Kind: "chat", PromptFile: "/p", Cwd: "/w"},
	} {
		if _, err := a.Headless(r); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
}

func TestInteractiveGoldenArgv(t *testing.T) {
	h := fakecli.NewHarness(t, fakecli.Build(t))
	brief := filepath.Join(h.Cwd, "brief.md")
	a := New()
	tests := []struct {
		name string
		req  adapter.InteractiveRequest
		want []string
	}{
		{"new", adapter.InteractiveRequest{}, nil},
		{"new with an id", adapter.InteractiveRequest{SessionID: sid}, []string{"--session-id", sid}},
		{"resume", adapter.InteractiveRequest{Resume: rid}, []string{"--resume", rid}},
		{"fork with a brief", adapter.InteractiveRequest{Resume: rid, Fork: true, SessionID: sid, BriefFile: brief},
			[]string{"--resume", rid, "--fork-session", "--session-id", sid, adapter.BriefPrompt(brief)}},
		{"resume with memory and a brief", adapter.InteractiveRequest{Resume: rid, MemoryFile: brief + ".mem", BriefFile: brief},
			[]string{"--resume", rid, "--append-system-prompt-file", brief + ".mem", adapter.BriefPrompt(brief)}},
		{"policy", adapter.InteractiveRequest{Deny: []string{"Read(**/.env)", "Bash(git push * main)"}},
			[]string{"--settings", `{"permissions":{"deny":["Read(**/.env)","Bash(git push * main)"]}}`}},
		// T6708: the tools' hooks share the policy's settings layer.
		{"policy and hooks", adapter.InteractiveRequest{Deny: []string{"Read(**/.env)"}, Hooks: map[string][]adapter.HookGroup{
			"PreToolUse": {{Matcher: "Bash", Hooks: []adapter.HookCommand{{Type: "command", Command: "x", Timeout: 10}}}}}},
			[]string{"--settings", `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"x","timeout":10}]}]},"permissions":{"deny":["Read(**/.env)"]}}`}},
		// T6708: additive, never --strict-mcp-config; re-passed on resume.
		{"tools on resume", adapter.InteractiveRequest{Resume: rid, MCPConfigFile: brief + ".mcp.json"},
			[]string{"--mcp-config", brief + ".mcp.json", "--resume", rid}},
		// W71-1: the variadic --mcp-config never takes the brief prompt.
		{"tools with a brief", adapter.InteractiveRequest{SessionID: sid, MCPConfigFile: brief + ".mcp.json", BriefFile: brief},
			[]string{"--mcp-config", brief + ".mcp.json", "--session-id", sid, "--", adapter.BriefPrompt(brief)}},
		{"tools with a brief, no other flag", adapter.InteractiveRequest{MCPConfigFile: brief + ".mcp.json", BriefFile: brief},
			[]string{"--mcp-config", brief + ".mcp.json", "--", adapter.BriefPrompt(brief)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.Cwd = h.Cwd
			c, err := a.Interactive(tt.req)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(c.Args, tt.want) || c.Stdin != adapter.StdinTerminal {
				t.Errorf("argv = %v, want %v", c.Args, tt.want)
			}
			out, rec := h.Run(t, c, `{"interactive_without_tty":true,"strict":["resume-with-session-id"]}`)
			if out.ExitCode != 0 || rec.Mode != "interactive" {
				t.Errorf("fake: exit %d %q", out.ExitCode, out.Stderr)
			}
			// W71-1: the brief stays the prompt, and --mcp-config one file.
			if tt.req.BriefFile != "" && !slices.Equal(rec.Positional, []string{adapter.BriefPrompt(tt.req.BriefFile)}) {
				t.Errorf("prompt = %q", rec.Positional)
			}
			if tt.req.MCPConfigFile != "" && !slices.Equal(rec.Flags["--mcp-config"], []string{tt.req.MCPConfigFile}) {
				t.Errorf("--mcp-config = %q", rec.Flags["--mcp-config"])
			}
		})
	}
	for _, r := range []adapter.InteractiveRequest{
		{Fork: true},
		{Resume: rid, SessionID: sid},
		{BriefFile: "brief.md"},
		{MemoryFile: "memory.md"},
		{MCPConfigFile: "mcp.json"},
	} {
		if _, err := a.Interactive(r); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
}

func TestParseHeadlessFromScenarios(t *testing.T) {
	h := fakecli.NewHarness(t, fakecli.Build(t))
	a := Adapter{Caps: []string{probe.CapSettingSources}}
	tests := []struct {
		scenario  string
		wantText  string
		wantErr   adapter.ErrorClass
		wantToken int64
		parseErr  error
	}{
		{"success", "review done", adapter.ErrNone, 150, nil},
		{"limit", "You've hit your limit · resets in 3h", adapter.ErrRateLimit, 0, nil},
		{"max_turns", "", adapter.ErrMaxTurns, 0, nil},
		{"garbage", "", adapter.ErrUnknown, 0, adapter.ErrUnparsed},
	}
	for _, tt := range tests {
		t.Run(tt.scenario, func(t *testing.T) {
			req := adapter.HeadlessRequest{Kind: adapter.Review, PromptFile: h.Prompt, Cwd: h.Cwd, RunDir: h.RunDir, SessionID: sid}
			c, err := a.Headless(req)
			if err != nil {
				t.Fatal(err)
			}
			out, _ := h.Run(t, c, scenario(t, h, tt.scenario))
			res, err := a.ParseHeadless(req, out)
			if !errors.Is(err, tt.parseErr) || res.Text != tt.wantText || res.Error != tt.wantErr || res.Tokens != tt.wantToken || res.SessionID != sid {
				t.Errorf("ParseHeadless = %+v, %v", res, err)
			}
			arts, err := a.Artifacts(req, res)
			if err != nil || len(arts) != 0 {
				t.Errorf("Artifacts = %v, %v", arts, err)
			}
		})
	}
}

func TestTranscriptAndPatches(t *testing.T) {
	a := New()
	if _, err := a.TranscriptTail(adapter.SessionRef{ID: sid}, 5); !errors.Is(err, adapter.ErrNoTranscript) {
		t.Errorf("TranscriptTail = %v", err)
	}
	if p, err := a.ConfigPatches(adapter.ConfigContext{}); err != nil || len(p) != 0 {
		t.Errorf("ConfigPatches = %v, %v", p, err)
	}
}
