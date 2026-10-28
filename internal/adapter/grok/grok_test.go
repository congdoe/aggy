package grok

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/safefs"
)

const sid = "5f0c6a2e-9b1d-4e7a-8c3f-2d4b6a8e0c1f"

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
	a := New()
	search := "web_search,x_keyword_search,x_semantic_search"
	tests := []struct {
		name     string
		req      adapter.HeadlessRequest
		want     []string
		scenario string
	}{
		{"research", adapter.HeadlessRequest{Kind: adapter.Research, MaxTurns: 30},
			[]string{"-p", "--output-format", "json", "--tools", search, "--max-turns", "30"}, ""},
		{"news", adapter.HeadlessRequest{Kind: adapter.News}, []string{"-p", "--output-format", "json", "--tools", search}, ""},
		{"image", adapter.HeadlessRequest{Kind: adapter.Image, SessionID: sid},
			[]string{"-p", "--output-format", "json", "--session-id", sid, "--tools", "image_gen"}, ""},
		{"image edit", adapter.HeadlessRequest{Kind: adapter.Image, Resume: sid},
			[]string{"-p", "--output-format", "json", "--resume", sid, "--tools", "image_gen,image_edit"}, ""},
		{"video", adapter.HeadlessRequest{Kind: adapter.Video}, []string{"-p", "--output-format", "json", "--tools", "video_gen"}, ""},
		{"review", adapter.HeadlessRequest{Kind: adapter.Review},
			[]string{"-p", "--output-format", "json", "--permission-mode", "plan", "--tools", "read_file,grep,list_dir"}, ""},
		{"edit with web search and a model", adapter.HeadlessRequest{Kind: adapter.Edit, WebSearch: true, Model: "grok-4.7"},
			[]string{"-p", "--output-format", "json", "--model", "grok-4.7", "--tools",
				"read_file,grep,list_dir,write,search_replace,run_terminal_cmd,web_search"}, `{"extra_flags":["--model"]}`},
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
			if out.ExitCode != 0 || rec.Mode != "headless" || rec.Stdin != "the task\n" {
				t.Errorf("fake: exit %d %q, record %+v", out.ExitCode, out.Stderr, rec)
			}
		})
	}
}

func TestHeadlessRefusals(t *testing.T) {
	a := New()
	base := adapter.HeadlessRequest{PromptFile: "/p/prompt.md", Cwd: "/w"}
	for _, r := range []adapter.HeadlessRequest{
		{Kind: adapter.Research, AllowedTools: []string{"read_file"}},
		{Kind: adapter.News, AllowedTools: []string{"web_fetch"}},
		{Kind: adapter.Research, AllowedTools: []string{"Agent"}},
		{Kind: adapter.Review, AllowedTools: []string{"write"}},
		{Kind: adapter.Image, AllowedTools: []string{"run_terminal_cmd"}},
		{Kind: adapter.Edit, AllowedTools: []string{"a,b"}},
		{Kind: adapter.Review, SessionID: "nope"},
		{Kind: adapter.Review, Resume: "r", SessionID: sid},
		{Kind: "chat"},
	} {
		r.PromptFile, r.Cwd = base.PromptFile, base.Cwd
		if _, err := a.Headless(r); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
	r := base
	r.Kind, r.RefImages = adapter.Image, []string{"/x.png"}
	if _, err := a.Headless(r); !errors.Is(err, adapter.ErrUnsupported) {
		t.Errorf("reference images: %v, want ErrUnsupported", err)
	}
	// A search tool the kind lacks may be added to research.
	r = base
	r.Kind, r.AllowedTools = adapter.Research, []string{"x_semantic_search", "extra_search"}
	c, err := a.Headless(r)
	if err != nil || !slices.Contains(c.Args, "web_search,x_keyword_search,x_semantic_search,extra_search") {
		t.Errorf("research extra tool = %q, %v", c.Args, err)
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
		{"new with an id and a brief", adapter.InteractiveRequest{SessionID: sid, BriefFile: brief},
			[]string{"--no-alt-screen", "--session-id", sid, adapter.BriefPrompt(brief)}},
		{"resume", adapter.InteractiveRequest{Resume: sid}, []string{"--no-alt-screen", "--resume", sid}},
		{"fork", adapter.InteractiveRequest{Resume: sid, Fork: true}, []string{"--no-alt-screen", "--resume", sid, "--fork-session"}},
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
			out, rec := h.Run(t, c, `{"interactive_without_tty":true,"strict":["resume-with-session-id"]}`)
			if out.ExitCode != 0 || rec.Mode != "interactive" {
				t.Errorf("fake: exit %d %q", out.ExitCode, out.Stderr)
			}
		})
	}
}

func TestParseHeadlessFromScenarios(t *testing.T) {
	fakes := fakecli.Build(t)
	tests := []struct {
		scenario string
		text     string
		class    adapter.ErrorClass
		images   int
	}{
		{"images", "saved images/1.jpg", adapter.ErrNone, 2},
		{"limit", "", adapter.ErrRateLimit, 0},
		{"limit_stderr", "", adapter.ErrRateLimit, 0},
		{"failure", "", adapter.ErrUnknown, 0},
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
			out, _ := h.Run(t, c, scenario(t, h, tt.scenario))
			res, err := a.ParseHeadless(req, out)
			if err != nil || res.SessionID != sid || res.Text != tt.text || res.Error != tt.class {
				t.Fatalf("ParseHeadless = %+v, %v", res, err)
			}
			arts, err := a.Artifacts(req, res)
			if err != nil || len(arts) != tt.images {
				t.Fatalf("Artifacts = %v, %v", arts, err)
			}
			for i, art := range arts {
				want := filepath.Join(h.Home, ".grok", "sessions", EncodeCwd(h.Cwd), sid, "images", []string{"1.jpg", "2.jpg"}[i])
				if art.Path != want || art.Kind != "image" {
					t.Errorf("artifact %d = %+v, want %s", i, art, want)
				}
			}
		})
	}
	if res, err := New().ParseHeadless(adapter.HeadlessRequest{}, adapter.Output{Stdout: []byte("hello\n")}); !errors.Is(err, adapter.ErrUnparsed) || res.Error != adapter.ErrUnknown {
		t.Errorf("unparsed = %+v, %v", res, err)
	}
}

// TestArtifactsFallbackScan: when the request's cwd does not encode to the
// folder Grok used, the one session folder with that id is found by scan.
func TestArtifactsFallbackScan(t *testing.T) {
	h := fakecli.NewHarness(t, fakecli.Build(t))
	grokHome := filepath.Join(t.TempDir(), "grok-home")
	a := Adapter{Getenv: func(k string) string {
		if k == "GROK_HOME" {
			return grokHome
		}
		return h.Getenv(k)
	}}
	req := adapter.HeadlessRequest{Kind: adapter.Image, PromptFile: h.Prompt, Cwd: h.Cwd}
	c, err := a.Headless(req)
	if err != nil {
		t.Fatal(err)
	}
	c.Env = append(c.Env, "GROK_HOME="+grokHome)
	out, _ := h.Run(t, c, scenario(t, h, "images"))
	res, err := a.ParseHeadless(req, out)
	if err != nil {
		t.Fatal(err)
	}
	req.Cwd = "/some/other/spelling"
	arts, err := a.Artifacts(req, res)
	if err != nil || len(arts) != 2 || !strings.HasPrefix(arts[0].Path, filepath.Join(grokHome, "sessions")) {
		t.Errorf("Artifacts = %v, %v", arts, err)
	}
	// The same session id under two working dirs is ambiguous.
	if err := safefs.MkdirAll(filepath.Join(grokHome, "sessions", "other", sid, "images"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := safefs.WriteFile(filepath.Join(grokHome, "sessions", "other", sid, "images", "1.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Artifacts(req, res); err == nil {
		t.Error("ambiguous session folders accepted")
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
