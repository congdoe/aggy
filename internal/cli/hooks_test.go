package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/home"
)

const testBin = "/opt/aggy/bin/aggy"

// hooksHome isolates HOME and the state directory and makes aggy's own
// path testBin.
func hooksHome(t *testing.T) string {
	t.Helper()
	h := isolate(t)
	oldBin, oldPath, oldCheck := hookBin, pathAggy, checkTrusted
	hookBin = func() (string, error) { return testBin, nil }
	pathAggy = func() (string, error) { return "", os.ErrNotExist }
	checkTrusted = func(string) error { return nil }
	t.Cleanup(func() { hookBin, pathAggy, checkTrusted = oldBin, oldPath, oldCheck })
	return h
}

const userSettings = `{
  "model": "opus",
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "afplay done.aiff"}]}]
  }
}
`

func TestHooksInstallClaude(t *testing.T) {
	h := hooksHome(t)
	settings := filepath.Join(h, ".claude", "settings.json")
	writeAt(t, settings, userSettings, 0o644)
	state := os.Getenv(home.Env)
	before, beforeState := snapshot(t, h), snapshot(t, state)

	out := mustRun(t, "hooks", "install", "--vendor", "claude", "--dry-run")
	if !strings.Contains(out, "hook --vendor claude StopFailure") || !strings.Contains(out, "# 1 file would change") {
		t.Errorf("dry run:\n%s", out)
	}
	if len(diffSnapshots(before, snapshot(t, h))) > 0 || len(diffSnapshots(beforeState, snapshot(t, state))) > 0 {
		t.Fatal("the dry run changed something")
	}

	mustRun(t, "hooks", "install", "--vendor", "claude")
	b, _ := os.ReadFile(settings)
	for _, s := range []string{"afplay done.aiff", `"model": "opus"`,
		"test -x " + testBin + " && " + testBin + " hook --vendor claude Stop || true", "hook --vendor claude StopFailure",
		"hook --vendor claude SessionStart", "hook --vendor claude Notification"} {
		if !strings.Contains(string(b), s) {
			t.Errorf("settings.json lacks %q:\n%s", s, b)
		}
	}
	if out := mustRun(t, "hooks", "install", "--vendor", "claude"); !strings.Contains(out, "Nothing to change") {
		t.Errorf("second install: %s", out)
	}
	if out := mustRun(t, "hooks", "status"); !strings.Contains(out, "claude  installed") {
		t.Errorf("status: %s", out)
	}

	mustRun(t, "unwire", "--all")
	if d := diffSnapshots(before, snapshot(t, h)); len(d) > 0 {
		t.Errorf("unwire left HOME changed: %v", d)
	}
}

func TestHooksInstallRefusesAmbiguous(t *testing.T) {
	h := hooksHome(t)
	settings := filepath.Join(h, ".claude", "settings.json")
	src := `{"hooks":{"Stop":[{"hooks":[{"command":"/a/aggy hook --vendor claude Stop"},{"command":"/b/aggy hook --vendor claude Stop"}]}]}}`
	writeAt(t, settings, src, 0o644)
	code, _, errOut := run("hooks", "install", "--vendor", "claude")
	if code != 1 || !strings.Contains(errOut, "remove all but one") {
		t.Errorf("exit %d: %s", code, errOut)
	}
	if b, _ := os.ReadFile(settings); string(b) != src {
		t.Error("settings.json changed")
	}
	if _, out, _ := run("hooks", "status"); !strings.Contains(out, "claude  cannot be installed as is") {
		t.Errorf("status: %s", out)
	}
}

func TestHooksArgs(t *testing.T) {
	hooksHome(t)
	for _, args := range [][]string{{"hooks", "bogus"}, {"hooks", "install", "--vendor", "gpt"}, {"hooks", "install", "x"}, {"hooks", "status", "--dry-run"}} {
		if code, _, _ := run(args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	hookBin = func() (string, error) { return "/tmp/go-build123/b001/exe/aggy", nil }
	if code, _, errOut := run("hooks", "install"); code != 1 || !strings.Contains(errOut, "go run") {
		t.Errorf("go run binary: %d %s", code, errOut)
	}
}

func TestHooksInstallCodex(t *testing.T) {
	h := hooksHome(t)
	ch := filepath.Join(h, "ch")
	t.Setenv("CODEX_HOME", ch)
	cfg := filepath.Join(ch, "config.toml")
	writeAt(t, cfg, "model = \"o3\"\n", 0o600)
	before := snapshot(t, h)

	out := mustRun(t, "hooks", "install", "--vendor", "codex")
	if !strings.Contains(out, "/hooks") || !strings.Contains(out, "cannot check") {
		t.Errorf("no approval note:\n%s", out)
	}
	b, _ := os.ReadFile(filepath.Join(ch, "hooks.json"))
	for _, s := range []string{"hook --vendor codex Stop", "hook --vendor codex SessionStart"} {
		if !strings.Contains(string(b), s) {
			t.Errorf("hooks.json lacks %q:\n%s", s, b)
		}
	}
	if strings.Contains(string(b), "StopFailure") {
		t.Error("Codex has no StopFailure hook")
	}
	// No notify: it would put every turn's prompts in aggy's argv.
	if c, _ := os.ReadFile(cfg); string(c) != "model = \"o3\"\n" {
		t.Errorf("config.toml changed:\n%s", c)
	}
	if out := mustRun(t, "hooks", "status"); !strings.Contains(out, "codex   installed: Codex runs its hooks only once you approve them") {
		t.Errorf("status: %s", out)
	}
	mustRun(t, "unwire", "--all")
	if d := diffSnapshots(before, snapshot(t, h)); len(d) > 0 {
		t.Errorf("unwire left: %v", d)
	}
}

func TestHooksInstallGrok(t *testing.T) {
	h := hooksHome(t)
	before := snapshot(t, h)
	mustRun(t, "hooks", "install", "--vendor", "grok")
	b, err := os.ReadFile(filepath.Join(h, ".grok", "hooks", "aggy.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{"Stop", "StopFailure", "StopCancelled", "SessionStart", "Notification"} {
		if !strings.Contains(string(b), "hook --vendor grok "+e+" || true") {
			t.Errorf("aggy.json lacks %s:\n%s", e, b)
		}
	}
	if out := mustRun(t, "hooks", "status"); !strings.Contains(out, "grok    installed") || !strings.Contains(out, "claude  not installed") {
		t.Errorf("status: %s", out)
	}
	mustRun(t, "unwire", "--all")
	if d := diffSnapshots(before, snapshot(t, h)); len(d) > 0 {
		t.Errorf("unwire left: %v", d)
	}
}

// TestUnwireKeepsLaterEdits: Claude rewrites settings.json after the
// install (here: the person adds a deny rule). aggy unwire takes out only
// aggy's hooks and keeps the rule, rather than refusing or restoring the
// pre-install bytes over it.
func TestUnwireKeepsLaterEdits(t *testing.T) {
	h := hooksHome(t)
	settings := filepath.Join(h, ".claude", "settings.json")
	writeAt(t, settings, userSettings, 0o644)
	mustRun(t, "hooks", "install", "--vendor", "claude")
	b, _ := os.ReadFile(settings)
	edited := strings.Replace(string(b), `"model": "opus",`, `"model": "opus",
  "permissions": {"deny": ["Bash(curl *)"]},`, 1)
	writeAt(t, settings, edited, 0o644)

	if out := mustRun(t, "unwire", "--all", "--dry-run"); !strings.Contains(out, "-") || !strings.Contains(out, "hook --vendor claude") {
		t.Errorf("dry run:\n%s", out)
	}
	if b, _ := os.ReadFile(settings); string(b) != edited {
		t.Fatal("the dry run wrote")
	}
	mustRun(t, "unwire", "--all")
	want := strings.Replace(userSettings, `"model": "opus",`, `"model": "opus",
  "permissions": {"deny": ["Bash(curl *)"]},`, 1)
	if b, _ := os.ReadFile(settings); string(b) != want {
		t.Errorf("after unwire:\n%s\nwant\n%s", b, want)
	}
}

// TestHooksReinstallAfterUpgrade: aggy's entries from an older path are
// updated in place, not refused.
func TestHooksReinstallAfterUpgrade(t *testing.T) {
	h := hooksHome(t)
	mustRun(t, "hooks", "install", "--vendor", "claude")
	hookBin = func() (string, error) { return "/opt/aggy2/bin/aggy", nil }
	mustRun(t, "hooks", "install", "--vendor", "claude")
	b, _ := os.ReadFile(filepath.Join(h, ".claude", "settings.json"))
	if strings.Contains(string(b), testBin) || strings.Count(string(b), "/opt/aggy2/bin/aggy hook --vendor claude Stop ") != 1 {
		t.Errorf("settings.json:\n%s", b)
	}
	if out := mustRun(t, "hooks", "status"); !strings.Contains(out, "claude  installed") {
		t.Errorf("status: %s", out)
	}
}

func TestAggyBinPrefersPath(t *testing.T) {
	hooksHome(t)
	d := t.TempDir()
	real := filepath.Join(d, "Cellar", "aggy", "1.0", "bin", "aggy")
	writeAt(t, real, "x", 0o755)
	link := filepath.Join(d, "bin", "aggy")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(d, "other", "aggy")
	writeAt(t, other, "x", 0o755)
	hookBin = func() (string, error) { return real, nil }
	for _, tt := range []struct{ onPath, want string }{{link, link}, {other, real}} {
		pathAggy = func() (string, error) { return tt.onPath, nil }
		if got, err := aggyBin(); err != nil || got != tt.want {
			t.Errorf("PATH %s: %s, %v", tt.onPath, got, err)
		}
	}
}
