package codex

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/safefs"
)

func scenario(t *testing.T, name string) string {
	t.Helper()
	b, err := safefs.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHeadlessGoldenArgv(t *testing.T) {
	h := fakecli.NewHarness(t, fakecli.Build(t))
	a := New()
	ref := filepath.Join(h.Cwd, "style.png")
	isolated := []string{"exec", "--json", "--sandbox", "read-only",
		"--disable", "shell_tool", "--disable", "unified_exec", "--disable", "view_image", "-c", `web_search="live"`, "-"}
	tests := []struct {
		name     string
		req      adapter.HeadlessRequest
		want     []string
		scenario string
	}{
		{"research", adapter.HeadlessRequest{Kind: adapter.Research}, isolated, ""},
		{"news", adapter.HeadlessRequest{Kind: adapter.News}, isolated, ""},
		{"image", adapter.HeadlessRequest{Kind: adapter.Image}, []string{"exec", "--json", "--sandbox", "read-only", "-"}, ""},
		{"image edit with a reference", adapter.HeadlessRequest{Kind: adapter.Image, Resume: "t-1", RefImages: []string{ref}},
			[]string{"exec", "resume", "t-1", "--json", "--sandbox", "read-only", "-i", ref, "-"}, ""},
		{"review", adapter.HeadlessRequest{Kind: adapter.Review}, []string{"exec", "--json", "--sandbox", "read-only", "-"}, ""},
		{"review with web search and a model", adapter.HeadlessRequest{Kind: adapter.Review, WebSearch: true, Model: "gpt-5.5"},
			[]string{"exec", "--json", "-c", `model="gpt-5.5"`, "--sandbox", "read-only", "-c", `web_search="live"`, "-"}, ""},
		{"edit", adapter.HeadlessRequest{Kind: adapter.Edit}, []string{"exec", "--json", "--sandbox", "workspace-write", "-"},
			`{"extra_values":{"--sandbox":["workspace-write"]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.PromptFile, tt.req.Cwd = h.Prompt, h.Cwd
			c, err := a.Headless(tt.req)
			if err != nil {
				t.Fatal(err)
			}
			if c.Path != Bin || !slices.Equal(c.Args, tt.want) {
				t.Errorf("argv = %s %q, want %q", c.Path, c.Args, tt.want)
			}
			if c.Stdin != adapter.StdinFile || c.StdinPath != h.Prompt || c.Dir != h.Cwd {
				t.Errorf("stdin %v %q dir %q", c.Stdin, c.StdinPath, c.Dir)
			}
			out, rec := h.Run(t, c, tt.scenario)
			if out.ExitCode != 0 || rec.Mode != "headless" || rec.Stdin != "the task\n" || rec.StdinTerminal {
				t.Errorf("fake: exit %d %q, record %+v", out.ExitCode, out.Stderr, rec)
			}
		})
	}
}

func TestHeadlessUnsupported(t *testing.T) {
	a := New()
	base := adapter.HeadlessRequest{Kind: adapter.Review, PromptFile: "/p/prompt.md", Cwd: "/w"}
	for _, mod := range []func(*adapter.HeadlessRequest){
		func(r *adapter.HeadlessRequest) { r.Kind = adapter.Video },
		func(r *adapter.HeadlessRequest) { r.AllowedTools = []string{"shell"} },
		func(r *adapter.HeadlessRequest) { r.SessionID = "0b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e" },
	} {
		r := base
		mod(&r)
		if _, err := a.Headless(r); !errors.Is(err, adapter.ErrUnsupported) {
			t.Errorf("%+v: err = %v, want ErrUnsupported", r, err)
		}
	}
	for _, mod := range []func(*adapter.HeadlessRequest){
		func(r *adapter.HeadlessRequest) { r.PromptFile = "rel.md" },
		func(r *adapter.HeadlessRequest) { r.RefImages = []string{"rel.png"} },
		func(r *adapter.HeadlessRequest) { r.Model = `x" -c sandbox="danger` },
	} {
		r := base
		mod(&r)
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
		{"new", adapter.InteractiveRequest{}, []string{"--no-alt-screen"}},
		{"new with a brief", adapter.InteractiveRequest{BriefFile: brief}, []string{"--no-alt-screen", adapter.BriefPrompt(brief)}},
		{"resume", adapter.InteractiveRequest{Resume: "t-1"}, []string{"resume", "--no-alt-screen", "t-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.Cwd = h.Cwd
			c, err := a.Interactive(tt.req)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(c.Args, tt.want) || c.Stdin != adapter.StdinTerminal {
				t.Errorf("argv = %q, want %q", c.Args, tt.want)
			}
			out, rec := h.Run(t, c, `{"interactive_without_tty":true}`)
			if out.ExitCode != 0 || rec.Mode != "interactive" {
				t.Errorf("fake: exit %d %q", out.ExitCode, out.Stderr)
			}
		})
	}
	for _, r := range []adapter.InteractiveRequest{
		{SessionID: "0b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e"},
		{Resume: "t-1", Fork: true},
	} {
		if _, err := a.Interactive(r); !errors.Is(err, adapter.ErrUnsupported) {
			t.Errorf("%+v: %v, want ErrUnsupported", r, err)
		}
	}
}

func TestParseHeadlessFromScenarios(t *testing.T) {
	fakes := fakecli.Build(t)
	tests := []struct {
		scenario string
		text     string
		class    adapter.ErrorClass
		tokens   int64
		images   int
	}{
		{"images", "made two images", adapter.ErrNone, 120, 2},
		{"usage_limit", "", adapter.ErrRateLimit, 0, 0},
		{"usage_limit_stderr", "", adapter.ErrRateLimit, 0, 0},
		{"failure", "", adapter.ErrUnknown, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.scenario, func(t *testing.T) {
			h := fakecli.NewHarness(t, fakes)
			a := Adapter{Getenv: h.Getenv}
			req := adapter.HeadlessRequest{Kind: adapter.Image, PromptFile: h.Prompt, Cwd: h.Cwd}
			c, err := a.Headless(req)
			if err != nil {
				t.Fatal(err)
			}
			out, _ := h.Run(t, c, scenario(t, tt.scenario))
			res, err := a.ParseHeadless(req, out)
			if err != nil || res.SessionID != "t-0199" || res.Text != tt.text || res.Error != tt.class || res.Tokens != tt.tokens {
				t.Fatalf("ParseHeadless = %+v, %v", res, err)
			}
			arts, err := a.Artifacts(req, res)
			if err != nil || len(arts) != tt.images {
				t.Fatalf("Artifacts = %v, %v", arts, err)
			}
			for i, art := range arts {
				want := filepath.Join(h.Home, ".codex", "generated_images", "t-0199", []string{"1.png", "2.png"}[i])
				if art.Path != want || art.Kind != "image" {
					t.Errorf("artifact %d = %+v, want %s", i, art, want)
				}
			}
		})
	}
	// Output with no thread.started is not a Codex run.
	if res, err := New().ParseHeadless(adapter.HeadlessRequest{}, adapter.Output{Stdout: []byte("hello\n")}); !errors.Is(err, adapter.ErrUnparsed) || res.Error != adapter.ErrUnknown {
		t.Errorf("unparsed = %+v, %v", res, err)
	}
}

func TestArtifactsUnderCodexHome(t *testing.T) {
	h := fakecli.NewHarness(t, fakecli.Build(t))
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	a := Adapter{Getenv: func(k string) string {
		if k == "CODEX_HOME" {
			return codexHome
		}
		return h.Getenv(k)
	}}
	req := adapter.HeadlessRequest{Kind: adapter.Image, PromptFile: h.Prompt, Cwd: h.Cwd}
	c, err := a.Headless(req)
	if err != nil {
		t.Fatal(err)
	}
	c.Env = append(c.Env, "CODEX_HOME="+codexHome)
	out, _ := h.Run(t, c, scenario(t, "images"))
	res, err := a.ParseHeadless(req, out)
	if err != nil {
		t.Fatal(err)
	}
	arts, err := a.Artifacts(req, res)
	if err != nil || len(arts) != 2 || filepath.Dir(arts[0].Path) != filepath.Join(codexHome, "generated_images", "t-0199") {
		t.Errorf("Artifacts = %v, %v", arts, err)
	}
	// A session id that is not one path element finds nothing.
	if arts, err := a.Artifacts(req, adapter.HeadlessResult{SessionID: "../t-0199"}); err != nil || len(arts) != 0 {
		t.Errorf("escaping id = %v, %v", arts, err)
	}
}

func TestTranscriptAndPatches(t *testing.T) {
	a := New()
	if _, err := a.TranscriptTail(adapter.SessionRef{ID: "t-1"}, 5); !errors.Is(err, adapter.ErrNoTranscript) {
		t.Errorf("TranscriptTail = %v", err)
	}
	if p, err := a.ConfigPatches(adapter.ConfigContext{}); err != nil || len(p) != 0 {
		t.Errorf("ConfigPatches = %v, %v", p, err)
	}
}
