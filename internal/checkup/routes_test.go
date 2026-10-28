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
		{"grok media keys", "",
			"[toolset.image_gen]\nbase_url = \"https://" + secret + "\"\n[toolset.video_gen]\nenv_key = \"XAI_API_KEY\"\nbase_url = \"\"\n",
			[]string{"route-config grok ~/.grok/config.toml: toolset.image_gen.base_url is set", "route-config grok ~/.grok/config.toml: toolset.video_gen.env_key is set"}},
		{"grok toolset odd shape", "", "toolset = 3\n", nil},
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
				line := f.Code + " " + string(f.Vendor) + " " + f.Message
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
	write(t, filepath.Join(gdir, "config.toml"), "[toolset.video_gen]\nbase_url = \"u\"\n")
	write(t, filepath.Join(home, ".claude", "settings.json"), `{"env":{"OPENAI_BASE_URL":"u"}}`)
	got := RouteFiles([]string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + cdir, "GROK_HOME=" + gdir})
	if len(got) != 2 || !strings.Contains(got[0].Message, "env.ANTHROPIC_BASE_URL") || !strings.Contains(got[1].Message, "toolset.video_gen.base_url") {
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
	if len(got) != 1 || got[0].Message != "~/.claude/settings.local.json: env.ANTHROPIC_BASE_URL is set: Claude Code may send its requests to another endpoint, which can bill an API key or sit in the middle of plan traffic" {
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
			if len(got) != 1 || !strings.HasPrefix(got[0].Message, tt.want) || got[0].Level != tt.level {
				t.Errorf("findings = %+v, want %q", got, tt.want)
			}
		})
	}
}
