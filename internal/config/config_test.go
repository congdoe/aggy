package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/safefs"
)

func TestDefaultIsValid(t *testing.T) {
	d := Default()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if d.DefaultVendor != "" {
		t.Errorf("default vendor = %q, want empty (generic)", d.DefaultVendor)
	}
	for _, v := range adapter.Vendors() {
		if m := d.Vendors.For(v).Model; m != "" {
			t.Errorf("%s model = %q, want empty", v, m)
		}
	}
	for _, k := range adapter.Kinds() {
		r := d.Routing.For(k)
		if len(r.Vendors) == 0 && !r.NotAuthor {
			t.Errorf("kind %s has no default route", k)
		}
		if d.Budgets.For(k).Minutes == 0 {
			t.Errorf("kind %s has no default time limit", k)
		}
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		check   func(t *testing.T, c Config)
		wantErr string
	}{
		{
			name:  "empty gives defaults",
			text:  "",
			check: func(t *testing.T, c Config) { equal(t, c, Default()) },
		},
		{
			name: "full proposal example",
			text: `
default_vendor = "claude"

[vendors.claude]
model = "opus"

[vendors.grok]
model = "grok-4.7"

[switch]
order = ["codex", "grok"]

[routing]
news     = ["grok", "codex"]
research = ["codex", "grok"]
image    = ["codex", "grok"]
video    = ["grok"]
review   = "not-author"
edit     = ["codex", "grok"]

[budgets]
research = { minutes = 10, turns = 30 }
news     = { minutes = 5, turns = 15 }
image    = { minutes = 5 }
`,
			check: func(t *testing.T, c Config) {
				if c.DefaultVendor != adapter.Claude {
					t.Errorf("default_vendor = %q", c.DefaultVendor)
				}
				if c.Vendors.Claude.Model != "opus" || c.Vendors.Grok.Model != "grok-4.7" || c.Vendors.Codex.Model != "" {
					t.Errorf("vendors = %+v", c.Vendors)
				}
				equal(t, c.Switch.Order, []adapter.Vendor{adapter.Codex, adapter.Grok})
				if !c.Routing.Review.NotAuthor {
					t.Error("review is not not-author")
				}
			},
		},
		{
			name: "partial override keeps other defaults",
			text: "[routing]\nnews = [\"codex\"]\n[budgets]\nresearch = { minutes = 20 }\n",
			check: func(t *testing.T, c Config) {
				equal(t, c.Routing.News.Vendors, []adapter.Vendor{adapter.Codex})
				equal(t, c.Routing.Research, Default().Routing.Research)
				equal(t, c.Budgets.Research, Budget{Minutes: 20, Turns: 30})
				equal(t, c.Budgets.News, Default().Budgets.News)
			},
		},
		{
			name: "empty list turns a kind off",
			text: "[routing]\nvideo = []\n",
			check: func(t *testing.T, c Config) {
				r := c.Routing.Video
				if len(r.Vendors) != 0 || r.NotAuthor {
					t.Errorf("video = %+v", r)
				}
			},
		},
		{
			name: "list replaces not-author",
			text: "[routing]\nreview = [\"grok\"]\n",
			check: func(t *testing.T, c Config) {
				equal(t, c.Routing.Review, Route{Vendors: []adapter.Vendor{adapter.Grok}})
			},
		},
		{name: "unknown top-level key", text: "colour = \"red\"\n", wantErr: "unknown key colour"},
		{name: "unknown kind", text: "[routing]\nchat = [\"grok\"]\n", wantErr: "unknown key routing.chat"},
		{name: "unknown vendor section", text: "[vendors.gemini]\nmodel = \"x\"\n", wantErr: "unknown key vendors.gemini"},
		{name: "unknown vendor field", text: "[vendors.codex]\nmodle = \"x\"\n", wantErr: "unknown key vendors.codex.modle"},
		{name: "unknown budget field", text: "[budgets]\nnews = { minutes = 5, tokens = 9 }\n", wantErr: "unknown key budgets.news.tokens"},
		{name: "several unknown keys", text: "a = 1\nb = 2\n", wantErr: "unknown keys a, b"},
		{name: "bad default vendor", text: "default_vendor = \"gemini\"\n", wantErr: "default_vendor: unknown vendor \"gemini\""},
		{name: "bad switch vendor", text: "[switch]\norder = [\"gpt\"]\n", wantErr: "switch.order: unknown vendor \"gpt\""},
		{name: "duplicate switch vendor", text: "[switch]\norder = [\"grok\", \"grok\"]\n", wantErr: "listed twice"},
		{name: "bad routing vendor", text: "[routing]\nnews = [\"bard\"]\n", wantErr: "unknown vendor \"bard\""},
		{name: "duplicate routing vendor", text: "[routing]\nnews = [\"grok\", \"grok\"]\n", wantErr: "listed twice"},
		{name: "bad routing string", text: "[routing]\nreview = \"anyone\"\n", wantErr: "not-author"},
		{name: "routing wrong type", text: "[routing]\nnews = 3\n", wantErr: "vendor list"},
		{name: "zero minutes", text: "[budgets]\nedit = { minutes = 0 }\n", wantErr: "budgets.edit.minutes must be positive"},
		{name: "negative turns", text: "[budgets]\nnews = { minutes = 5, turns = -1 }\n", wantErr: "budgets.news.turns must not be negative"},
		{name: "syntax error", text: "[routing\n", wantErr: "toml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse(tt.text)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, c)
		})
	}
}

func TestLoad(t *testing.T) {
	t.Run("missing file gives defaults", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv(home.Env, t.TempDir())
		c, err := LoadDefault()
		if err != nil {
			t.Fatal(err)
		}
		equal(t, c, Default())
	})

	t.Run("reads AGGY_HOME", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		dir := t.TempDir()
		t.Setenv(home.Env, dir)
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte("default_vendor = \"grok\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadDefault()
		if err != nil || c.DefaultVendor != adapter.Grok {
			t.Errorf("LoadDefault = %q, %v", c.DefaultVendor, err)
		}
	})

	t.Run("reads ~/.aggy by default", func(t *testing.T) {
		h := t.TempDir()
		t.Setenv("HOME", h)
		t.Setenv(home.Env, "")
		p, err := Path()
		if err != nil || p != filepath.Join(h, ".aggy", "config.toml") {
			t.Errorf("Path = %q, %v", p, err)
		}
	})

	t.Run("error names the file", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		p := filepath.Join(t.TempDir(), FileName)
		if err := os.WriteFile(p, []byte("x = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "unknown key x") {
			t.Errorf("Load error = %v", err)
		}
	})

	t.Run("refuses a credential path", func(t *testing.T) {
		h := t.TempDir()
		t.Setenv("HOME", h)
		t.Setenv("CODEX_HOME", "")
		if _, err := Load(filepath.Join(h, ".codex", "auth.json")); !errors.Is(err, safefs.ErrAuthPath) {
			t.Errorf("Load = %v, want safefs.ErrAuthPath", err)
		}
	})
}

func equal[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
