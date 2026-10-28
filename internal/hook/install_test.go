package hook

import (
	"errors"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

const bin = "/opt/aggy dir/bin/aggy"

func TestMergeKeepsUserHooks(t *testing.T) {
	want := []Entry{{adapter.Claude, "Stop"}, {adapter.Claude, "SessionStart"}}
	src := `{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls)"], "n": 1.50},
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "say done <&>"}]}]
  }
}`
	out, err := Merge(bin, want)([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	wantOut := `{
  "model": "opus",
  "permissions": {
    "allow": [
      "Bash(ls)"
    ],
    "n": 1.50
  },
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "say done <&>"
          }
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "'/opt/aggy dir/bin/aggy' hook --vendor claude Stop",
            "timeout": 10
          }
        ]
      }
    ],
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "'/opt/aggy dir/bin/aggy' hook --vendor claude SessionStart",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
`
	if string(out) != wantOut {
		t.Errorf("got\n%s", out)
	}
	again, err := Merge(bin, want)(out)
	if err != nil || string(again) != string(out) {
		t.Errorf("not idempotent: %v\n%s", err, again)
	}
	if m, err := Missing(out, bin, want); err != nil || len(m) != 0 {
		t.Errorf("Missing after merge = %v, %v", m, err)
	}
	// Already installed: the input comes back byte for byte.
	same := []byte(`{"hooks":{"Stop":[{"hooks":[{"command":"'/opt/aggy dir/bin/aggy' hook --vendor claude Stop"}]}]}}`)
	if got, err := Merge(bin, want[:1])(same); err != nil || string(got) != string(same) {
		t.Errorf("unchanged file rewritten: %s %v", got, err)
	}
	if got, err := Merge(bin, want[:1])(nil); err != nil || !strings.Contains(string(got), `"Stop"`) {
		t.Errorf("new file: %s %v", got, err)
	}
}

func TestMissingRefusesAmbiguity(t *testing.T) {
	want := []Entry{{adapter.Claude, "Stop"}}
	for name, src := range map[string]string{
		"array root":        `[]`,
		"two values":        `{} {}`,
		"duplicate key":     `{"hooks":{},"hooks":{}}`,
		"nested duplicate":  `{"a":{"x":1,"x":2}}`,
		"hooks not object":  `{"hooks":[]}`,
		"event not list":    `{"hooks":{"Stop":{}}}`,
		"group not object":  `{"hooks":{"Stop":["x"]}}`,
		"inner not list":    `{"hooks":{"Stop":[{"hooks":{}}]}}`,
		"hook not object":   `{"hooks":{"Stop":[{"hooks":[1]}]}}`,
		"other aggy binary": `{"hooks":{"Stop":[{"hooks":[{"command":"'/old/aggy' hook --vendor claude Stop"}]}]}}`,
		"bare other aggy":   `{"hooks":{"Stop":[{"hooks":[{"command":"/usr/bin/aggy hook --vendor claude Stop"}]}]}}`,
		"broken":            `{"hooks":`,
		"too deep":          strings.Repeat("[", 300) + strings.Repeat("]", 300),
	} {
		if _, err := Missing([]byte(src), bin, want); !errors.Is(err, ErrAmbiguous) {
			t.Errorf("%s: err %v", name, err)
		}
		if out, err := Merge(bin, want)([]byte(src)); err == nil {
			t.Errorf("%s: merged into\n%s", name, out)
		}
	}
	// Another vendor's or event's aggy hook is not ours to judge.
	ok := `{"hooks":{"Stop":[{"hooks":[{"command":"'/old/aggy' hook --vendor grok Stop"}]}]}}`
	if m, err := Missing([]byte(ok), bin, want); err != nil || len(m) != 1 {
		t.Errorf("Missing = %v, %v", m, err)
	}
}

func TestCheckBin(t *testing.T) {
	for _, b := range []string{"aggy", "/a/../aggy", "/a/it's/aggy", "/a/b\n/aggy", ""} {
		if CheckBin(b) == nil {
			t.Errorf("CheckBin(%q) passed", b)
		}
	}
	if err := CheckBin("/usr/local/bin/aggy"); err != nil {
		t.Error(err)
	}
}

func TestAddNotify(t *testing.T) {
	line := "notify = ['" + bin + "', 'hook', '--vendor', 'codex', 'notify']"
	tests := []struct {
		name, src, want string
		err             bool
	}{
		{"new file", "", line + "\n", false},
		{"keeps the rest", "# mine\r\nmodel = \"o3\"\r\n[tui]\r\nx = 1\r\n", line + "\r\n# mine\r\nmodel = \"o3\"\r\n[tui]\r\nx = 1\r\n", false},
		{"already there", line + "\n[a]\n", line + "\n[a]\n", false},
		{"someone else's notify", "notify = [\"notify-send\"]\n", "", true},
		{"other aggy path", "notify = ['/old/aggy', 'hook', '--vendor', 'codex', 'notify']\n", "", true},
		{"not TOML", "a = = 1\n", "", true},
	}
	for _, tt := range tests {
		out, err := AddNotify(bin)([]byte(tt.src))
		if (err != nil) != tt.err || (!tt.err && string(out) != tt.want) {
			t.Errorf("%s: %q, %v", tt.name, out, err)
		}
		if tt.err && !errors.Is(err, ErrAmbiguous) && !errors.Is(err, ErrNotifyTaken) {
			t.Errorf("%s: err %v is not ErrAmbiguous", tt.name, err)
		}
	}
}
