package checkup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

func TestBaseURLVar(t *testing.T) {
	tests := []struct {
		name string
		want adapter.Vendor
	}{
		{"ANTHROPIC_BASE_URL", adapter.Claude},
		{"ANTHROPIC_BEDROCK_BASE_URL", adapter.Claude},
		{"OPENAI_BASE_URL", adapter.Codex},
		{"CODEX_API_BASE_URL", adapter.Codex},
		{"GROK_MODELS_BASE_URL", adapter.Grok},
		{"GROK_XAI_API_BASE_URL", adapter.Grok},
		{"XAI_BASE_URL", adapter.Grok},
		{"GEMINI_BASE_URL", ""},
		{"OPENAI_BASE_URL_X", ""},
		{"openai_base_url", ""},
		{"BASE_URL", ""},
	}
	for _, tt := range tests {
		v, ok := BaseURLVar(tt.name)
		if v != tt.want || ok != (tt.want != "") {
			t.Errorf("BaseURLVar(%s) = %q, %v", tt.name, v, ok)
		}
	}
}

func TestRouteEnv(t *testing.T) {
	env := []string{
		"OPENAI_BASE_URL=http://" + secret, "GROK_MODELS_BASE_URL=http://x", "ANTHROPIC_BASE_URL=",
		"XAI_BASE_URL=http://y", "XAI_BASE_URL=", // the last entry wins: unset
		"CODEX_X_BASE_URL=http://z", "OTHER_BASE_URL=http://o",
	}
	got := RouteEnv(env)
	var names []string
	for _, f := range got {
		names = append(names, strings.Fields(f.Message)[0])
		if f.Code != CodeBaseURLEnv || f.Level != LevelWarn || strings.Contains(f.Message, secret) || strings.Contains(f.Message, "http") {
			t.Errorf("finding = %+v", f)
		}
	}
	if want := []string{"CODEX_X_BASE_URL", "GROK_MODELS_BASE_URL", "OPENAI_BASE_URL"}; !slices.Equal(names, want) {
		t.Errorf("flagged %v, want %v", names, want)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := safefs.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := safefs.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRouteFiles(t *testing.T) {
	tests := []struct {
		name   string
		claude string // ~/.claude/settings.json, "" for none
		grok   string // ~/.grok/config.toml
		want   []string
	}{
		{"no files", "", "", nil},
		{"clean files", `{"env":{"FOO":"1"},"model":"x"}`, "[toolset.image_gen]\nenabled = true\n", nil},
		{"claude env routes",
			`{"env":{"ANTHROPIC_BASE_URL":"http://` + secret + `","ANTHROPIC_API_KEY":"` + secret + `","OPENAI_BASE_URL":"","XAI_API_KEY":"k","PATH":"/x"}}`, "",
			[]string{"route-config claude ~/.claude/settings.json: env.ANTHROPIC_API_KEY is set", "route-config claude ~/.claude/settings.json: env.ANTHROPIC_BASE_URL is set", "route-config claude ~/.claude/settings.json: env.XAI_API_KEY is set"}},
		{"claude env not an object", `{"env":"x"}`, "", nil},
		{"claude bad json", `{"env":`, "", []string{"config-unreadable claude ~/.claude/settings.json: cannot be checked"}},
		{"claude BOM", "\ufeff{\"env\":{\"OPENAI_BASE_URL\":\"u\"}}", "", []string{"route-config claude ~/.claude/settings.json: env.OPENAI_BASE_URL is set"}},
		{"claude apiKeyHelper and cloud switch (T6611 review 7)",
			`{"apiKeyHelper":"/bin/` + secret + `","env":{"CLAUDE_CODE_USE_VERTEX":"1","ANTHROPIC_AUTH_TOKEN":"` + secret + `"}}`, "",
			[]string{"route-config claude ~/.claude/settings.json: apiKeyHelper is set", "route-config claude ~/.claude/settings.json: env.ANTHROPIC_AUTH_TOKEN is set", "route-config claude ~/.claude/settings.json: env.CLAUDE_CODE_USE_VERTEX is set: Claude Code may use Google Vertex AI"}},
		{"grok endpoints and BYOK models (T6611 D)", "",
			"[endpoints]\nxai_api_base_url = \"https://" + secret + "\"\nmodels_base_url = \"u\"\n[model.mine]\nbase_url = \"u\"\napi_key = \"" + secret + "\"\nenv_key = \"\"\n",
			[]string{"route-config grok ~/.grok/config.toml: endpoints.xai_api_base_url is set: Grok, and its image and video tools,",
				"route-config grok ~/.grok/config.toml: endpoints.models_base_url is set",
				"route-config grok ~/.grok/config.toml: model.mine.base_url is set",
				"route-config grok ~/.grok/config.toml: model.mine.api_key is set"}},
		{"grok toolset keys do not exist and are not flagged", "",
			"[toolset.image_gen]\nbase_url = \"u\"\nenv_key = \"XAI_API_KEY\"\n", nil},
		{"grok odd shapes", "", "endpoints = 3\nmodel = \"x\"\n", nil},
		{"grok bad toml", "", "[toolset\n", []string{"config-unreadable grok ~/.grok/config.toml: cannot be checked"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			if tt.claude != "" {
				write(t, filepath.Join(home, ".claude", "settings.json"), tt.claude)
			}
			if tt.grok != "" {
				write(t, filepath.Join(home, ".grok", "config.toml"), tt.grok)
			}
			got := RouteFiles([]string{"HOME=" + home})
			if len(got) != len(tt.want) {
				t.Fatalf("findings = %+v, want %v", got, tt.want)
			}
			for i, f := range got {
				line := f.Code + " " + string(f.Vendor) + " " + f.Text(home)
				if !strings.HasPrefix(line, tt.want[i]) {
					t.Errorf("finding %d = %q, want prefix %q", i, line, tt.want[i])
				}
				if strings.Contains(f.Message, secret) || strings.Contains(f.Message, home) {
					t.Errorf("finding leaks a value or the home path: %q", f.Message)
				}
			}
		})
	}
}

// TestRouteFilesRelocated reads the relocated homes instead of ~, and
// never a relative one.
func TestRouteFilesRelocated(t *testing.T) {
	home := t.TempDir()
	cdir, gdir := filepath.Join(t.TempDir(), "c"), filepath.Join(t.TempDir(), "g")
	write(t, filepath.Join(cdir, "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"u"}}`)
	write(t, filepath.Join(gdir, "config.toml"), "[endpoints]\nxai_api_base_url = \"u\"\n")
	write(t, filepath.Join(home, ".claude", "settings.json"), `{"env":{"OPENAI_BASE_URL":"u"}}`)
	got := RouteFiles([]string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + cdir, "GROK_HOME=" + gdir})
	if len(got) != 2 || !strings.Contains(got[0].Message, "env.ANTHROPIC_BASE_URL") || !strings.Contains(got[1].Message, "endpoints.xai_api_base_url") {
		t.Errorf("findings = %+v", got)
	}
	if got := RouteFiles([]string{"HOME=" + home, "CLAUDE_CONFIG_DIR=rel"}); len(got) != 0 {
		t.Errorf("a relative CLAUDE_CONFIG_DIR was read: %+v", got)
	}
}

// TestRouteFilesLocalSettings (T6611 review 7): settings.local.json is
// checked like settings.json.
func TestRouteFilesLocalSettings(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude", "settings.local.json"), `{"env":{"ANTHROPIC_BASE_URL":"u"}}`)
	got := RouteFiles([]string{"HOME=" + home})
	if len(got) != 1 || got[0].Text(home) != "~/.claude/settings.local.json: env.ANTHROPIC_BASE_URL is set: Claude Code may send its requests to another endpoint, which can bill an API key or sit in the middle of plan traffic" {
		t.Errorf("findings = %+v", got)
	}
}

// TestRouteFilesSymlinks (T6611 review 6): a linked config file, as
// dotfile managers make, is read through its target; a link to a
// credential file or to nothing is a warning, and never read.
func TestRouteFilesSymlinks(t *testing.T) {
	tests := []struct {
		name   string
		target func(t *testing.T, home string) string // the link's target
		want   string                                 // prefix of the one finding
		level  Level
	}{
		{"linked settings", func(t *testing.T, _ string) string {
			p := filepath.Join(t.TempDir(), "dotfiles", "settings.json")
			write(t, p, `{"env":{"OPENAI_BASE_URL":"u"}}`)
			return p
		}, "~/.claude/settings.json: env.OPENAI_BASE_URL is set", LevelWarn},
		{"link to a credential file", func(t *testing.T, home string) string {
			p := filepath.Join(home, ".codex", "auth.json")
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(`{"env":{"OPENAI_BASE_URL":"u"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}, "~/.claude/settings.json: cannot be checked for base URLs and API keys (a credential file)", LevelWarn},
		{"dangling link", func(t *testing.T, _ string) string {
			return filepath.Join(t.TempDir(), "gone.json")
		}, "~/.claude/settings.json: cannot be checked for base URLs and API keys (a symlink whose target cannot be found)", LevelWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			link := filepath.Join(home, ".claude", "settings.json")
			if err := safefs.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tt.target(t, home), link); err != nil {
				t.Fatal(err)
			}
			got := RouteFiles([]string{"HOME=" + home})
			if len(got) != 1 || !strings.HasPrefix(got[0].Text(home), tt.want) || got[0].Level != tt.level {
				t.Errorf("findings = %+v, want %q", got, tt.want)
			}
		})
	}
}

// TestRouteFilesCodex (T6611 D): Codex's config.toml, in $CODEX_HOME or
// ~/.codex, is checked for openai_base_url and model provider base URLs;
// auth.json next to it is never read.
func TestRouteFilesCodex(t *testing.T) {
	tests := []struct {
		name, config string
		relocate     bool
		want         []string
	}{
		{"clean", "model = \"gpt-5\"\n[mcp_servers.docs]\ncommand = \"npx\"\n", false, nil},
		{"openai_base_url", "openai_base_url = \"https://" + secret + "\"\n", false,
			[]string{"~/.codex/config.toml: openai_base_url is set: Codex may send its requests to another endpoint"}},
		{"model providers", "model_provider = \"proxy\"\n[model_providers.proxy]\nbase_url = \"u\"\nenv_key = \"K\"\n[model_providers.other]\nname = \"x\"\n", false,
			[]string{"~/.codex/config.toml: model_providers.proxy.base_url is set"}},
		{"relocated", "openai_base_url = \"u\"\n", true, []string{"openai_base_url is set"}},
		{"bad toml", "openai_base_url = \n", false, []string{"~/.codex/config.toml: cannot be checked"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, ".codex")
			env := []string{"HOME=" + home}
			if tt.relocate {
				dir = filepath.Join(t.TempDir(), "ch")
				env = append(env, "CODEX_HOME="+dir)
			}
			write(t, filepath.Join(dir, "config.toml"), tt.config)
			if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte("openai_base_url = \"u\""), 0o600); err != nil {
				t.Fatal(err)
			}
			got := RouteFiles(env)
			if len(got) != len(tt.want) {
				t.Fatalf("findings = %+v, want %v", got, tt.want)
			}
			for i, f := range got {
				if f.Vendor != adapter.Codex || !strings.Contains(f.Text(home), tt.want[i]) || strings.Contains(f.Text(home), secret) || strings.Contains(f.Text(home), "auth.json") {
					t.Errorf("finding %d = %q, want %q", i, f.Text(home), tt.want[i])
				}
			}
		})
	}
}

// TestRouteEnvOpenAI (T6611 D): Codex does not read OPENAI_BASE_URL, so
// it is flagged without naming Codex as the reader.
func TestRouteEnvOpenAI(t *testing.T) {
	got := RouteEnv([]string{"OPENAI_BASE_URL=u", "GROK_XAI_API_BASE_URL=u"})
	if len(got) != 2 || got[0].Vendor != adapter.Grok || got[1].Vendor != "" ||
		!strings.HasPrefix(got[1].Message, "OPENAI_BASE_URL is set: Codex does not read it, but other OpenAI tools may") {
		t.Errorf("findings = %+v", got)
	}
}
