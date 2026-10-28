package codex

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
)

const (
	tid = "019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b" // the scenarios' thread id
	oid = "7c4f1e2a-3b5d-4a6c-9e8f-1a2b3c4d5e6f"
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
	lock := []string{"--strict-config", "--ignore-user-config", "--sandbox", "read-only",
		"--disable", "shell_tool", "--disable", "unified_exec", "--disable", "view_image",
		"--disable", "apps", "--disable", "plugins", "--disable", "goals",
		"--disable", "image_generation", "--disable", "sleep_tool"}
	// image keeps image generation.
	imageLock := []string{"--strict-config", "--ignore-user-config", "--sandbox", "read-only",
		"--disable", "shell_tool", "--disable", "unified_exec", "--disable", "view_image",
		"--disable", "apps", "--disable", "plugins", "--disable", "goals", "--disable", "sleep_tool"}
	skip := []string{"--skip-git-repo-check"}
	end := []string{"--", "-"}
	tests := []struct {
		name     string
		req      adapter.HeadlessRequest
		want     []string
		scenario string
	}{
		{"image", adapter.HeadlessRequest{Kind: adapter.Image, IsolatedCwd: true},
			slices.Concat([]string{"exec", "--json"}, imageLock, skip, end), ""},
		{"image edit with a reference", adapter.HeadlessRequest{Kind: adapter.Image, Resume: tid, RefImages: []string{ref}},
			slices.Concat([]string{"exec", "-i", ref, "--json"}, imageLock, skip, []string{"resume", tid}, end), ""},
		{"review in a repo", adapter.HeadlessRequest{Kind: adapter.Review, GitRepo: true},
			slices.Concat([]string{"exec", "--json"}, lock, end), ""},
		{"review outside a repo", adapter.HeadlessRequest{Kind: adapter.Review},
			slices.Concat([]string{"exec", "--json"}, lock, skip, end), ""},
		{"review with a model", adapter.HeadlessRequest{Kind: adapter.Review, Model: "gpt-5.5", GitRepo: true},
			slices.Concat([]string{"exec", "--json", "-c", `model="gpt-5.5"`}, lock, end), ""},
		// W-1: the edit child ignores the user's config.toml (MCP servers,
		// writable_roots, network_access) like every other kind. K46-2:
		// /tmp and $TMPDIR are not writable for it.
		{"edit", adapter.HeadlessRequest{Kind: adapter.Edit, GitRepo: true}, []string{"exec", "--json",
			"--strict-config", "--ignore-user-config", "--sandbox", "workspace-write",
			"-c", "sandbox_workspace_write.exclude_slash_tmp=true", "-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true",
			"--disable", "view_image", "--disable", "apps", "--disable", "plugins", "--disable", "goals",
			"--disable", "image_generation", "--disable", "sleep_tool", "--", "-"}, ""},
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
		func(r *adapter.HeadlessRequest) { r.Kind, r.IsolatedCwd = adapter.Research, true },
		func(r *adapter.HeadlessRequest) { r.Kind, r.IsolatedCwd, r.WebSearch = adapter.News, true, true },
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
	alt := []string{probe.CapNoAltScreen}
	tests := []struct {
		name string
		caps []string
		req  adapter.InteractiveRequest
		want []string
	}{
		{"new", alt, adapter.InteractiveRequest{}, []string{"--no-alt-screen"}},
		{"new with a brief", alt, adapter.InteractiveRequest{BriefFile: brief}, []string{"--no-alt-screen", adapter.BriefPrompt(brief)}},
		{"resume", alt, adapter.InteractiveRequest{Resume: tid}, []string{"resume", "--no-alt-screen", tid}},
		// --no-alt-screen is not verified for Codex: without the probe
		// finding it in help, it is left out.
		{"new, help lacks the flag", nil, adapter.InteractiveRequest{}, nil},
		{"resume, help lacks the flag", nil, adapter.InteractiveRequest{Resume: tid}, []string{"resume", tid}},
		{"other caps only", []string{probe.CapConfigOverride}, adapter.InteractiveRequest{BriefFile: brief}, []string{adapter.BriefPrompt(brief)}},
		// -c is global and goes before resume; the value is a TOML string.
		{"resume with memory", alt, adapter.InteractiveRequest{Resume: tid, Memory: "a \"b\"\\\n\tc\x1b"},
			[]string{"-c", `developer_instructions="a \"b\"\\\n\tc\u001B"`, "resume", "--no-alt-screen", tid}},
		// T6708: each MCP server as -c overrides, before resume, with no
		// approval mode.
		{"tools on resume", alt, adapter.InteractiveRequest{Resume: tid, MCP: []adapter.MCPServer{
			{Name: "code-graph", Command: "codegraph", Args: []string{"serve", `a"b`}, Cwd: "/s/run/x", EnvVars: []string{"DEMO_HOME"}},
			{Name: "docs", Command: "docs-mcp"},
		}}, []string{
			"-c", `mcp_servers.code-graph.command="codegraph"`, "-c", `mcp_servers.code-graph.args=["serve", "a\"b"]`,
			"-c", `mcp_servers.code-graph.cwd="/s/run/x"`, "-c", `mcp_servers.code-graph.env_vars=["DEMO_HOME"]`,
			"-c", `mcp_servers.docs.command="docs-mcp"`, "-c", `mcp_servers.docs.args=[]`,
			"resume", "--no-alt-screen", tid}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.Cwd = h.Cwd
			c, err := Adapter{Caps: tt.caps}.Interactive(tt.req)
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
		{Resume: tid, Fork: true},
	} {
		if _, err := a.Interactive(r); !errors.Is(err, adapter.ErrUnsupported) {
			t.Errorf("%+v: %v, want ErrUnsupported", r, err)
		}
	}
	for _, s := range []adapter.MCPServer{
		{Name: "a.b", Command: "x"}, {Name: "a b", Command: "x"}, {Name: "", Command: "x"}, {Name: "x"},
		{Name: "x", Command: "x", Cwd: "rel"},
	} {
		if _, err := a.Interactive(adapter.InteractiveRequest{MCP: []adapter.MCPServer{s}}); err == nil {
			t.Errorf("%+v accepted", s)
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
			if err != nil || res.SessionID != tid || res.Text != tt.text || res.Error != tt.class || res.Tokens != tt.tokens {
				t.Fatalf("ParseHeadless = %+v, %v", res, err)
			}
			arts, err := a.Artifacts(req, res)
			if err != nil || len(arts) != tt.images {
				t.Fatalf("Artifacts = %v, %v", arts, err)
			}
			for i, art := range arts {
				want := filepath.Join(h.Home, ".codex", "generated_images", tid, []string{"1.png", "2.png"}[i])
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
	if err != nil || len(arts) != 2 || filepath.Dir(arts[0].Path) != filepath.Join(codexHome, "generated_images", tid) {
		t.Errorf("Artifacts = %v, %v", arts, err)
	}
	// A session id that is not a valid id is an error.
	if arts, err := a.Artifacts(req, adapter.HeadlessResult{SessionID: "../" + tid}); err == nil || len(arts) != 0 {
		t.Errorf("escaping id = %v, %v", arts, err)
	}
}

// TestArtifactsEveryFile (T6632): every file in the thread's folder is
// listed, whatever its name, for the artifact gate to judge; a
// subfolder is not.
func TestArtifactsEveryFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "generated_images", tid)
	for _, p := range []string{"1.png", "2.PNG", "3.webp", "notes", "sub/4.png"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "1.png"), filepath.Join(dir, "5.png")); err != nil {
		t.Fatal(err)
	}
	a := Adapter{Getenv: func(k string) string {
		if k == "CODEX_HOME" {
			return home
		}
		return ""
	}}
	arts, err := a.Artifacts(adapter.HeadlessRequest{}, adapter.HeadlessResult{SessionID: tid})
	var names []string
	for _, x := range arts {
		names = append(names, filepath.Base(x.Path))
	}
	if err != nil || !slices.Equal(names, []string{"1.png", "2.PNG", "3.webp", "5.png", "notes"}) {
		t.Errorf("Artifacts = %q, %v", names, err)
	}
}

func TestTranscriptAndPatches(t *testing.T) {
	a := New()
	if _, err := a.TranscriptTail(adapter.SessionRef{ID: tid}, 5); !errors.Is(err, adapter.ErrNoTranscript) {
		t.Errorf("TranscriptTail = %v", err)
	}
	if p, err := a.ConfigPatches(adapter.ConfigContext{}); err != nil || len(p) != 0 {
		t.Errorf("ConfigPatches = %v, %v", p, err)
	}
}
