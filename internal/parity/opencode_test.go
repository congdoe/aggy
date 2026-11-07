package parity

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func renderDoc(t *testing.T, c OpenCodeConfig) openCodeDoc {
	t.Helper()
	b, err := c.Render()
	if err != nil {
		t.Fatal(err)
	}
	var d openCodeDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func baseConfig() OpenCodeConfig {
	return OpenCodeConfig{Providers: []string{"xai"}, Plugin: "/l/plugin", Aggy: "/bin/aggy", Root: "/repo"}
}

func TestOpenCodePermissions(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		c := baseConfig()
		c.Trusted = trusted
		d := renderDoc(t, c)
		if d.Permissions[0] != (openCodeRule{"*", "*", "ask"}) {
			t.Fatalf("first rule = %+v: an ask on everything must come first, as a later rule wins", d.Permissions[0])
		}
		reads := 0
		for _, r := range d.Permissions[1:] {
			if r.Effect != "allow" {
				t.Errorf("%+v: permissions only list allows after the ask", r)
			}
			if r.Action == "shell" && ExactRule(r.Resource) != nil {
				t.Errorf("%+v: not an exact rule", r)
			}
			if r.Resource == "*" && r.Action != "todowrite" && r.Action != "question" {
				if !slices.Contains(openCodeReadTools, r.Action) {
					t.Errorf("%+v: only reads and the session's own tools may be allowed on *", r)
				}
				reads++
			}
			if r.Action == "edit" {
				t.Errorf("%+v: the starter lists no edits", r)
			}
		}
		// A cloned symlink to a key file reached the model under a read allow.
		if want := map[bool]int{false: 0, true: 3}[trusted]; reads != want {
			t.Errorf("trusted=%v: %d read rules, want %d", trusted, reads, want)
		}
	}
}

func TestExactRule(t *testing.T) {
	for _, c := range []struct {
		cmd string
		ok  bool
	}{
		{"git status", true},
		{"rm -f *.tmp", false}, // allowed rm -f keep a.tmp (fake model)
		{"ls ?", false},
		{"{file:/etc/passwd}", false},
		{"a\nb", false},
		{" ls", false},
		{"", false},
	} {
		if err := ExactRule(c.cmd); (err == nil) != c.ok {
			t.Errorf("%q: err = %v, want ok=%v", c.cmd, err, c.ok)
		}
	}
}

func TestOpenCodePolicies(t *testing.T) {
	c := baseConfig()
	c.Providers = []string{"openai", "xai"}
	d := renderDoc(t, c)
	var provider []openCodeRule
	tool := map[string]bool{}
	for _, r := range d.Policies {
		switch r.Action {
		case "provider.use":
			provider = append(provider, r)
		case "tool.use":
			if r.Effect != "deny" {
				t.Errorf("%+v: policies only ever deny", r)
			}
			tool[r.Resource] = true
		default:
			t.Errorf("unexpected policy %+v", r)
		}
	}
	want := []openCodeRule{
		{"provider.use", "*", "deny"}, {"provider.use", "openai", "allow"}, {"provider.use", "xai", "allow"},
		{"provider.use", "claude-agent", "deny"}, {"provider.use", "opencode", "deny"}, {"provider.use", "anthropic", "deny"},
	}
	if !slices.Equal(provider, want) {
		t.Errorf("provider rules = %+v, want %+v (the last match wins)", provider, want)
	}
	for _, r := range []string{
		"shell:rm -rf*", "shell:git push * main", "shell:git reset --hard*", "shell:*opencode auth export*", "shell:*opencode.db*",
		"read:.env", "read:*/.env", "edit:*/.env", "glob:*/.env", "external_directory:*/id_rsa", "read:*/opencode.db", "read:*/opencode.db-*", "read:*/auth.json",
		"grep:.env", "grep:opencode.db",
	} {
		if !tool[r] {
			t.Errorf("no deny on %s", r)
		}
	}
	// grep's resource is its pattern: these would refuse searching for text.
	for _, r := range []string{"grep:*.key", "grep:*.pem", "grep:*/*.key"} {
		if tool[r] {
			t.Errorf("%s would refuse a search for text like api.key", r)
		}
	}
	for r := range tool {
		if strings.Contains(r, "Bash(") {
			t.Errorf("%s: left in Claude's syntax", r)
		}
	}
}

func TestOpenCodeRenderRest(t *testing.T) {
	c := baseConfig()
	c.Model = "xai/grok-build-0.1"
	c.Instructions = []string{"/l/agents.md"}
	d := renderDoc(t, c)
	if d.Model != c.Model || !slices.Equal(d.Instructions, c.Instructions) {
		t.Errorf("model %q, instructions %q", d.Model, d.Instructions)
	}
	if len(d.Plugins) != 1 || d.Plugins[0].Package != "file:///l/plugin" || d.Plugins[0].Options["aggy"] != "/bin/aggy" || d.Plugins[0].Options["root"] != "/repo" {
		t.Errorf("plugins = %+v: the directory form, with what the guard needs", d.Plugins)
	}
	c.NoPolicy = true
	for _, r := range renderDoc(t, c).Policies {
		if r.Action == "tool.use" {
			t.Fatalf("--no-policy kept %+v", r)
		}
	}
	for _, bad := range []func(*OpenCodeConfig){
		func(c *OpenCodeConfig) { c.Providers = nil },
		func(c *OpenCodeConfig) { c.Plugin = "" },
		func(c *OpenCodeConfig) { c.Aggy = "" },
		func(c *OpenCodeConfig) { c.Root = "" },
		func(c *OpenCodeConfig) { c.Root = "/tmp/{env:NOPE}" },
		func(c *OpenCodeConfig) { c.Root = "/tmp/{file:~/.ssh/id_rsa}" },
		func(c *OpenCodeConfig) { c.Instructions = []string{"/tmp/{env:X}/a.md"} },
	} {
		c := baseConfig()
		bad(&c)
		if _, err := c.Render(); err == nil {
			t.Errorf("%+v rendered: a launch needs providers and its guard", c)
		}
	}
}
