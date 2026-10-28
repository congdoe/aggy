package tooldef

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
)

// TestMain keeps every test off the real home and state directory.
func TestMain(m *testing.M) { os.Exit(fakecli.IsolatedMain(m)) }

const base = `name = "demo"
description = "A demo tool"
homepage = "https://example.com/demo"
version = "1.2.3"
scope = "global"

[install]
check = ["demo", "--version"]

[mcp]
vendors = ["claude", "codex"]
argv = ["demo", "serve"]
env = ["DEMO_HOME"]

[[hook]]
vendors = ["grok"]
event = "PostToolUse"
argv = ["/usr/local/bin/demo", "hook"]

[cost]
method = "mcp-schema"
`

// skillOnly is a valid definition with only a skill part.
const skillOnly = `name = "brief"
description = "A skill"
version = "1"
scope = "global"

[skill]
vendors = ["claude"]
path = "~/.aggy/skills/brief"

[cost]
method = "skill-md"
`

func set(old, new string) func(string) string {
	return func(s string) string {
		if !strings.Contains(s, old) {
			panic("base lacks " + old)
		}
		return strings.Replace(s, old, new, 1)
	}
}

// pre adds top-level keys, which must come before the first table.
func pre(extra string) func(string) string { return func(s string) string { return extra + "\n" + s } }

func manyArgs(n int) string {
	a := make([]string, n)
	for i := range a {
		a[i] = `"x"`
	}
	return `argv = ["demo", ` + strings.Join(a, ", ") + `]`
}

func manyEnv(n int) string {
	a := make([]string, n)
	for i := range a {
		a[i] = fmt.Sprintf(`"V%d"`, i)
	}
	return `env = [` + strings.Join(a, ", ") + `]`
}

func TestParseValid(t *testing.T) {
	for _, c := range []struct {
		name, text string
		builtIn    bool
	}{
		{"base", base, false},
		{"skill only, no check", skillOnly, false},
		{"absolute skill path", strings.Replace(skillOnly, `"~/.aggy/skills/brief"`, `"/opt/skills/brief"`, 1), false},
		{"repo scope with condition", strings.Replace(base, `scope = "global"`, "scope = \"repo\"\nwhen = \"has-ui\"", 1), false},
		{"no homepage", strings.Replace(base, "homepage = \"https://example.com/demo\"\n", "", 1), false},
		{"built-in default on", "default = true\n" + base, true},
		{"built-in lifecycle", `lifecycle = ["worktree-copy", "gc", "refresh"]` + "\n" + base, true},
		{"unverified keys", `unverified = ["version", "mcp", "install"]` + "\n" + base, false},
		{"static cost", strings.Replace(base, `method = "mcp-schema"`, "method = \"static\"\ntokens = 300", 1), false},
		{"crlf", strings.ReplaceAll(base, "\n", "\r\n"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse("demo.toml", []byte(c.text), c.builtIn); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestParseRefuses covers every validator rule: each case breaks one
// rule of a valid definition, and the error names the file and the key.
func TestParseRefuses(t *testing.T) {
	cases := []struct {
		name    string
		edit    func(string) string
		builtIn bool
		key     string // the key the error names; "" is the whole file
		want    string // a substring of the message
	}{
		// Size, encoding, characters.
		{"too large", func(s string) string { return s + "#" + strings.Repeat("x", MaxSize) }, false, "", "bytes"},
		{"not UTF-8", func(s string) string { return s + "# \xff\n" }, false, "", "UTF-8"},
		{"bidi in a comment", func(s string) string { return s + "# \u202e hidden\n" }, false, "", "line"},
		{"zero width in a comment", func(s string) string { return s + "#\u200b\n" }, false, "", "line"},
		{"escaped bell in description", set(`"A demo tool"`, `"A demo\u0007tool"`), false, "description", "control"},
		{"escaped zero width in an argument", set(`argv = ["demo", "serve"]`, `argv = ["demo", "ser\u200bve"]`), false, "mcp.argv", "control"},
		{"escaped newline in the program", set(`argv = ["demo", "serve"]`, `argv = ["de\nmo", "serve"]`), false, "mcp.argv", "control"},
		{"unknown key", pre(`colour = "red"`), false, "colour", "unknown"},
		{"unknown nested key", set(`method = "mcp-schema"`, "method = \"mcp-schema\"\nfoo = 1"), false, "cost.foo", "unknown"},

		// Name, text fields, version.
		{"name upper case", set(`name = "demo"`, `name = "Demo"`), false, "name", ""},
		{"name with a path", set(`name = "demo"`, `name = "../demo"`), false, "name", ""},
		{"name too long", set(`name = "demo"`, `name = "`+strings.Repeat("a", 33)+`"`), false, "name", ""},
		{"name missing", set(`name = "demo"`, ``), false, "name", ""},
		{"description missing", set(`description = "A demo tool"`, ``), false, "description", "required"},
		{"description too long", set(`"A demo tool"`, `"`+strings.Repeat("d", maxDescription+1)+`"`), false, "description", "bytes"},
		{"homepage http", set(`https://example.com/demo`, `http://example.com/demo`), false, "homepage", "https"},
		{"homepage user info", set(`https://example.com/demo`, `https://u:p@example.com/demo`), false, "homepage", "user info"},
		{"homepage no host", set(`https://example.com/demo`, `https:///demo`), false, "homepage", ""},
		{"version missing", set(`version = "1.2.3"`, ``), false, "version", "pinned"},
		{"version with a space", set(`version = "1.2.3"`, `version = "1.2 3"`), false, "version", ""},
		{"version range", set(`version = "1.2.3"`, `version = "^1.2"`), false, "version", ""},

		// Default, scope, condition, parts.
		{"user tool default on", pre("default = true"), false, "default", "off"},
		{"unknown scope", set(`scope = "global"`, `scope = "world"`), false, "scope", ""},
		{"condition on a global tool", pre(`when = "has-ui"`), false, "when", ""},
		{"unknown condition", set(`scope = "global"`, "scope = \"repo\"\nwhen = \"has-gpu\""), false, "when", ""},
		{"no parts", func(string) string {
			return "name = \"x\"\ndescription = \"d\"\nversion = \"1\"\nscope = \"global\"\n[cost]\nmethod = \"none\"\n"
		}, false, "mcp", "at least one part"},

		// Vendors.
		{"no vendors", set(`vendors = ["claude", "codex"]`, `vendors = []`), false, "mcp.vendors", "at least one"},
		{"unknown vendor", set(`vendors = ["claude", "codex"]`, `vendors = ["claude", "gemini"]`), false, "mcp.vendors[1]", ""},
		{"vendor twice", set(`vendors = ["claude", "codex"]`, `vendors = ["codex", "codex"]`), false, "mcp.vendors[1]", "twice"},

		// Argv: never a shell string, programs bare or absolute.
		{"shell string", set(`argv = ["demo", "serve"]`, `argv = "demo serve"`), false, "", ""},
		{"program with a space", set(`argv = ["demo", "serve"]`, `argv = ["demo serve"]`), false, "mcp.argv", "shell string"},
		{"empty argv", set(`argv = ["demo", "serve"]`, `argv = []`), false, "mcp.argv", "program"},
		{"empty program", set(`argv = ["demo", "serve"]`, `argv = [""]`), false, "mcp.argv", ""},
		{"sh -c", set(`argv = ["demo", "serve"]`, `argv = ["sh", "-c", "demo serve"]`), false, "mcp.argv", "shell"},
		{"absolute bash", set(`argv = ["demo", "serve"]`, `argv = ["/bin/bash", "-c", "x"]`), false, "mcp.argv", "shell"},
		{"env wrapper", set(`argv = ["demo", "serve"]`, `argv = ["env", "X=1", "demo"]`), false, "mcp.argv", "shell"},
		{"relative program", set(`argv = ["demo", "serve"]`, `argv = ["./demo"]`), false, "mcp.argv", "bare name"},
		{"program in a subfolder", set(`argv = ["demo", "serve"]`, `argv = ["bin/demo"]`), false, "mcp.argv", "bare name"},
		{"dot dot program", set(`argv = ["demo", "serve"]`, `argv = [".."]`), false, "mcp.argv", "bare name"},
		{"unclean absolute program", set(`argv = ["demo", "serve"]`, `argv = ["/usr/../bin/demo"]`), false, "mcp.argv", "clean"},
		{"too many arguments", set(`argv = ["demo", "serve"]`, manyArgs(maxArgs)), false, "mcp.argv", "at most"},
		{"argument too long", set(`argv = ["demo", "serve"]`, `argv = ["demo", "`+strings.Repeat("a", maxArg+1)+`"]`), false, "mcp.argv", "bytes"},
		{"hook shell", set(`argv = ["/usr/local/bin/demo", "hook"]`, `argv = ["zsh", "-c", "x"]`), false, "hook[0].argv", "shell"},
		{"check shell", set(`check = ["demo", "--version"]`, `check = ["bash", "-c", "demo"]`), false, "install.check", "shell"},
		{"install relative", set(`check = ["demo", "--version"]`, "check = [\"demo\"]\ninstall = [\"./install.sh\"]"), false, "install.install", "bare name"},
		{"prebuild shell", set(`check = ["demo", "--version"]`, "check = [\"demo\"]\nprebuild = [\"dash\", \"-c\", \"x\"]"), false, "install.prebuild", "shell"},
		{"check missing with mcp", set(`check = ["demo", "--version"]`, ``), false, "install.check", "program"},

		// Env allowlist.
		{"base URL", set(`env = ["DEMO_HOME"]`, `env = ["ANTHROPIC_BASE_URL"]`), false, "mcp.env[0]", "base-URL"},
		{"any base URL", set(`env = ["DEMO_HOME"]`, `env = ["DEMO_BASE_URL"]`), false, "mcp.env[0]", "base-URL"},
		{"proxy", set(`env = ["DEMO_HOME"]`, `env = ["HTTPS_PROXY"]`), false, "mcp.env[0]", "proxy"},
		{"lower-case proxy", set(`env = ["DEMO_HOME"]`, `env = ["all_proxy"]`), false, "mcp.env[0]", "proxy"},
		{"API key", set(`env = ["DEMO_HOME"]`, `env = ["OPENAI_API_KEY"]`), false, "mcp.env[0]", "secret"},
		{"tool API key", set(`env = ["DEMO_HOME"]`, `env = ["CONTEXT7_API_KEY"]`), false, "mcp.env[0]", "secret"},
		{"token", set(`env = ["DEMO_HOME"]`, `env = ["GITHUB_TOKEN"]`), false, "mcp.env[0]", "secret"},
		{"secret", set(`env = ["DEMO_HOME"]`, `env = ["DEMO_SECRET"]`), false, "mcp.env[0]", "secret"},
		{"vendor setting", set(`env = ["DEMO_HOME"]`, `env = ["CLAUDE_CODE_USE_BEDROCK"]`), false, "mcp.env[0]", "vendor"},
		{"aggy setting", set(`env = ["DEMO_HOME"]`, `env = ["AGGY_HOME"]`), false, "mcp.env[0]", "aggy"},
		{"env value", set(`env = ["DEMO_HOME"]`, `env = ["DEMO_HOME=/x"]`), false, "mcp.env[0]", "variable name"},
		{"env twice", set(`env = ["DEMO_HOME"]`, `env = ["DEMO_HOME", "DEMO_HOME"]`), false, "mcp.env[1]", "twice"},
		{"too many env", set(`env = ["DEMO_HOME"]`, manyEnv(maxEnv+1)), false, "mcp.env", "at most"},
		{"env table", set(`env = ["DEMO_HOME"]`, `env = { DEMO_HOME = "/x" }`), false, "", ""},

		// Skill, hook, init.
		{"skill relative path", func(string) string { return strings.Replace(skillOnly, `"~/.aggy/skills/brief"`, `"skills/brief"`, 1) }, false, "skill.path", "absolute"},
		{"skill path escapes home", func(string) string { return strings.Replace(skillOnly, `"~/.aggy/skills/brief"`, `"~/../other"`, 1) }, false, "skill.path", "clean"},
		{"skill unclean absolute", func(string) string { return strings.Replace(skillOnly, `"~/.aggy/skills/brief"`, `"/opt/../etc"`, 1) }, false, "skill.path", "clean"},
		{"hook event", set(`event = "PostToolUse"`, `event = "post tool use"`), false, "hook[0].event", ""},
		{"too many hooks", func(s string) string {
			return s + strings.Repeat("[[hook]]\nvendors = [\"grok\"]\nevent = \"Stop\"\nargv = [\"demo\"]\n", maxHooks)
		}, false, "hook", "at most"},
		{"too many init steps", func(s string) string {
			return s + strings.Repeat("[[init]]\nvendors = [\"grok\"]\nargv = [\"demo\"]\n", maxInit+1)
		}, false, "init", "at most"},
		{"init vendor", func(s string) string { return s + "[[init]]\nvendors = [\"x\"]\nargv = [\"demo\"]\n" }, false, "init[0].vendors[0]", ""},

		// Cost.
		{"cost method", set(`method = "mcp-schema"`, `method = "guess"`), false, "cost", "method"},
		{"static without tokens", set(`method = "mcp-schema"`, `method = "static"`), false, "cost", "tokens"},
		{"tokens with a measured method", set(`method = "mcp-schema"`, "method = \"mcp-schema\"\ntokens = 5"), false, "cost", "tokens"},
		{"negative tokens", set(`method = "mcp-schema"`, "method = \"static\"\ntokens = -5"), false, "cost", "tokens"},
		{"too many tokens", set(`method = "mcp-schema"`, "method = \"static\"\ntokens = 2000000"), false, "cost", "tokens"},
		{"skill-md without a skill", set(`method = "mcp-schema"`, `method = "skill-md"`), false, "cost", "skill"},
		{"mcp-schema without mcp", func(string) string {
			return strings.Replace(skillOnly, `method = "skill-md"`, `method = "mcp-schema"`, 1)
		}, false, "cost", "mcp"},

		// Lifecycle and unverified.
		{"user lifecycle", pre(`lifecycle = ["gc"]`), false, "lifecycle", "built-in"},
		{"unknown lifecycle", pre(`lifecycle = ["deploy"]`), true, "lifecycle[0]", ""},
		{"lifecycle twice", pre(`lifecycle = ["gc", "gc"]`), true, "lifecycle[1]", "twice"},
		{"unknown unverified key", pre(`unverified = ["mcp", "colour"]`), false, "unverified[1]", ""},
		{"unverified twice", pre(`unverified = ["mcp", "mcp"]`), false, "unverified[1]", "twice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse("/x/demo.toml", []byte(c.edit(base)), c.builtIn)
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want a *Error", err)
			}
			if e.File != "/x/demo.toml" || e.Key != c.key {
				t.Errorf("file %q key %q, want /x/demo.toml and %q (%v)", e.File, e.Key, c.key, err)
			}
			if !strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), "/x/demo.toml: ") {
				t.Errorf("err = %q, want the file and %q", err, c.want)
			}
		})
	}
}

func TestPartsFor(t *testing.T) {
	text := base + "[[init]]\nvendors = [\"claude\", \"grok\"]\nargv = [\"demo\", \"init\"]\n"
	d, err := Parse("demo.toml", []byte(text), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		v               adapter.Vendor
		mcp             bool
		hooks, initRuns int
	}{
		{adapter.Claude, true, 0, 1},
		{adapter.Codex, true, 0, 0},
		{adapter.Grok, false, 1, 1},
	} {
		p := d.PartsFor(c.v)
		if (p.MCP != nil) != c.mcp || len(p.Hooks) != c.hooks || len(p.Init) != c.initRuns || p.Skill != nil || p.Empty() {
			t.Errorf("%s: parts %+v", c.v, p)
		}
	}
	s, err := Parse("brief.toml", []byte(skillOnly), false)
	if err != nil {
		t.Fatal(err)
	}
	if !s.PartsFor(adapter.Codex).Empty() || s.PartsFor(adapter.Claude).Skill == nil {
		t.Error("a claude-only skill must be empty for codex and present for claude")
	}
}

// TestDesignExample: the full example in docs/design.md is a valid user
// definition.
func TestDesignExample(t *testing.T) {
	b, err := os.ReadFile("../../docs/design.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), "**Tool definitions**")
	if !ok {
		t.Fatal("design.md lacks the tool definitions section")
	}
	_, rest, _ = strings.Cut(rest, "```toml\n")
	example, _, ok := strings.Cut(rest, "```")
	if !ok {
		t.Fatal("design.md lacks the example")
	}
	d, err := Parse("design.md", []byte(example), false)
	if err != nil {
		t.Fatal(err)
	}
	if d.MCP == nil || d.Skill == nil || len(d.Hooks) != 1 || len(d.Init) != 1 {
		t.Errorf("the example should show every part: %+v", d)
	}
}
