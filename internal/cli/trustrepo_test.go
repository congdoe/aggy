package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/safefs"
)

// atKeyboard stands in for a person at a terminal who answers.
func atKeyboard(t *testing.T, answers string) {
	t.Helper()
	oldK, oldT := keyboard, stdinIsTerminal
	keyboard, stdinIsTerminal = strings.NewReader(answers), func() bool { return true }
	t.Cleanup(func() { keyboard, stdinIsTerminal = oldK, oldT })
	for _, v := range []string{"AGGY_DELEGATED", "AGGY_SOCKET", "AGGY_PARENT", "AGGY_VENDOR"} {
		t.Setenv(v, "")
	}
}

func TestPersonGate(t *testing.T) {
	yes, no := func() bool { return true }, func() bool { return false }
	for _, tc := range []struct {
		name string
		env  map[string]string
		term func() bool
		ok   bool
	}{
		{"person", nil, yes, true},
		{"no terminal", nil, no, false},
		{"borrow child", map[string]string{"AGGY_DELEGATED": "1"}, yes, false},
		{"session tool shell", map[string]string{"AGGY_SOCKET": "/s", "AGGY_PARENT": "p"}, yes, false},
		{"session tool shell, vendor only", map[string]string{"AGGY_VENDOR": "claude"}, yes, false},
		{"session tool shell without a terminal", map[string]string{"AGGY_SOCKET": "/s"}, no, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := stdinIsTerminal
			stdinIsTerminal = tc.term
			t.Cleanup(func() { stdinIsTerminal = old })
			err := personGate(func(k string) string { return tc.env[k] })
			if (err == nil) != tc.ok {
				t.Errorf("err %v", err)
			}
		})
	}
}

func trustFileRepos(t *testing.T) []string {
	t.Helper()
	path, err := trustFile()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var f openCodeTrust
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Repos
}

func TestTrustRepo(t *testing.T) {
	h := isolate(t)
	repo := tempRepo(t)
	codexDir := filepath.Join(h, ".codex")
	if err := os.Mkdir(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(codexDir, "config.toml")
	const before = "# mine\nmodel = \"x\"\n"
	if err := os.WriteFile(cfg, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	// A dry run has no side effects, and needs no terminal.
	code, out, _ := run("trust", repo, "--dry-run")
	if code != 0 || !strings.Contains(out, "trust_level") {
		t.Errorf("dry run: %d %q", code, out)
	}
	if b, _ := os.ReadFile(cfg); string(b) != before || trustFileRepos(t) != nil {
		t.Error("the dry run changed something")
	}

	// Not at a terminal: refused, and tagged.
	code, _, errb := run("trust", repo)
	if code != 1 || !strings.Contains(errb, "terminal") || !strings.Contains(errb, "run ") {
		t.Errorf("no terminal: %d %q", code, errb)
	}

	// A person says no.
	atKeyboard(t, "n\n")
	if code, out, _ = run("trust", repo); code != 1 || !strings.Contains(out, "Not trusted") {
		t.Errorf("no: %d %q", code, out)
	}
	if b, _ := os.ReadFile(cfg); string(b) != before {
		t.Error("a no changed something")
	}

	// A session's own shell, even with a terminal.
	atKeyboard(t, "y\n")
	t.Setenv("AGGY_PARENT", "p")
	if code, _, errb = run("trust", repo); code != 1 || !strings.Contains(errb, "tool shell") {
		t.Errorf("session: %d %q", code, errb)
	}
	t.Setenv("AGGY_PARENT", "")

	// A person says yes. What it means is said first, Claude and Grok plainly.
	if code, out, errb = run("trust", repo); code != 0 {
		t.Fatalf("yes: %d %q %q", code, out, errb)
	}
	for _, w := range []string{"Claude and Grok: not handled yet", "committed .claude/settings.json", "aggy unwire", "trust_level"} {
		if !strings.Contains(out, w) {
			t.Errorf("does not say %q:\n%s", w, out)
		}
	}
	key, err := safefs.CanonicalPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(trustFileRepos(t), key) || !repoTrusted(key) {
		t.Errorf("not recorded: %q", trustFileRepos(t))
	}
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), before) || !strings.Contains(string(b), `[projects."`+repo+`"]`) {
		t.Errorf("config: %q", b)
	}
	if _, err := os.Stat(filepath.Join(h, ".claude.json")); !os.IsNotExist(err) {
		t.Error("~/.claude.json must not be touched")
	}

	// Asked again, nothing to do.
	atKeyboard(t, "")
	if code, out, _ = run("trust", repo); code != 0 || !strings.Contains(out, "already trusted") {
		t.Errorf("again: %d %q", code, out)
	}

	// aggy unwire undoes both files.
	if code, out, errb = run("unwire", "--all"); code != 0 {
		t.Fatalf("unwire: %d %q %q", code, out, errb)
	}
	if b, _ := os.ReadFile(cfg); string(b) != before {
		t.Errorf("after unwire: %q", b)
	}
	if repoTrusted(key) {
		t.Error("still trusted after unwire")
	}
}

func TestTrustRepoCodexLeftAlone(t *testing.T) {
	for _, tc := range []struct{ name, config, want string }{
		{"another level", "[projects.\"%s\"]\ntrust_level = \"untrusted\"\n", "yours to change"},
		{"broken", "= nope", "cannot change"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := isolate(t)
			repo := tempRepo(t)
			if err := os.Mkdir(filepath.Join(h, ".codex"), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(h, ".codex", "config.toml")
			text := strings.ReplaceAll(tc.config, "%s", repo)
			if err := os.WriteFile(cfg, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			atKeyboard(t, "y\n")
			code, out, errb := run("trust", repo)
			if code != 0 || !strings.Contains(out, tc.want) {
				t.Errorf("%d %q %q", code, out, errb)
			}
			if b, _ := os.ReadFile(cfg); string(b) != text {
				t.Errorf("Codex's config changed: %q", b)
			}
			if len(trustFileRepos(t)) != 1 {
				t.Error("aggy's own record should still be made")
			}
		})
	}
}

func TestTrustRepoNoCodex(t *testing.T) {
	isolate(t)
	repo := tempRepo(t)
	atKeyboard(t, "y\n")
	if code, out, errb := run("trust", repo); code != 0 || !strings.Contains(out, "no Codex config folder") {
		t.Errorf("%d %q %q", code, out, errb)
	}
}

// TestTrustMakesRepoCodeApply: the point of it, from the rules' side.
func TestTrustMakesRepoCodeApply(t *testing.T) {
	isolate(t)
	repo := tempRepo(t)
	writePermissions(t, `{"permissions":{"allow":["Bash(npm test)"]}}`)
	s := permissionInput(repo)
	if s.Trusted {
		t.Fatal("trusted from the start")
	}
	atKeyboard(t, "y\n")
	if code, out, errb := run("trust", repo); code != 0 {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	if !permissionInput(repo).Trusted {
		t.Error("not trusted after aggy trust")
	}
}
