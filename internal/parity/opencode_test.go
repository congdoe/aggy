package parity

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/permissions"
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

// TestOpenCodeOwnProvider: OpenCode's own provider is denied last unless the
// launch's providers name it (free_models on); claude-agent and anthropic
// stay denied, and a launch that names them is refused.
func TestOpenCodeOwnProvider(t *testing.T) {
	providerRules := func(c OpenCodeConfig) []openCodeRule {
		var out []openCodeRule
		for _, r := range renderDoc(t, c).Policies {
			if r.Action == "provider.use" {
				out = append(out, r)
			}
		}
		return out
	}
	cases := []struct {
		name      string
		providers []string
		want      []openCodeRule
	}{
		{"off: denied last", []string{"xai"}, []openCodeRule{
			{"provider.use", "*", "deny"}, {"provider.use", "xai", "allow"},
			{"provider.use", "claude-agent", "deny"}, {"provider.use", "anthropic", "deny"}, {"provider.use", "opencode", "deny"},
		}},
		{"on: allowed, no deny after it", []string{"xai", "opencode"}, []openCodeRule{
			{"provider.use", "*", "deny"}, {"provider.use", "xai", "allow"}, {"provider.use", "opencode", "allow"},
			{"provider.use", "claude-agent", "deny"}, {"provider.use", "anthropic", "deny"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Providers = c.providers
			if got := providerRules(cfg); !slices.Equal(got, c.want) {
				t.Errorf("provider rules = %+v, want %+v", got, c.want)
			}
		})
	}
	for _, p := range OpenCodeAlwaysDenied {
		cfg := baseConfig()
		cfg.Providers = []string{"xai", p}
		if _, err := cfg.Render(); err == nil {
			t.Errorf("a launch allowing %q rendered", p)
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
		{"provider.use", "claude-agent", "deny"}, {"provider.use", "anthropic", "deny"}, {"provider.use", "opencode", "deny"},
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

// TestOpenCodeShellRules: the guard's rule check follows the rendered rules.
func TestOpenCodeShellRules(t *testing.T) {
	got, err := OpenCodeShellRules(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, OpenCodeStarter) {
		t.Errorf("got %q, want %q", got, OpenCodeStarter)
	}
	got, err = OpenCodeShellRules([]string{"gh pr list", "git status"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "gh pr list") || len(got) != len(OpenCodeStarter)+1 {
		t.Errorf("an added rule must reach the guard: %q", got)
	}
}

// TestOpenCodeSharedNeverListed: the same normalizing as the filter, so another
// spelling of a command that runs code or reads any path is not a way in.
func TestOpenCodeSharedNeverListed(t *testing.T) {
	var s permissions.Set
	for _, r := range []string{
		"Bash(/bin/cat /etc/passwd)", "Bash(/usr/bin/python3 x.py)", "Bash(python3.12 x.py)", "Bash(env python3 x.py)", "Bash(env x)",
		"Bash(git -c alias.x=!id x)", "Bash(head .env)", "Bash(deno eval x)", "Bash(git  status)", "Bash(PYTHON x.py)", "Bash(\\cat x)", "Bash(sudo make)",
	} {
		s.Rules = append(s.Rules, permissions.Rule{Kind: permissions.Allow, Text: r, Origin: permissions.FromAggy})
	}
	s.Rules = append(s.Rules, permissions.Rule{Kind: permissions.Allow, Text: "Bash(make build)", Origin: permissions.FromAggy})
	got, skipped := OpenCodeShared(s)
	if !slices.Equal(got.Allow, []string{"make build"}) {
		t.Errorf("allow: %q", got.Allow)
	}
	if len(skipped) != 12 {
		t.Errorf("skipped: %+v", skipped)
	}
}

// TestOpenCodeSharedLostRule: never fewer denies, in any form.
func TestOpenCodeSharedLostRule(t *testing.T) {
	rules := func(extra ...permissions.Rule) permissions.Set {
		return permissions.Set{Rules: append([]permissions.Rule{{Kind: permissions.Allow, Text: "Bash(make build)", Origin: permissions.FromAggy}}, extra...)}
	}
	for _, r := range []permissions.Rule{
		{Kind: permissions.Deny, Text: "Bash(x {file:y})", Origin: permissions.FromAggy},
		{Kind: permissions.Ask, Text: "Bash(x ?)", Origin: permissions.FromAggy},
	} {
		if got, _ := OpenCodeShared(rules(r)); len(got.Allow) != 0 {
			t.Errorf("%s: %q", r.Text, got.Allow)
		}
	}
	s := rules()
	s.Withhold = "a file could not be read"
	if got, skipped := OpenCodeShared(s); len(got.Allow) != 0 || len(skipped) != 1 || skipped[0].Why != s.Withhold {
		t.Errorf("%+v, %+v", got, skipped)
	}
}

// TestOpenCodeShared: a rule that cannot be exact is named, never guessed at.
func TestOpenCodeShared(t *testing.T) {
	rule := func(k permissions.Kind, text string) permissions.Rule {
		return permissions.Rule{Kind: k, Text: text, Origin: permissions.FromAggy}
	}
	s := permissions.Set{Rules: []permissions.Rule{
		rule(permissions.Allow, "Bash(gh pr list)"),
		rule(permissions.Allow, "Bash(git diff *)"),
		rule(permissions.Allow, "Bash(git log:*)"),
		rule(permissions.Allow, "Bash(ls ?)"),
		rule(permissions.Allow, "Bash(echo {file:/etc/passwd})"),
		rule(permissions.Allow, "Bash(cat x)"),
		rule(permissions.Allow, "Bash(find .)"),
		rule(permissions.Allow, "Bash(a && b)"),
		rule(permissions.Allow, "Bash(FOO=1 git status)"),
		rule(permissions.Allow, "Read(./x)"),
		rule(permissions.Ask, "Bash(git push *)"),
		rule(permissions.Ask, "Edit"),
		rule(permissions.Deny, "Bash(curl *)"),
		rule(permissions.Deny, "Read(./.env)"),
	}}
	got, skipped := OpenCodeShared(s)
	if !slices.Equal(got.Allow, []string{"gh pr list"}) || !slices.Equal(got.Ask, []string{"git push *"}) || !slices.Equal(got.Deny, []string{"shell:curl *"}) {
		t.Errorf("%+v", got)
	}
	if len(skipped) != 11 {
		t.Errorf("skipped %d: %+v", len(skipped), skipped)
	}
	for _, k := range skipped {
		if k.Why == "" {
			t.Errorf("%q is not named", k.Text)
		}
	}
	b, err := OpenCodeConfig{Providers: []string{"xai"}, Plugin: "/p", Aggy: "/a", Root: "/r", Shared: got}.Render()
	if err != nil {
		t.Fatal(err)
	}
	if allows := OpenCodeRenderedAllows(b); !slices.Contains(allows, "gh pr list") {
		t.Errorf("not rendered: %q", allows)
	}
	var doc openCodeDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if last := doc.Permissions[len(doc.Permissions)-1]; last != (openCodeRule{"shell", "git push *", "ask"}) {
		t.Errorf("an ask must come last, as a later rule wins: %+v", last)
	}
	if !slices.Contains(doc.Policies, openCodeRule{"tool.use", "shell:curl *", "deny"}) {
		t.Errorf("deny missing: %+v", doc.Policies[len(doc.Policies)-1])
	}
	if b, err = (OpenCodeConfig{Providers: []string{"xai"}, Plugin: "/p", Aggy: "/a", Root: "/r", NoPolicy: true, Shared: got}).Render(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "shell:curl *") {
		t.Error("--no-policy drops aggy's denies, not the person's")
	}
}

// TestOpenCodeAutoOption: only a launch with full auto tells the guard so.
func TestOpenCodeAutoOption(t *testing.T) {
	for _, auto := range []bool{false, true} {
		b, err := OpenCodeConfig{Providers: []string{"xai"}, Plugin: "/p", Aggy: "/a", Root: "/r", Auto: auto}.Render()
		if err != nil {
			t.Fatal(err)
		}
		var doc openCodeDoc
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		_, got := doc.Plugins[0].Options["auto"]
		if got != auto {
			t.Errorf("auto %v: options %v", auto, doc.Plugins[0].Options)
		}
	}
}
