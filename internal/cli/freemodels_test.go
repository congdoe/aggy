package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
		{"on", "n\n", 1, true, false}, // a no records nothing: the note again
		{"on", "y\n", 0, true, true},  // the note, then yes, which records it
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
		// M2: the note and the question say paid models come too.
		if s.note {
			for _, w := range []string{"only its free models", "paid models", "spend your OpenCode credit", "OpenCode's and each model provider's terms"} {
				if !strings.Contains(out, w) {
					t.Errorf("step %d: the note lacks %q: %q", i, w, out)
				}
			}
		}
		if s.answer != "" && (!strings.Contains(out, "free and paid") || !strings.Contains(out, "OpenCode credit)? [y/N]")) {
			t.Errorf("step %d: the question does not name paid models: %q", i, out)
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

// TestConfigSetOtherSpelling (PR #128 review, M1): a file that spells a key
// another way than the line edit finds is never reported as "already" the
// value asked while aggy reads another; the set is refused with
// config.edit_by_hand and the file is unchanged. "Already" goes by the value
// aggy reads.
func TestConfigSetOtherSpelling(t *testing.T) {
	cases := []struct {
		name, text, key, val string
		already              bool
	}{
		{"dotted key", "opencode.free_models = true\n", "opencode-free-models", "off", false},
		{"inline table", "opencode = { free_models = true }\n", "opencode-free-models", "off", false},
		{"another case", "[opencode]\nFree_Models = true\n", "opencode-free-models", "off", false},
		{"dotted harness", "vendors.codex.off = true\n", "harness.codex", "on", false},
		{"inline commands", "commands = { off = true }\n", "commands", "on", false},
		{"dotted, already on", "opencode.free_models = true\n", "opencode-free-models", "on", true},
		{"explicit false is already off", "[opencode]\nfree_models = false\n", "opencode-free-models", "off", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			path, err := config.Path()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(c.text), 0o600); err != nil {
				t.Fatal(err)
			}
			atKeyboard(t, "y\n")
			code, out, errb := run("config", "set", c.key, c.val)
			if c.already {
				if code != 0 || !strings.Contains(out, "already") {
					t.Errorf("%d %q %q, want already", code, out, errb)
				}
				return
			}
			if strings.Contains(out, "already") {
				t.Errorf("said already while aggy reads the old value: %q", out)
			}
			if code != 1 || !strings.Contains(errb, "edit that line by hand") || !strings.Contains(errb, "[config.edit_by_hand · run ") {
				t.Errorf("%d %q %q, want a refusal naming the file", code, out, errb)
			}
			if b, _ := os.ReadFile(path); string(b) != c.text {
				t.Errorf("the file changed: %q", b)
			}
		})
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
