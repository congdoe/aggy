package grok

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
)

// sandboxed is an adapter whose probe found --sandbox, on a darwin host
// that can apply Grok's sandbox, with HOME /home/u and no symlinks.
func sandboxed() Adapter {
	return Adapter{Caps: []string{probe.CapSandbox, probe.CapKernelSandbox}, GOOS: "darwin", Getenv: func(k string) string {
		if k == "HOME" {
			return "/home/u"
		}
		return ""
	}, Realpath: func(p string) (string, error) { return p, nil }}
}

// denyArgs is --deny Read(p) --deny Grep(p) for each pattern.
func denyArgs(pats ...string) []string {
	var out []string
	for _, p := range pats {
		out = append(out, "--deny", "Read("+p+")", "--deny", "Grep("+p+")")
	}
	return out
}

// spellingPats are the spelling-independent patterns every deny set ends
// with: any ~ path, and any path with a quote- or space-led component.
var spellingPats = []string{"~*", "~*/**", "**/[" + leadChars + "]*", "**/[" + leadChars + "]*/**"}

// sandboxedDenies is readDeny's argv for sandboxed().
var sandboxedDenies = denyArgs(slices.Concat([]string{"/home/u/Library/**", "/home/u/.grok/**", "/System/Volumes/Data/**",
	"/[Hh][Oo][Mm][Ee]/[Uu]/[Ll][Ii][Bb][Rr][Aa][Rr][Yy]/**", "/[Hh][Oo][Mm][Ee]/[Uu]/.[Gg][Rr][Oo][Kk]/**",
	"/[Ss][Yy][Ss][Tt][Ee][Mm]/[Vv][Oo][Ll][Uu][Mm][Ee][Ss]/[Dd][Aa][Tt][Aa]/**"}, spellingPats)...)

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
	a := sandboxed()
	all := strings.Split("read_file,grep,list_dir,write,search_replace,run_terminal_cmd,web_fetch,Agent,search_tool,use_tool,"+
		"image_gen,image_edit,image_to_video,reference_to_video,send_feedback,monitor,workflow,todo_write,"+
		"ask_user_question,enter_plan_mode,exit_plan_mode,web_search", ",")
	// deny is every lockdown tool but the kind's own.
	deny := func(keep ...string) string {
		var out []string
		for _, t := range all {
			if !slices.Contains(keep, t) {
				out = append(out, t)
			}
		}
		return strings.Join(out, ",")
	}
	tail := func(tools string, keep ...string) []string {
		return []string{"--tools", tools, "--disallowed-tools", deny(keep...), "--no-subagents"}
	}
	head := []string{"--prompt-file", h.Prompt, "--output-format", "json"}
	denies := sandboxedDenies
	strict := slices.Concat([]string{"--sandbox", "strict"}, denies)
	tests := []struct {
		name     string
		req      adapter.HeadlessRequest
		want     []string
		scenario string
	}{
		{"research", adapter.HeadlessRequest{Kind: adapter.Research, MaxTurns: 30, IsolatedCwd: true},
			slices.Concat(head, strict, tail("web_search", "web_search"), []string{"--max-turns", "30"}), ""},
		{"news", adapter.HeadlessRequest{Kind: adapter.News, IsolatedCwd: true, WebSearch: true, MaxTurns: 15},
			slices.Concat(head, strict, tail("web_search", "web_search"), []string{"--max-turns", "15"}), ""},
		{"image", adapter.HeadlessRequest{Kind: adapter.Image, SessionID: sid},
			slices.Concat(head, []string{"--session-id", sid}, denies, tail("image_gen", "image_gen")), ""},
		{"image edit", adapter.HeadlessRequest{Kind: adapter.Image, Resume: sid, FileTools: true},
			slices.Concat(head, []string{"--resume", sid}, denies, tail("image_gen,image_edit", "image_gen", "image_edit")), ""},
		{"video", adapter.HeadlessRequest{Kind: adapter.Video, FileTools: true},
			slices.Concat(head, denies, tail("image_gen,image_to_video", "image_gen", "image_to_video")), ""},
		{"review", adapter.HeadlessRequest{Kind: adapter.Review},
			slices.Concat(head, []string{"--permission-mode", "plan"}, strict, tail("read_file,grep,list_dir", "read_file", "grep", "list_dir")), ""},
		{"edit with a model", adapter.HeadlessRequest{Kind: adapter.Edit, Model: "grok-4.7"},
			slices.Concat(head, []string{"--model", "grok-4.7"},
				tail("read_file,grep,list_dir,write,search_replace", "read_file", "grep", "list_dir", "write", "search_replace")),
			`{"extra_flags":["--model"]}`},
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
			// The prompt goes by --prompt-file, never stdin or argv.
			if c.Stdin != adapter.StdinNull || c.StdinPath != "" || c.Dir != h.Cwd {
				t.Errorf("stdin %v %q dir %q", c.Stdin, c.StdinPath, c.Dir)
			}
			out, rec := h.Run(t, c, tt.scenario)
			if strings.Contains(string(out.Stderr), UnknownToolWarning) {
				t.Errorf("an unknown tool name in %q: %s", c.Args, out.Stderr)
			}
			if out.ExitCode != 0 || rec.Mode != "headless" || rec.Prompt != "the task\n" || rec.Stdin != "" || slices.Contains(c.Args, "the task\n") {
				t.Errorf("fake: exit %d %q, record %+v", out.ExitCode, out.Stderr, rec)
			}
		})
	}
}

// TestSandboxFailsClosed: review, research, and news (and image and
// video if strictMedia is on) need --sandbox strict, so without the
// probe's sandbox capability, or on a host without a kernel sandbox,
// they are refused (ErrNoCapability, which is ErrUnsupported); edit, and
// image and video while strictMedia is off, do not use it.
func TestSandboxFailsClosed(t *testing.T) {
	all := []adapter.Kind{adapter.Review, adapter.Research, adapter.News, adapter.Image, adapter.Video, adapter.Edit}
	// G32-1: no kernel sandbox on the host refuses the sandboxed kinds.
	noKernel := sandboxed()
	noKernel.Caps = []string{probe.CapSandbox}
	for _, k := range all {
		req := adapter.HeadlessRequest{Kind: k, PromptFile: "/p/prompt.md", Cwd: "/w", IsolatedCwd: true, FileTools: true}
		if c, err := noKernel.Headless(req); usesSandbox(k) != errors.Is(err, adapter.ErrNoCapability) {
			t.Errorf("%s with no kernel sandbox: %q, %v", k, c.Args, err)
		}
	}
	// G32-3: a HOME that cannot be named in a deny rule refuses every
	// sandboxed kind. Image and video without strict (N54-4) run without
	// the denies instead: they match none of their tools.
	home := func(h string) Adapter {
		a := sandboxed()
		a.Getenv = func(string) string { return h }
		return a
	}
	for _, a := range []Adapter{home(""), home("rel/home"), home("/"), home("/home/u*"), home("/home/[u]"), home("/home/a,b"), home("/home/u)"), home("/home/../u")} {
		for _, k := range all {
			req := adapter.HeadlessRequest{Kind: k, PromptFile: "/p/prompt.md", Cwd: "/w", IsolatedCwd: true, FileTools: true}
			c, err := a.Headless(req)
			if usesSandbox(k) != errors.Is(err, adapter.ErrNoCapability) || (!usesSandbox(k) && (err != nil || slices.Contains(c.Args, "--deny"))) {
				t.Errorf("%s with HOME %q: %q, %v", k, a.Getenv("HOME"), c.Args, err)
			}
		}
	}
	for _, k := range []adapter.Kind{adapter.Review, adapter.Research, adapter.News, adapter.Image, adapter.Video, adapter.Edit} {
		req := adapter.HeadlessRequest{Kind: k, PromptFile: "/p/prompt.md", Cwd: "/w", IsolatedCwd: true, FileTools: true}
		_, err := New().Headless(req)
		c, cerr := sandboxed().Headless(req)
		strict := slices.Contains(c.Args, "--sandbox") && c.Args[slices.Index(c.Args, "--sandbox")+1] == "strict"
		switch {
		case usesSandbox(k):
			if !errors.Is(err, adapter.ErrNoCapability) || !errors.Is(err, adapter.ErrUnsupported) {
				t.Errorf("%s without the sandbox capability: %v", k, err)
			}
			if cerr != nil || !strict {
				t.Errorf("%s with it: %q, %v", k, c.Args, cerr)
			}
		default:
			if err != nil || cerr != nil || slices.Contains(c.Args, "--sandbox") {
				t.Errorf("%s: %q, %v, %v", k, c.Args, err, cerr)
			}
		}
	}
}

// TestStrictMediaSwitch: strictMedia is off by default, since image
// under strict failed live (TODO(T6751)); image and video then run with
// no --sandbox and no sandbox capability needed, but keep review's read
// denies. On, they also get --sandbox strict and its host check.
func TestStrictMediaSwitch(t *testing.T) {
	if strictMedia {
		t.Error("strictMedia is on by default")
	}
	defer func(old bool) { strictMedia = old }(strictMedia)
	denies := strings.Join(sandboxedDenies, " ") + " "
	tests := []struct {
		req  adapter.HeadlessRequest
		tail string // the argv after --output-format json
	}{
		{adapter.HeadlessRequest{Kind: adapter.Image},
			"--tools image_gen --disallowed-tools read_file,grep,list_dir,write,search_replace,run_terminal_cmd,web_fetch,Agent,search_tool,use_tool," +
				"image_edit,image_to_video,reference_to_video,send_feedback,monitor,workflow,todo_write,ask_user_question,enter_plan_mode,exit_plan_mode,web_search --no-subagents"},
		{adapter.HeadlessRequest{Kind: adapter.Image, Resume: sid, FileTools: true},
			"--tools image_gen,image_edit --disallowed-tools read_file,grep,list_dir,write,search_replace,run_terminal_cmd,web_fetch,Agent,search_tool,use_tool," +
				"image_to_video,reference_to_video,send_feedback,monitor,workflow,todo_write,ask_user_question,enter_plan_mode,exit_plan_mode,web_search --no-subagents"},
		{adapter.HeadlessRequest{Kind: adapter.Video, FileTools: true},
			"--tools image_gen,image_to_video --disallowed-tools read_file,grep,list_dir,write,search_replace,run_terminal_cmd,web_fetch,Agent,search_tool,use_tool," +
				"image_edit,reference_to_video,send_feedback,monitor,workflow,todo_write,ask_user_question,enter_plan_mode,exit_plan_mode,web_search --no-subagents"},
	}
	for _, on := range []bool{true, false} {
		strictMedia = on
		for _, tt := range tests {
			tt.req.PromptFile, tt.req.Cwd = "/p/prompt.md", "/w"
			head := "--prompt-file /p/prompt.md --output-format json "
			if tt.req.Resume != "" {
				head += "--resume " + sid + " "
			}
			want := head + denies + tt.tail
			if on {
				want = head + "--sandbox strict " + denies + tt.tail
			}
			c, err := sandboxed().Headless(tt.req)
			if err != nil || strings.Join(c.Args, " ") != want {
				t.Errorf("strictMedia %v, %s: %q, %v\nwant %s", on, tt.req.Kind, c.Args, err, want)
			}
			// No sandbox capability: refused when on, built when off.
			_, err = New().Headless(tt.req)
			if on != errors.Is(err, adapter.ErrNoCapability) || (!on && err != nil) {
				t.Errorf("strictMedia %v, %s without the capability: %v", on, tt.req.Kind, err)
			}
			if got := usesSandbox(tt.req.Kind); got != on {
				t.Errorf("strictMedia %v: usesSandbox(%s) = %v", on, tt.req.Kind, got)
			}
		}
	}
}

func TestHeadlessRefusals(t *testing.T) {
	a := sandboxed()
	base := adapter.HeadlessRequest{PromptFile: "/p/prompt.md", Cwd: "/w"}
	for _, r := range []adapter.HeadlessRequest{
		{Kind: adapter.Research, AllowedTools: []string{"read_file"}},
		{Kind: adapter.News, AllowedTools: []string{"web_fetch"}},
		{Kind: adapter.Research, AllowedTools: []string{"Agent"}},
		{Kind: adapter.Review, AllowedTools: []string{"write"}},
		{Kind: adapter.Image, AllowedTools: []string{"run_terminal_cmd"}},
		{Kind: adapter.Edit, AllowedTools: []string{"a,b"}},
		{Kind: adapter.Research, AllowedTools: []string{"x_semantic_search"}, IsolatedCwd: true},
		{Kind: adapter.Research, AllowedTools: []string{"search_tool"}, IsolatedCwd: true},
		{Kind: adapter.News, AllowedTools: []string{"use_tool"}, IsolatedCwd: true},
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
	// T6709: a child's tools are gated with the session's.
	r = base
	r.Kind, r.MCP = adapter.Edit, []adapter.MCPServer{{Name: "x", Command: "/x"}}
	if _, err := a.Headless(r); !errors.Is(err, adapter.ErrUnsupported) {
		t.Errorf("tools: %v, want ErrUnsupported", err)
	}
	// A permitted tool in another case maps to its own name.
	r = base
	r.Kind, r.AllowedTools, r.FileTools = adapter.Image, []string{"IMAGE_EDIT"}, true
	c, err := a.Headless(r)
	if err != nil || !slices.Contains(c.Args, "image_gen,image_edit") {
		t.Errorf("image_edit = %q, %v", c.Args, err)
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
		{"memory and a brief", adapter.InteractiveRequest{Memory: "notes, with a comma", BriefFile: brief},
			[]string{"--no-alt-screen", "--rules", "notes, with a comma", adapter.BriefPrompt(brief)}},
		{"policy", adapter.InteractiveRequest{Deny: []string{"Read(**/.env)", "Bash(rm -rf *)"}},
			[]string{"--no-alt-screen", "--deny", "Read(**/.env)", "--deny", "Bash(rm -rf *)"}},
		// T6708: the tools' MCP servers through an agent file.
		{"tools on resume", adapter.InteractiveRequest{Resume: sid, AgentFile: brief + ".agent.md"},
			[]string{"--no-alt-screen", "--resume", sid, "--agent", brief + ".agent.md"}},
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
	for _, r := range []string{"Bash(a,b)", ""} {
		if _, err := a.Interactive(adapter.InteractiveRequest{Deny: []string{r}}); err == nil {
			t.Errorf("deny rule %q accepted", r)
		}
	}
	if _, err := a.Interactive(adapter.InteractiveRequest{AgentFile: "agent.md"}); err == nil {
		t.Error("a relative agent file accepted")
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
			a := Adapter{Getenv: h.Getenv, Caps: []string{probe.CapSandbox, probe.CapKernelSandbox}}
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
	a := Adapter{Caps: []string{probe.CapSandbox, probe.CapKernelSandbox}, Getenv: func(k string) string {
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

// TestArtifactsEveryFile (T6633): every file in the session's images
// folder is listed, for the artifact gate to judge.
func TestArtifactsEveryFile(t *testing.T) {
	home := t.TempDir()
	cwd := "/tmp/work"
	dir := filepath.Join(home, "sessions", EncodeCwd(cwd), sid, "images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"1.jpg", "2.png", "notes"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a := Adapter{Getenv: func(k string) string {
		if k == "GROK_HOME" {
			return home
		}
		return ""
	}}
	arts, err := a.Artifacts(adapter.HeadlessRequest{Cwd: cwd}, adapter.HeadlessResult{SessionID: sid})
	if err != nil || len(arts) != 3 || arts[2].Path != filepath.Join(dir, "notes") || arts[2].Kind != "image" {
		t.Errorf("Artifacts = %v, %v", arts, err)
	}
}

// TestVideoArtifacts (T6637): the video kind lists the session's videos
// folder, where Grok's video tools save <n>.mp4.
func TestVideoArtifacts(t *testing.T) {
	home := t.TempDir()
	cwd := "/tmp/work"
	session := filepath.Join(home, "sessions", EncodeCwd(cwd), sid)
	for _, p := range []string{"images/1.jpg", "videos/1.mp4"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(session, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(session, p), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a := Adapter{Getenv: func(k string) string {
		if k == "GROK_HOME" {
			return home
		}
		return ""
	}}
	arts, err := a.Artifacts(adapter.HeadlessRequest{Kind: adapter.Video, Cwd: cwd}, adapter.HeadlessResult{SessionID: sid})
	if err != nil || len(arts) != 1 || arts[0] != (adapter.Artifact{Path: filepath.Join(session, "videos", "1.mp4"), Kind: "video"}) {
		t.Errorf("Artifacts = %v, %v", arts, err)
	}
}

// TestFileToolsOptIn (B15-2): image_edit, image_to_video, and
// reference_to_video read any local path and upload it, so they need the
// user's opt-in (FileTools). Without it, video and an image edit fail
// with ErrFileTools, and AllowedTools cannot name them; reference_to_video
// is never given.
func TestFileToolsOptIn(t *testing.T) {
	a := sandboxed()
	base := adapter.HeadlessRequest{PromptFile: "/p/prompt.md", Cwd: "/w"}
	for _, r := range []adapter.HeadlessRequest{
		{Kind: adapter.Video},
		{Kind: adapter.Image, Resume: sid},
	} {
		r.PromptFile, r.Cwd = base.PromptFile, base.Cwd
		if _, err := a.Headless(r); !errors.Is(err, adapter.ErrFileTools) || !errors.Is(err, adapter.ErrUnsupported) {
			t.Errorf("%s resume=%q: %v, want ErrFileTools", r.Kind, r.Resume, err)
		}
	}
	for _, r := range []adapter.HeadlessRequest{
		{Kind: adapter.Image, AllowedTools: []string{"image_edit"}},
		{Kind: adapter.Image, AllowedTools: []string{"image_to_video"}, FileTools: true},
		{Kind: adapter.Video, AllowedTools: []string{"reference_to_video"}, FileTools: true},
	} {
		r.PromptFile, r.Cwd = base.PromptFile, base.Cwd
		if _, err := a.Headless(r); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
	// A plain image never gets a file tool, opt-in or not.
	for _, ft := range []bool{false, true} {
		c, err := a.Headless(adapter.HeadlessRequest{Kind: adapter.Image, PromptFile: base.PromptFile, Cwd: base.Cwd, FileTools: ft})
		if err != nil || !slices.Contains(c.Args, "image_gen") {
			t.Fatalf("image: %q, %v", c.Args, err)
		}
		deny := c.Args[slices.Index(c.Args, "--disallowed-tools")+1]
		for _, f := range FileTools {
			if !strings.Contains(deny, f) {
				t.Errorf("image (file tools %v) does not deny %s: %q", ft, f, deny)
			}
		}
	}
}

// liveReply has the shape grok 1.0.70 printed for --output-format json
// (live 2026-10-28): one pretty-printed object, ids and costs replaced.
const liveReply = `{
  "text": "the answer",
  "stopReason": "end_turn",
  "sessionId": "` + sid + `",
  "requestId": "<id>",
  "cwd": "/w",
  "thought": "THOUGHT-NOT-THE-ANSWER",
  "usage": {
    "input_tokens": 100,
    "cache_read_input_tokens": 0,
    "cache_creation_input_tokens": 0,
    "output_tokens": 20,
    "reasoning_tokens": 7,
    "total_tokens": 127
  },
  "num_turns": 2,
  "total_cost_usd": 0.01,
  "total_cost_usd_ticks": 100000,
  "modelUsage": {
    "grok-4.7-build": {
      "input_tokens": 100,
      "output_tokens": 20
    }
  }
}
`

// TestParseLiveReplyShape (T6740): the multi-line reply parses, text is
// the answer and never thought, and a one-line object after other output
// still parses.
func TestParseLiveReplyShape(t *testing.T) {
	tests := []struct {
		name, stdout, text string
		tokens             int64
	}{
		{"pretty object", liveReply, "the answer", 120},
		{"thought only", strings.Replace(liveReply, `"text": "the answer",`, `"text": "",`, 1), "", 120},
		{"one line after a notice", "notice: something\n" + `{"sessionId":"` + sid + `","text":"x"}` + "\n", "x", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := New().ParseHeadless(adapter.HeadlessRequest{Kind: adapter.News}, adapter.Output{Stdout: []byte(tt.stdout)})
			if err != nil || res.SessionID != sid || res.Text != tt.text || res.Tokens != tt.tokens || res.Error != adapter.ErrNone {
				t.Errorf("ParseHeadless = %+v, %v", res, err)
			}
		})
	}
	// Through the fake, which prints the same shape.
	h := fakecli.NewHarness(t, fakecli.Build(t))
	a := sandboxed()
	req := adapter.HeadlessRequest{Kind: adapter.News, PromptFile: h.Prompt, Cwd: h.Cwd, IsolatedCwd: true}
	c, err := a.Headless(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := h.Run(t, c, `{"json":{"text":"from the fake"}}`)
	if n := strings.Count(strings.TrimSpace(string(out.Stdout)), "\n"); n < 10 {
		t.Fatalf("the fake printed %d lines, want a pretty object", n+1)
	}
	res, err := a.ParseHeadless(req, out)
	if err != nil || res.Text != "from the fake" || strings.Contains(res.Text, "THOUGHT") {
		t.Errorf("ParseHeadless = %+v, %v", res, err)
	}
}

// TestSandboxNotAppliedFailsClosed (G32-1): a sandboxed run whose stderr
// says Grok ran without its sandbox is ErrLockdownRejected, whatever it
// printed; the same text in the reply, or on an unsandboxed kind, is not.
func TestSandboxNotAppliedFailsClosed(t *testing.T) {
	for _, msg := range []string{
		"WARN Sandbox not supported on this platform, continuing without sandbox details=no landlock",
		"WARN Sandbox could not be applied, continuing without sandbox profile=strict",
		"INFO Sandbox enforcement unavailable (built without 'enforce' feature)",
	} {
		out := adapter.Output{Stdout: []byte(liveReply), Stderr: []byte("ok\n" + msg + "\n")}
		for _, k := range []adapter.Kind{adapter.Review, adapter.Research, adapter.News, adapter.Image, adapter.Video, adapter.Edit} {
			res, err := sandboxed().ParseHeadless(adapter.HeadlessRequest{Kind: k}, out)
			if usesSandbox(k) {
				if !errors.Is(err, adapter.ErrLockdownRejected) || res.Error != adapter.ErrLockdown || res.Text != "" {
					t.Errorf("%s, %q: %+v, %v", k, msg, res, err)
				}
			} else if err != nil {
				t.Errorf("%s (no sandbox asked) with %q: %v", k, msg, err)
			}
		}
	}
	quoted := strings.Replace(liveReply, `"the answer"`, `"the docs say: continuing without sandbox"`, 1)
	if res, err := sandboxed().ParseHeadless(adapter.HeadlessRequest{Kind: adapter.Review}, adapter.Output{Stdout: []byte(quoted)}); err != nil || res.Error != adapter.ErrNone {
		t.Errorf("the message in the reply text failed the run: %+v, %v", res, err)
	}
}

// TestFirmlinkDeny (G32-9): on darwin the sandboxed kinds also deny the
// /System/Volumes/Data firmlink spelling, which read a ~/Library canary
// past the lexical ~/Library rule live; elsewhere there is no such path.
func TestFirmlinkDeny(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		a := sandboxed()
		a.GOOS = goos
		for _, k := range []adapter.Kind{adapter.Review, adapter.Research, adapter.News, adapter.Image, adapter.Video} {
			c, err := a.Headless(adapter.HeadlessRequest{Kind: k, PromptFile: "/p/prompt.md", Cwd: "/w", IsolatedCwd: true, FileTools: true})
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Join(c.Args, " ")
			has := strings.Contains(args, "--deny Read(/System/Volumes/Data/**) --deny Grep(/System/Volumes/Data/**)")
			if has != (goos == "darwin") || !strings.Contains(args, "--deny Read(/home/u/Library/**) --deny Grep(/home/u/Library/**)") {
				t.Errorf("%s %s: %q", goos, k, c.Args)
			}
			// Case folding is for APFS only.
			if folded := strings.Contains(args, "[Ll][Ii]"); folded != (goos == "darwin") {
				t.Errorf("%s %s: case-folded rules %v", goos, k, folded)
			}
		}
	}
}

// TestGrokHomeDeny (T6742): every sandboxed kind, and image and video
// with or without strict, denies Read and Grep of Grok's own home (its
// auth file lives there), and of ~/.grok too when GROK_HOME points
// elsewhere; never a write rule. A Grok home that cannot be named in a
// rule refuses the kind.
func TestGrokHomeDeny(t *testing.T) {
	env := func(grokHome string) Adapter {
		a := sandboxed()
		a.Getenv = func(k string) string {
			switch k {
			case "HOME":
				return "/home/u"
			case "GROK_HOME":
				return grokHome
			}
			return ""
		}
		return a
	}
	denyKinds := []adapter.Kind{adapter.Review, adapter.Research, adapter.News, adapter.Image, adapter.Video}
	for _, k := range []adapter.Kind{adapter.Review, adapter.Research, adapter.News, adapter.Image, adapter.Video, adapter.Edit} {
		if usesDeny(k) != slices.Contains(denyKinds, k) {
			t.Fatalf("usesDeny(%s) = %v", k, usesDeny(k))
		}
	}
	tests := []struct {
		grokHome string
		want     []string
	}{
		{"", []string{"/home/u/.grok"}},
		{"/home/u/.grok", []string{"/home/u/.grok"}},
		{"/data/grok/", []string{"/data/grok", "/home/u/.grok"}},
	}
	for _, tt := range tests {
		for _, k := range denyKinds {
			req := adapter.HeadlessRequest{Kind: k, PromptFile: "/p/prompt.md", Cwd: "/w", IsolatedCwd: true, FileTools: true}
			c, err := env(tt.grokHome).Headless(req)
			if err != nil {
				t.Fatalf("%s, GROK_HOME %q: %v", k, tt.grokHome, err)
			}
			var want []string
			for _, h := range tt.want {
				want = append(want, "--deny", "Read("+h+"/**)", "--deny", "Grep("+h+"/**)")
			}
			args := strings.Join(c.Args, " ")
			if !strings.Contains(args, strings.Join(want, " ")) || strings.Contains(args, "Write(") || strings.Contains(args, "Edit(") {
				t.Errorf("%s, GROK_HOME %q: %q, want %q", k, tt.grokHome, c.Args, want)
			}
		}
	}
	// Edit takes no sandbox and so no deny.
	c, err := env("").Headless(adapter.HeadlessRequest{Kind: adapter.Edit, PromptFile: "/p/prompt.md", Cwd: "/w"})
	if err != nil || slices.Contains(c.Args, "--deny") {
		t.Errorf("edit: %q, %v", c.Args, err)
	}
	for _, bad := range []string{"rel/grok", "/", "/g*", "/g,h", "/g)"} {
		for _, k := range denyKinds {
			req := adapter.HeadlessRequest{Kind: k, PromptFile: "/p/prompt.md", Cwd: "/w", IsolatedCwd: true, FileTools: true}
			c, err := env(bad).Headless(req)
			if usesSandbox(k) != errors.Is(err, adapter.ErrNoCapability) || (!usesSandbox(k) && (err != nil || slices.Contains(c.Args, "--deny"))) {
				t.Errorf("%s, GROK_HOME %q: %q, %v", k, bad, c.Args, err)
			}
		}
	}
}

// TestPhysicalDeny (N54-3): a HOME or Grok home reached through a symlink
// is denied in its physical form too; a physical path that cannot be
// found or named refuses a sandboxed kind.
func TestPhysicalDeny(t *testing.T) {
	a := sandboxed()
	a.GOOS = "linux"
	a.Realpath = func(p string) (string, error) { return strings.Replace(p, "/home/u", "/mnt/home/u", 1), nil }
	c, err := a.Headless(adapter.HeadlessRequest{Kind: adapter.Review, PromptFile: "/p/prompt.md", Cwd: "/w"})
	want := denyArgs(slices.Concat([]string{"/home/u/Library/**", "/home/u/.grok/**", "/mnt/home/u/Library/**", "/mnt/home/u/.grok/**"}, spellingPats)...)
	if err != nil || !strings.Contains(strings.Join(c.Args, " "), strings.Join(want, " ")) {
		t.Errorf("argv %q, %v\nwant %q", c.Args, err, want)
	}
	for _, rp := range []func(string) (string, error){
		func(string) (string, error) { return "", errors.New("loop") },
		func(string) (string, error) { return "/mnt/*", nil },
	} {
		a.Realpath = rp
		if _, err := a.Headless(adapter.HeadlessRequest{Kind: adapter.Review, PromptFile: "/p/prompt.md", Cwd: "/w"}); !errors.Is(err, adapter.ErrNoCapability) {
			t.Errorf("review: %v", err)
		}
		if c, err := a.Headless(adapter.HeadlessRequest{Kind: adapter.Image, PromptFile: "/p/prompt.md", Cwd: "/w"}); err != nil || slices.Contains(c.Args, "--deny") {
			t.Errorf("image: %q, %v", c.Args, err)
		}
	}
}

// TestDenyPatternShape: every deny pattern compiles the way Grok's glob
// crate wants (a ** is a whole path component, or Pattern::new fails and
// the rule silently matches nothing), and holds no rule syntax.
func TestDenyPatternShape(t *testing.T) {
	c, err := sandboxed().Headless(adapter.HeadlessRequest{Kind: adapter.Review, PromptFile: "/p/prompt.md", Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	for i, arg := range c.Args {
		if arg != "--deny" {
			continue
		}
		rule := c.Args[i+1]
		pat := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(rule, "Read("), "Grep("), ")")
		if strings.ContainsAny(pat, "(),\\") || pat == rule {
			t.Errorf("rule %q", rule)
		}
		for j := strings.Index(pat, "**"); j >= 0; j = next(pat, j) {
			if (j > 0 && pat[j-1] != '/') || (j+2 < len(pat) && pat[j+2] != '/') {
				t.Errorf("** not a whole component in %q", pat)
			}
		}
	}
}

func next(s string, j int) int {
	k := strings.Index(s[j+2:], "**")
	if k < 0 {
		return -1
	}
	return j + 2 + k
}

// TestLeadChars: the class holds the quotes and every rune Rust's
// str::trim strips (Unicode White_Space, which Go's unicode table shares).
func TestLeadChars(t *testing.T) {
	in := func(r rune) bool {
		rs := []rune(leadChars)
		for i := 0; i < len(rs); i++ {
			if i+2 < len(rs) && rs[i+1] == '-' {
				if r >= rs[i] && r <= rs[i+2] {
					return true
				}
				i += 2
				continue
			}
			if rs[i] == r {
				return true
			}
		}
		return false
	}
	for r := rune(0); r <= 0x10FFFF; r++ {
		want := r == '"' || r == '\'' || unicode.Is(unicode.White_Space, r)
		if in(r) != want {
			t.Errorf("rune %U in class: %v, want %v", r, in(r), want)
		}
	}
}

// TestSandboxRefusalFailsClosed (G32-8): grok that could not apply its
// sandbox profile refuses to start (live, nested in sandbox-exec); every
// kind reports it as a lockdown failure that names the likely cause.
func TestSandboxRefusalFailsClosed(t *testing.T) {
	out := adapter.Output{ExitCode: 1, Stderr: []byte("sandbox initialization failed: Operation not permitted\n" +
		"error: could not apply the 'strict' sandbox profile; see the warning above for the cause. Refusing to start with its protections missing.\n")}
	for _, k := range []adapter.Kind{adapter.Review, adapter.Research, adapter.News, adapter.Image} {
		res, err := sandboxed().ParseHeadless(adapter.HeadlessRequest{Kind: k}, out)
		if !errors.Is(err, adapter.ErrLockdownRejected) || res.Error != adapter.ErrLockdown || !strings.Contains(err.Error(), "inside another sandbox") {
			t.Errorf("%s: %+v, %v", k, res, err)
		}
	}
}
