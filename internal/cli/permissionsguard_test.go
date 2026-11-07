package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/probe"
)

func realGitRepo(t *testing.T) string {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("no git: %v %s", err, out)
	}
	return repo
}

func TestTrackedFold(t *testing.T) {
	const rel = ".claude/settings.local.json"
	for _, tc := range []struct {
		list string
		want bool
	}{
		{"", false},
		{"a\x00b\x00", false},
		{rel + "\x00", true},
		{"a\x00.CLAUDE/settings.local.json\x00", true},
		{".Claude/SETTINGS.LOCAL.JSON\x00", true},
		{".claude/ſettings.local.json\x00", true}, // the long s folds to s
		{".claude/settings.local.json.bak\x00", false},
	} {
		if got := trackedFold(tc.list, rel); got != tc.want {
			t.Errorf("%q: %v", tc.list, got)
		}
	}
}

// TestCommittedCapitalsAreCommitted is the clone that gave itself allows: on a
// file system that folds case, git lists .CLAUDE/settings.local.json, and its
// pathspec for the lowercase name finds nothing.
func TestCommittedCapitalsAreCommitted(t *testing.T) {
	isolate(t)
	repo := realGitRepo(t)
	file := filepath.Join(repo, ".CLAUDE", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"permissions":{"allow":["Bash(curl evil)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.local.json")); err != nil {
		t.Skip("this file system does not fold case")
	}
	if out, err := exec.Command("git", "-C", repo, "add", "-f", ".CLAUDE/settings.local.json").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	atKeyboard(t, "y\n")
	if code, out, errb := run("trust", repo); code != 0 {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	s := loadPermissions(context.Background(), repo, config.Config{})
	if slices.ContainsFunc(s.Rules, func(r permissions.Rule) bool { return r.Text == "Bash(curl evil)" }) {
		t.Errorf("a committed file's allow applied: %+v", s.Rules)
	}
	if !slices.ContainsFunc(s.Skipped, func(k permissions.Skip) bool {
		return k.Text == "Bash(curl evil)" && strings.Contains(k.Why, "git tracks")
	}) {
		t.Errorf("not named: %+v", s.Skipped)
	}
}

// TestPlantedGitLooksUntracked: with an empty index nothing looks committed, so
// the file's allows wait for trust, either kind.
func TestPlantedGitLooksUntracked(t *testing.T) {
	isolate(t)
	repo := realGitRepo(t)
	file := filepath.Join(repo, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"permissions":{"allow":["Bash(git status)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := loadPermissions(context.Background(), repo, config.Config{})
	if len(s.Rules) != 0 || s.HeldLocal != 1 {
		t.Fatalf("%+v", s)
	}
	atKeyboard(t, "y\n")
	if code, out, errb := run("trust", "--opencode", repo); code != 0 {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	s = loadPermissions(context.Background(), repo, config.Config{})
	if len(s.Rules) != 1 || s.HeldLocal != 0 {
		t.Errorf("aggy trust --opencode counts as trust: %+v", s)
	}
}

func TestUnwireIsForThePerson(t *testing.T) {
	isolate(t)
	old := unwireGate
	unwireGate = personGate
	t.Cleanup(func() { unwireGate = old })

	for _, args := range [][]string{{"unwire", "--all"}, {"unwire", "--run", "x"}, {"unwire", "--change", "x"}, {"unwire", "--change", "x", "--force", "--yes"}} {
		code, _, errb := run(args...)
		if code != 1 || !strings.Contains(errb, "[permissions.not_at_keyboard · run ") {
			t.Errorf("no terminal: %q: %d %q", args, code, errb)
		}
	}
	atKeyboard(t, "")
	t.Setenv("AGGY_SOCKET", "/s")
	if code, _, errb := run("unwire", "--all"); code != 1 || !strings.Contains(errb, "not_at_keyboard") {
		t.Errorf("a session's shell: %d %q", code, errb)
	}
	// A preview changes nothing, so it stays open.
	if code, _, errb := run("unwire", "--all", "--dry-run"); code != 0 {
		t.Errorf("dry run: %d %q", code, errb)
	}
	t.Setenv("AGGY_SOCKET", "")
	if code, _, errb := run("unwire", "--all"); code != 0 {
		t.Errorf("a person: %d %q", code, errb)
	}
}

// TestBrokenAggyFileGuardScansAll: with the rules unknown, the gate has nothing
// to go by, so every command is scanned.
func TestBrokenAggyFileGuardScansAll(t *testing.T) {
	isolate(t)
	repo := tempRepo(t)
	none := func(string) string { return "" }
	writePermissions(t, `{"permissions":{"allow":["Bash(mytool x)"]},}`)
	if code := openCodeGuardFor(t, repo, "PATH=./bin:$PATH; mytool x", none); code != 2 {
		t.Errorf("exit %d", code)
	}
}

// TestGrokNoDenyFlagNoAllows: a deny that cannot be delivered might have stopped
// an allow.
func TestGrokNoDenyFlagNoAllows(t *testing.T) {
	isolate(t)
	s := permissions.Set{Trusted: true, Rules: []permissions.Rule{
		{Kind: permissions.Allow, Text: "Bash(make build)", Origin: permissions.FromAggy},
		{Kind: permissions.Deny, Text: "Bash(make clean)", Origin: permissions.FromAggy},
	}}
	var req adapter.InteractiveRequest
	notes := addPermissions(adapter.Grok, []string{probe.CapAllow}, s, &req)
	if len(req.Rules.Allow) != 0 || !strings.Contains(strings.Join(notes, "\n"), "neither do your allows") {
		t.Errorf("%+v %q", req.Rules, notes)
	}
}

func TestProblemWithholdsAndSaysSo(t *testing.T) {
	isolate(t)
	s := permissions.Set{
		Trusted:  true,
		Rules:    []permissions.Rule{{Kind: permissions.Allow, Text: "Bash(make build)", Origin: permissions.FromAggy}},
		Problems: []permissions.Problem{{Path: "/p", Why: "broken"}},
		Withhold: "no allow goes out",
	}
	var req adapter.InteractiveRequest
	notes := addPermissions(adapter.Claude, []string{probe.CapSettings}, s, &req)
	if len(req.Rules.Allow) != 0 || !strings.Contains(strings.Join(notes, "\n"), "no allow goes out") {
		t.Errorf("%+v %q", req.Rules, notes)
	}
}

// TestCodexHeldNotRepeated: Codex's file is global, so a held approval from it
// would be a note at every launch in every untrusted repo.
func TestCodexHeldNotRepeated(t *testing.T) {
	isolate(t)
	s := permissions.Set{Skipped: []permissions.Skip{
		{Rule: permissions.Rule{Kind: permissions.Allow, Text: "Bash(x *)", Origin: permissions.FromCodexGrant}, Why: "waits"},
	}}
	var req adapter.InteractiveRequest
	if notes := addPermissions(adapter.Claude, []string{probe.CapSettings}, s, &req); len(notes) != 0 {
		t.Errorf("%q", notes)
	}
}

func TestTrustRefusesTooMuch(t *testing.T) {
	h := isolate(t)
	if err := os.Mkdir(filepath.Join(h, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(h, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	atKeyboard(t, "y\ny\ny\ny\ny\ny\n")
	for _, args := range [][]string{{"trust", "/"}, {"trust", "--opencode", "/"}, {"trust", h}, {"trust", "--opencode", h}, {"trust", sub}, {"trust", "--opencode", sub}} {
		code, out, errb := run(args...)
		if code != 1 || !strings.Contains(errb, "too much") || !strings.Contains(errb, " · run ") {
			t.Errorf("%q: %d %q %q", args, code, out, errb)
		}
	}
	if trustFileRepos(t) != nil {
		t.Errorf("recorded: %q", trustFileRepos(t))
	}
}

func TestTrustOutsideGitClaimsNothing(t *testing.T) {
	isolate(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	atKeyboard(t, "y\n")
	code, out, errb := run("trust", dir)
	if code != 0 || !strings.Contains(out, "no git repo") || strings.Contains(out, "now apply here") {
		t.Errorf("%d %q %q", code, out, errb)
	}
	if trustFileRepos(t) != nil {
		t.Errorf("recorded: %q", trustFileRepos(t))
	}
}

// TestUnwireSaysCodexStaysTrusted: aggy's record goes, and Codex's key stays once
// its config changed, so aggy says untrusted while Codex is not.
func TestUnwireSaysCodexStaysTrusted(t *testing.T) {
	h := isolate(t)
	repo := tempRepo(t)
	codexDir := filepath.Join(h, ".codex")
	if err := os.Mkdir(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(codexDir, "config.toml")
	if err := os.WriteFile(cfg, []byte("model = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	atKeyboard(t, "y\n")
	if code, out, errb := run("trust", repo); code != 0 {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(b, "other = 1\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ := run("unwire", "--all")
	if !strings.Contains(out, "Codex still may") || !strings.Contains(out, "config.toml") || !strings.Contains(out, "--force") {
		t.Errorf("%q", out)
	}
	if trustFileRepos(t) != nil {
		t.Errorf("aggy's record stayed: %q", trustFileRepos(t))
	}
}

func TestConfigSetAsksBeforeLoosening(t *testing.T) {
	isolate(t)
	for _, tc := range []struct {
		key, val string
		asks     bool
	}{
		{"permission-mode", "auto", true},
		{"harness.grok", "off", false},
		{"harness.grok", "on", true},
		{"default-harness", "grok", false},
	} {
		atKeyboard(t, "n\n")
		code, out, _ := run("config", "set", tc.key, tc.val)
		if tc.asks && (code != 1 || !strings.Contains(out, "[y/N]") || !strings.Contains(out, "Nothing changed")) {
			t.Errorf("%s no: %d %q", tc.key, code, out)
		}
		if !tc.asks && (code != 0 || strings.Contains(out, "[y/N]")) {
			t.Errorf("%s: %d %q", tc.key, code, out)
		}
		if tc.asks {
			atKeyboard(t, "y\n")
			if code, out, _ := run("config", "set", tc.key, tc.val); code != 0 {
				t.Errorf("%s yes: %d %q", tc.key, code, out)
			}
		}
	}
}

// TestHandTypedRepoKey: a remove of a rule the file holds under another spelling
// of the repo finds it, rather than saying "no such rule" about a listed rule.
func TestHandTypedRepoKey(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	writePermissions(t, `{"repos":{"`+link+`":{"allow":["Bash(git log -5)"]}}}`)
	if code, out, _ := run("permissions", "list"); code != 0 || !strings.Contains(out, "Bash(git log -5)  [aggy, this repo]") {
		t.Fatalf("list: %d %q", code, out)
	}
	atKeyboard(t, "y\n")
	if code, out, errb := run("permissions", "remove", "allow", "Bash(git log -5)"); code != 0 {
		t.Errorf("remove: %d %q %q", code, out, errb)
	}
	if strings.Contains(aggyFileText(t), "git log") {
		t.Errorf("still there: %s", aggyFileText(t))
	}
}
