package permissions

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

func TestGrokScopeDir(t *testing.T) {
	got, ok := grokScopeDir("/Users/me/a b.c-d_e~")
	if want := "%2FUsers%2Fme%2Fa%20b.c-d_e~"; got != want || !ok {
		t.Errorf("%q, %v", got, ok)
	}
	if _, ok := grokScopeDir("/" + strings.Repeat("a/", 100)); ok {
		t.Error("a long path gets a hash, which aggy does not compute")
	}
}

func withApprovals(t *testing.T) fixture {
	t.Helper()
	f := newFixture(t)
	dir := filepath.Dir(f.in.Main)
	f.in.GrokHome = filepath.Join(dir, ".grok")
	f.in.CodexHome = filepath.Join(dir, ".codex")
	f.in.Worktree = f.in.Main
	return f
}

func (f fixture) grokFile(root string) string {
	d, _ := grokScopeDir(root)
	return filepath.Join(f.in.GrokHome, "sessions", d, "permission.toml")
}

func TestGrokApprovals(t *testing.T) {
	f := withApprovals(t)
	write(t, f.grokFile(f.in.Main), `
edit_policy = "ask"
allow_bash_execute = true
allowed_bash_commands = ["gh pr list", "python3 x"]
disallowed_bash_commands = ["curl evil"]
allowed_bash_globs = ["gh api repos/owner/*", "ls ?"]
allowed_web_fetch_domains = ["docs.example.com"]
disallowed_web_fetch_domains = ["evil.example.com"]
allowed_mcp_tools = ["srv__tool"]
allowed_mcp_servers = ["srv"]
disallowed_mcp_tools = ["srv__bad"]
some_future_field = 3
`)
	write(t, f.grokFile("/another/repo"), `allowed_bash_commands = ["whoami"]`)
	s := Load(f.in)
	want := []string{"Bash(gh pr list)@grok-approval", "Bash(gh api repos/owner/*)@grok-approval", "WebFetch(domain:docs.example.com)@grok-approval"}
	if got := texts(s.Rules, Allow); !slices.Equal(got, want) {
		t.Errorf("allow: %q", got)
	}
	wantDeny := []string{"Bash(curl evil *)@grok-approval", "WebFetch(domain:evil.example.com)@grok-approval", "WebFetch(domain:*.evil.example.com)@grok-approval"}
	if got := texts(s.Rules, Deny); !slices.Equal(got, wantDeny) {
		t.Errorf("deny: %q", got)
	}
	for _, named := range []string{"allow_bash_execute", "Bash(python3 x)", "ls ?", "srv__tool", "srv", "srv__bad"} {
		if !hasSkip(s, named) {
			t.Errorf("%q not named: %+v", named, s.Skipped)
		}
	}
	if hasSkip(s, "Bash(whoami)") || slices.ContainsFunc(s.Rules, func(r Rule) bool { return strings.Contains(r.Text, "whoami") }) {
		t.Error("another repo's approval came along")
	}
	if len(s.Problems) != 0 {
		t.Errorf("problems: %+v", s.Problems)
	}

	// Grok has them already; Claude and OpenCode are the ones that need them.
	if r := s.Render(adapter.Grok); slices.Contains(r.Allow, "Bash(gh pr list)") {
		t.Errorf("sent back to Grok: %q", r.Allow)
	}
	if r := s.Render(adapter.Claude); !slices.Contains(r.Allow, "Bash(gh pr list)") {
		t.Errorf("not sent to Claude: %q", r.Allow)
	}
}

func TestGrokApprovalsBadFile(t *testing.T) {
	f := withApprovals(t)
	write(t, f.grokFile(f.in.Main), `allowed_bash_commands = 3`)
	if s := Load(f.in); len(s.Rules) != 0 || len(s.Problems) != 1 {
		t.Errorf("%+v", s)
	}
}

const codexFile = `# Comment
prefix_rule(pattern=["gh", "pr", "list"], decision="allow")
prefix_rule(pattern=["python3"], decision="allow")
prefix_rule(pattern=["npm", "test"], decision="allow")
prefix_rule(pattern=["curl"], decision="forbidden")
prefix_rule(pattern=["git", "push"], decision="prompt")
prefix_rule(pattern=["echo", "Hello, world!"], decision="allow")
prefix_rule(pattern=["git", ["a", "b"]], decision="allow")
network_rule(host="example.com", protocol="https", decision="allow")
prefix_rule(
`

func TestCodexApprovals(t *testing.T) {
	f := withApprovals(t)
	write(t, filepath.Join(f.in.CodexHome, "rules", "default.rules"), codexFile)
	s := Load(f.in)
	if got := texts(s.Rules, Allow); len(got) != 0 {
		t.Errorf("an untrusted repo took a rule from a global file: %q", got)
	}
	if got := texts(s.Rules, Deny); !slices.Equal(got, []string{"Bash(curl *)@codex-approval"}) {
		t.Errorf("deny: %q", got)
	}
	if got := texts(s.Rules, Ask); !slices.Equal(got, []string{"Bash(git push *)@codex-approval"}) {
		t.Errorf("ask: %q", got)
	}
	if !hasSkip(s, "Bash(gh pr list *)") {
		t.Errorf("not named: %+v", s.Skipped)
	}
	for _, line := range []string{`network_rule(`, `prefix_rule(`, `"Hello, world!"`, `["a", "b"]`} {
		if !slices.ContainsFunc(s.Skipped, func(k Skip) bool { return strings.Contains(k.Text, line) }) {
			t.Errorf("%q not named: %+v", line, s.Skipped)
		}
	}

	f.in.Trusted = true
	s = Load(f.in)
	if got := texts(s.Rules, Allow); !slices.Equal(got, []string{"Bash(gh pr list *)@codex-approval", "Bash(npm test *)@codex-approval"}) {
		t.Errorf("trusted: %q", got)
	}
	if !hasSkip(s, "Bash(python3 *)") {
		t.Errorf("the filter comes first: %+v", s.Skipped)
	}
	for _, v := range []adapter.Vendor{adapter.Claude, adapter.Grok} {
		if r := s.Render(v); !slices.Contains(r.Allow, "Bash(gh pr list *)") {
			t.Errorf("%s: %q", v, r.Allow)
		}
	}
}

// TestApprovalReachesNextLaunch: each load reads the files again, so an
// approval given since the last launch counts, and one removed stops counting.
func TestApprovalReachesNextLaunch(t *testing.T) {
	f := withApprovals(t)
	if s := Load(f.in); len(s.Rules) != 0 {
		t.Fatalf("%+v", s.Rules)
	}
	write(t, f.local(), `{"permissions":{"allow":["Bash(gh pr view *)"]}}`)
	write(t, f.grokFile(f.in.Main), `allowed_bash_commands = ["gh pr list"]`)
	if s := Load(f.in); len(texts(s.Rules, Allow)) != 2 {
		t.Errorf("%+v", s.Rules)
	}
	write(t, f.grokFile(f.in.Main), ``)
	if s := Load(f.in); len(texts(s.Rules, Allow)) != 1 {
		t.Errorf("%+v", s.Rules)
	}
}
