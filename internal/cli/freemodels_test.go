package cli

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/store"
)

// TestConfigFreeModels (T6956): aggy config shows and changes
// opencode-free-models; a dry run writes nothing; turning it on asks, after a
// note said once; off needs no question.
func TestConfigFreeModels(t *testing.T) {
	isolate(t)
	inRepo(t)
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	index, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	listed := func(want string) {
		t.Helper()
		_, out, _ := run("config", "list")
		if !strings.Contains(out, "opencode-free-models "+want) {
			t.Errorf("list lacks opencode-free-models %s:\n%s", want, out)
		}
	}
	listed("off")

	code, out, _ := run("config", "set", "opencode-free-models", "on", "--dry-run")
	if code != 0 || !strings.Contains(out, "free_models = true") || strings.Contains(out, "OpenCode's own service") {
		t.Errorf("dry run: %d %q", code, out)
	}
	for _, p := range []string{path, index} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the dry run made %s: %v", p, err)
		}
	}

	code, _, errb := run("config", "set", "opencode-free-models", "on")
	if code != 1 || !strings.Contains(errb, "[permissions.not_at_keyboard · run ") {
		t.Errorf("no terminal: %d %q", code, errb)
	}

	steps := []struct {
		val, answer string
		code        int
		note        bool
		on          bool
	}{
		{"on", "n\n", 1, true, false}, // the note, then no
		{"on", "y\n", 0, false, true}, // asked again, the note not repeated
		{"off", "", 0, false, false},  // off loosens nothing: no question
		{"on", "y\n", 0, false, true}, // still no note
		{"on", "", 0, false, true},    // already on
		{"maybe", "", 1, false, true}, // a bad value
		{"off", "", 0, false, false},
	}
	for i, s := range steps {
		atKeyboard(t, s.answer)
		code, out, errb := run("config", "set", "opencode-free-models", s.val)
		if code != s.code {
			t.Fatalf("step %d (%s): %d %q %q", i, s.val, code, out, errb)
		}
		if got := strings.Contains(out, "OpenCode's own service"); got != s.note {
			t.Errorf("step %d (%s): note shown = %v, want %v: %q", i, s.val, got, s.note, out)
		}
		if s.val == "maybe" && !strings.Contains(errb, "[config.bad_value · run ") {
			t.Errorf("step %d: %q", i, errb)
		}
		cfg, err := config.LoadDefault()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.OpenCode.FreeModels != s.on {
			t.Errorf("step %d (%s): free_models = %v, want %v", i, s.val, cfg.OpenCode.FreeModels, s.on)
		}
		if s.on {
			listed("on")
		} else {
			listed("off")
		}
	}
}

func TestOpenCodeFreeWarns(t *testing.T) {
	on := config.Default()
	on.OpenCode.FreeModels = true
	cases := []struct {
		name string
		user config.Config
		repo string
		want []string // substrings, one per line, in order
	}{
		{"off, no repo setting", config.Default(), "", nil},
		{"on, allowed", on, "", nil},
		{"repo sets it: ignored", config.Default(), "[opencode]\nfree_models = true\n", []string{"aggy ignores"}},
		{"on, repo refuses", on, `allowed_vendors = ["opencode", "codex"]`, []string{"does not list opencode-free"}},
		{"on, repo sets it and refuses", on, "allowed_vendors = [\"opencode\", \"codex\"]\n[opencode]\nfree_models = true\n",
			[]string{"aggy ignores", "does not list opencode-free"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc, err := config.ParseRepo(c.repo)
			if err != nil {
				t.Fatal(err)
			}
			e := config.Merge(c.user, rc)
			got := openCodeFreeWarns(e, e.OpenCodeProviders())
			if len(got) != len(c.want) {
				t.Fatalf("warns = %q, want %q", got, c.want)
			}
			for i, w := range c.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("warn %d = %q, want %q", i, got[i], w)
				}
			}
			if rc.SetsFreeModels() && !c.user.OpenCode.FreeModels && slices.Contains(e.OpenCodeProviders(), config.OpenCodeFreeProvider) {
				t.Error("a repo's free_models turned it on")
			}
		})
	}
}
