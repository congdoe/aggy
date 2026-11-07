package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/probe"
)

// writePermissions writes ~/.aggy/permissions.json.
func writePermissions(t *testing.T, text string) string {
	t.Helper()
	path, err := permissionsFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// tempRepo is a real path with a .git folder.
func tempRepo(t *testing.T) string {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return repo
}

// openCodeGuardFor runs the guard on a shell command.
func openCodeGuardFor(t *testing.T, root, cmd string, getenv func(string) string) int {
	t.Helper()
	b, err := json.Marshal(hook.OpenCodeInput{Tool: "shell", Directory: root, Root: root, Command: cmd})
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	return hookWith([]string{"opencode-guard"}, bytes.NewReader(b), getenv, &out, &errb)
}

// TestGuardSeesAddedRule is the safety requirement of T6913: a rule added by
// hand is rendered with no prompt, so the guard's scan must count it, or a
// variable set in front of it would run a planted program with no prompt.
func TestGuardSeesAddedRule(t *testing.T) {
	isolate(t)
	repo := tempRepo(t)
	noEnv := func(string) string { return "" }
	const cmd = "PATH=./bin; gh pr list"
	if code := openCodeGuardFor(t, repo, cmd, noEnv); code != 0 {
		t.Fatalf("no rule, so OpenCode asks: exit %d", code)
	}
	writePermissions(t, `{"permissions":{"allow":["Bash(gh pr list)"]}}`)
	if code := openCodeGuardFor(t, repo, cmd, noEnv); code != 2 {
		t.Errorf("an everywhere rule: exit %d", code)
	}
	writePermissions(t, `{"repos":{"`+repo+`":{"allow":["Bash(gh pr list)"]}}}`)
	if code := openCodeGuardFor(t, repo, cmd, noEnv); code != 2 {
		t.Errorf("this repo's rule: exit %d", code)
	}
	if code := openCodeGuardFor(t, tempRepo(t), cmd, noEnv); code != 0 {
		t.Errorf("another repo's rule: exit %d", code)
	}
	// A rule that runs the repo's code renders only in a trusted repo, but
	// trust can change during a session, so the gate counts it regardless.
	writePermissions(t, `{"permissions":{"allow":["Bash(npm test)"]}}`)
	if code := openCodeGuardFor(t, repo, "PATH=./bin; npm test", noEnv); code != 2 {
		t.Errorf("a rule for a trusted repo: exit %d", code)
	}
}

// TestGuardSeesRenderedRule: a rule removed from the file during a session is
// still in the running OpenCode's config.
func TestGuardSeesRenderedRule(t *testing.T) {
	isolate(t)
	repo := tempRepo(t)
	shared, _ := parity.OpenCodeShared(permissions.Set{Rules: []permissions.Rule{{Kind: permissions.Allow, Text: "Bash(gh pr list)"}}})
	data, err := parity.OpenCodeConfig{Providers: []string{"xai"}, Plugin: "/p", Aggy: "/a", Root: repo, Shared: shared}.Render()
	if err != nil {
		t.Fatal(err)
	}
	rendered := filepath.Join(t.TempDir(), "opencode.json")
	if err := os.WriteFile(rendered, data, 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "OPENCODE_CONFIG" {
			return rendered
		}
		return ""
	}
	if code := openCodeGuardFor(t, repo, "PATH=./bin; gh pr list", getenv); code != 2 {
		t.Errorf("exit %d", code)
	}
}

// TestGuardBadPermissionsFile: a broken file must not stop every tool call.
func TestGuardBadPermissionsFile(t *testing.T) {
	isolate(t)
	repo := tempRepo(t)
	writePermissions(t, `{not json`)
	if code := openCodeGuardFor(t, repo, "git status", func(string) string { return "" }); code != 0 {
		t.Errorf("exit %d", code)
	}
}

func TestAddPermissions(t *testing.T) {
	isolate(t)
	s := permissions.Set{
		Mode: "auto", ModeFrom: permissions.FromConfig, Trusted: true,
		Rules: []permissions.Rule{
			{Kind: permissions.Allow, Text: "Bash(gh pr list)", Origin: permissions.FromAggy},
			{Kind: permissions.Deny, Text: "Bash(curl *)", Origin: permissions.FromAggy},
			{Kind: permissions.Allow, Text: "Bash(git status)", Origin: permissions.FromClaudeLocal},
		},
		Skipped: []permissions.Skip{
			{Rule: permissions.Rule{Kind: permissions.Allow, Text: "Bash(*)", Origin: permissions.FromAggy}, Why: "x"},
			{Rule: permissions.Rule{Kind: permissions.Allow, Text: "Bash(npm *)", Origin: permissions.FromClaudeLocal}, Why: "x"},
		},
		Problems: []permissions.Problem{{Path: "/p", Why: "broken"}},
	}
	var req adapter.InteractiveRequest
	notes := addPermissions(adapter.Claude, []string{probe.CapSettings}, s, &req)
	if !slices.Equal(req.Rules.Allow, []string{"Bash(gh pr list)"}) || !slices.Equal(req.Rules.Deny, []string{"Bash(curl *)"}) || req.Rules.Mode != "auto" {
		t.Errorf("%+v", req.Rules)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "1 of your permission rules") || !strings.Contains(joined, "/p") {
		t.Errorf("notes: %s", joined)
	}

	req = adapter.InteractiveRequest{}
	notes = addPermissions(adapter.Claude, nil, s, &req)
	if len(req.Rules.Allow) != 0 || !strings.Contains(strings.Join(notes, "\n"), "--settings") {
		t.Errorf("no --settings: %+v %v", req.Rules, notes)
	}

	req = adapter.InteractiveRequest{}
	addPermissions(adapter.Grok, []string{probe.CapAllow, probe.CapDeny, probe.CapPermissionMode}, s, &req)
	if !slices.Equal(req.Rules.Allow, []string{"Bash(gh pr list)"}) || req.Rules.Mode != "auto" {
		t.Errorf("grok: %+v", req.Rules)
	}
	req = adapter.InteractiveRequest{Extra: []string{"--permission-mode=plan"}}
	addPermissions(adapter.Grok, []string{probe.CapAllow, probe.CapDeny, probe.CapPermissionMode}, s, &req)
	if req.Rules.Mode != "" {
		t.Errorf("the person's own flag comes first: %+v", req.Rules)
	}
	req = adapter.InteractiveRequest{}
	notes = addPermissions(adapter.Grok, nil, s, &req)
	if len(req.Rules.Allow)+len(req.Rules.Deny) != 0 || req.Rules.Mode != "" || len(notes) < 3 {
		t.Errorf("no flags: %+v %v", req.Rules, notes)
	}

	req = adapter.InteractiveRequest{}
	addPermissions(adapter.Codex, nil, s, &req)
	if len(req.Rules.Allow) != 0 {
		t.Errorf("codex: %+v", req.Rules)
	}
}

// TestPermissionsArgv: the rules reach each CLI's own command line, beside the
// policy's denies, and nothing is written anywhere.
func TestPermissionsArgv(t *testing.T) {
	isolate(t)
	req := adapter.InteractiveRequest{
		SessionID: "018f6d3e-7c1a-4b2e-9d4f-0a1b2c3d4e5f",
		Deny:      []string{"Bash(rm -rf*)"},
		Rules: adapter.SharedRules{
			Allow: []string{"Bash(gh pr list)"}, Ask: []string{"Bash(git push *)"},
			Deny: []string{"Bash(curl *)"}, Mode: "auto",
		},
	}
	cmd, err := claude.Adapter{Caps: []string{probe.CapSettings}}.Interactive(req)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(cmd.Args, "--settings")
	if i < 0 {
		t.Fatalf("no --settings: %q", cmd.Args)
	}
	var layer struct {
		Permissions permissions.Object `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(cmd.Args[i+1]), &layer); err != nil {
		t.Fatal(err)
	}
	p := layer.Permissions
	if p.DefaultMode != "auto" || !slices.Equal(p.Allow, req.Rules.Allow) || !slices.Equal(p.Ask, req.Rules.Ask) || !slices.Equal(p.Deny, []string{"Bash(rm -rf*)", "Bash(curl *)"}) {
		t.Errorf("layer: %+v", p)
	}

	cmd, err = grok.New().Interactive(req)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, w := range []string{"--deny Bash(rm -rf*)", "--deny Bash(curl *)", "--allow Bash(gh pr list)", "--permission-mode auto"} {
		if !strings.Contains(joined, w) {
			t.Errorf("grok argv lacks %q: %s", w, joined)
		}
	}
	if strings.Contains(joined, "git push") {
		t.Errorf("an ask rule has no confirmed flag on Grok: %s", joined)
	}
	req.Rules.Allow = []string{"Bash(a,b)"}
	if _, err := grok.New().Interactive(req); err == nil {
		t.Error("a comma would split the rule in two")
	}
}

// TestBorrowCarriesNoSharedAllow: a borrow child's lockdown is unchanged by
// this layer.
func TestBorrowCarriesNoSharedAllow(t *testing.T) {
	const sentinel = "sentinel-shared-rule"
	f := newBorrowFixture(t, adapter.Grok)
	writePermissions(t, `{"permissions":{"defaultMode":"dontAsk","allow":["Bash(`+sentinel+`)"],"deny":["Bash(`+sentinel+`-deny)"]},"repos":{"`+f.repo+`":{"allow":["Bash(`+sentinel+`-repo)"]}}}`)
	f.scenario["grok"] = `{"json":{"result":"Summary: ok"}}`
	if code, out, errOut := f.run(t, "news", "what shipped?"); code != 0 {
		t.Fatalf("news: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	rec, err := os.ReadFile(f.record)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec) == 0 {
		t.Fatal("no run was recorded")
	}
	if strings.Contains(string(rec), sentinel) || strings.Contains(string(rec), "dontAsk") {
		t.Errorf("a borrow's argv carries a shared rule:\n%s", rec)
	}
}

func TestLoadPermissionsConfigMode(t *testing.T) {
	isolate(t)
	repo := tempRepo(t)
	cfg := config.Default()
	cfg.Permissions.Mode = "manual"
	s := loadPermissions(t.Context(), repo, cfg)
	if s.Mode != "default" || s.ModeFrom != permissions.FromConfig {
		t.Errorf("%+v", s)
	}
}
