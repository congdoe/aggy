package hook

import (
	"errors"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

const bin = "/opt/aggy/bin/aggy"

func cmdFor(v, e string) string {
	return "test -x " + bin + " && " + bin + " hook --vendor " + v + " " + e + " || true"
}

// The person's file: four-space indents, a tab-indented array, a number
// spelling, an HTML-ish string, and a lone surrogate escape, which
// re-encoding would turn into U+FFFD.
const userFile = `{
    "model": "opus",
    "permissions": {"deny": ["Bash(curl *)"], "n": 1.50},
    "note": "a <b> & \ud800",
    "hooks": {
        "Stop": [
	    {"hooks": [{"type": "command", "command": "say done"}]}
        ]
    }
}
`

func TestMergeInsertsOnly(t *testing.T) {
	want := []Entry{{adapter.Claude, "Stop"}, {adapter.Claude, "SessionStart"}}
	out, err := Merge(bin, want)([]byte(userFile))
	if err != nil {
		t.Fatal(err)
	}
	wantOut := `{
    "model": "opus",
    "permissions": {"deny": ["Bash(curl *)"], "n": 1.50},
    "note": "a <b> & \ud800",
    "hooks": {
        "Stop": [
	    {"hooks": [{"type": "command", "command": "say done"}]},
	    {
	        "hooks": [
	            {
	                "type": "command",
	                "command": "` + cmdFor("claude", "Stop") + `",
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
                        "command": "` + cmdFor("claude", "SessionStart") + `",
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
	if n, err := Missing(out, bin, want); err != nil || n != 0 {
		t.Errorf("Missing after merge = %d, %v", n, err)
	}
	// Strip takes exactly aggy's entries out again.
	back, err := Strip(out)
	if err != nil || string(back) != userFile {
		t.Errorf("Strip: %v\n%s", err, back)
	}
}

func TestMergeShapes(t *testing.T) {
	stop := []Entry{{adapter.Claude, "Stop"}}
	tests := []struct{ name, src, want string }{
		{"new file", "", "{\n  \"hooks\": {\n    \"Stop\": [\n      {\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n            \"command\": \"" + cmdFor("claude", "Stop") + "\",\n            \"timeout\": 10\n          }\n        ]\n      }\n    ]\n  }\n}\n"},
		{"compact", `{"a":1}`, `{"a":1, "hooks": {"Stop":[{"hooks":[{"type":"command","command":"` + cmdFor("claude", "Stop") + `","timeout":10}]}]}}`},
		{"empty object", "{}", `{"hooks": {"Stop":[{"hooks":[{"type":"command","command":"` + cmdFor("claude", "Stop") + `","timeout":10}]}]}}`},
		{"old aggy path updated in place", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"'/old/aggy' hook --vendor claude Stop","timeout":10}]}]}}`,
			`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"` + cmdFor("claude", "Stop") + `","timeout":10}]}]}}`},
		{"already there", `{"hooks":{"Stop":[{"hooks":[{"command":"` + cmdFor("claude", "Stop") + `"}]}]}}`,
			`{"hooks":{"Stop":[{"hooks":[{"command":"` + cmdFor("claude", "Stop") + `"}]}]}}`},
	}
	for _, tt := range tests {
		out, err := Merge(bin, stop)([]byte(tt.src))
		if err != nil || string(out) != tt.want {
			t.Errorf("%s: %v\n%s", tt.name, err, out)
		}
	}
}

func TestStrip(t *testing.T) {
	a := cmdFor("claude", "Stop")
	tests := []struct{ name, src, want string }{
		{"only aggy", `{"x":1,"hooks":{"Stop":[{"hooks":[{"command":"` + a + `"}]}]}}`, `{"x":1}`},
		{"keeps the person's group", `{"hooks":{"Stop":[{"hooks":[{"command":"say"}]},{"hooks":[{"command":"` + a + `"}]}]}}`,
			`{"hooks":{"Stop":[{"hooks":[{"command":"say"}]}]}}`},
		{"keeps the person's hook in a shared group", `{"hooks":{"Stop":[{"hooks":[{"command":"` + a + `"},{"command":"say"}]}]}}`,
			`{"hooks":{"Stop":[{"hooks":[{"command":"say"}]}]}}`},
		{"keeps other events", `{"hooks":{"Pre":[],"Stop":[{"hooks":[{"command":"'/old/aggy' hook --vendor claude Stop"}]}]}}`,
			`{"hooks":{"Pre":[]}}`},
		{"nothing of aggy's", `{"hooks":{"Stop":[{"hooks":[{"command":"/x/notaggy hook --vendor claude Stop"}]}]}}`,
			`{"hooks":{"Stop":[{"hooks":[{"command":"/x/notaggy hook --vendor claude Stop"}]}]}}`},
		{"empty", ``, ``},
	}
	for _, tt := range tests {
		out, err := Strip([]byte(tt.src))
		if err != nil || string(out) != tt.want {
			t.Errorf("%s: %v\n%s", tt.name, err, out)
		}
	}
	if _, err := Strip([]byte(`{"hooks":{"Stop":{}}}`)); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("bad shape: %v", err)
	}
}

func TestMissingRefusesAmbiguity(t *testing.T) {
	want := []Entry{{adapter.Claude, "Stop"}}
	for name, src := range map[string]string{
		"array root":       `[]`,
		"two values":       `{} {}`,
		"duplicate key":    `{"hooks":{},"hooks":{}}`,
		"nested duplicate": `{"a":{"x":1,"x":2}}`,
		"hooks not object": `{"hooks":[]}`,
		"event not list":   `{"hooks":{"Stop":{}}}`,
		"group not object": `{"hooks":{"Stop":["x"]}}`,
		"inner not list":   `{"hooks":{"Stop":[{"hooks":{}}]}}`,
		"hook not object":  `{"hooks":{"Stop":[{"hooks":[1]}]}}`,
		"two aggy hooks":   `{"hooks":{"Stop":[{"hooks":[{"command":"/a/aggy hook --vendor claude Stop"},{"command":"/b/aggy hook --vendor claude Stop"}]}]}}`,
		"broken":           `{"hooks":`,
		"too deep":         `{"a":` + strings.Repeat("[", 300) + strings.Repeat("]", 300) + `}`,
	} {
		if _, err := Missing([]byte(src), bin, want); !errors.Is(err, ErrAmbiguous) {
			t.Errorf("%s: err %v", name, err)
		}
		if out, err := Merge(bin, want)([]byte(src)); err == nil {
			t.Errorf("%s: merged into\n%s", name, out)
		}
	}
	// Another vendor's aggy hook is not this entry.
	ok := `{"hooks":{"Stop":[{"hooks":[{"command":"/old/aggy hook --vendor grok Stop"}]}]}}`
	if n, err := Missing([]byte(ok), bin, want); err != nil || n != 1 {
		t.Errorf("Missing = %d, %v", n, err)
	}
}

func TestCheckBin(t *testing.T) {
	for _, b := range []string{"aggy", "/a/../aggy", "/a/it's/aggy", "/a/b\n/aggy", "", "/a b/aggy", "/a/$(x)/aggy", "/a/*/aggy", "/a\\b/aggy"} {
		if CheckBin(b) == nil {
			t.Errorf("CheckBin(%q) passed", b)
		}
	}
	for _, b := range []string{"/usr/local/bin/aggy", "/opt/homebrew/Cellar/aggy/0.1.0_1/bin/aggy", "/home/u@x/.local/bin/aggy"} {
		if err := CheckBin(b); err != nil {
			t.Error(err)
		}
	}
}

func TestAggyHook(t *testing.T) {
	for cmd, ok := range map[string]bool{
		cmdFor("codex", "Stop"):                                        true,
		"'/opt/aggy dir/aggy' hook --vendor claude Stop":               true,
		"/x/aggy hook --vendor claude Stop":                            true,
		"test -x /x/aggy && /y/aggy hook --vendor claude Stop || true": false,
		"/x/aggy hook --vendor gpt Stop":                               false,
		"/x/aggy hook --vendor claude Stop; rm -rf ~":                  false,
		"aggy hook --vendor claude Stop":                               false,
		"/x/notaggy hook --vendor claude Stop":                         false,
	} {
		if _, _, got := aggyHook(cmd); got != ok {
			t.Errorf("aggyHook(%q) = %v", cmd, got)
		}
	}
}
