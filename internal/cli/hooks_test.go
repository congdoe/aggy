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
	old := hookBin
	hookBin = func() (string, error) { return testBin, nil }
	t.Cleanup(func() { hookBin = old })
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
		"'" + testBin + "' hook --vendor claude Stop", "hook --vendor claude StopFailure",
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
	src := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/old/bin/aggy hook --vendor claude Stop"}]}]}}`
	writeAt(t, settings, src, 0o644)
	code, _, errOut := run("hooks", "install", "--vendor", "claude")
	if code != 1 || !strings.Contains(errOut, "another path") {
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
	writeAt(t, cfg, "model = \"o3\"\n[tui]\nx = 1\n", 0o600)
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
	c, _ := os.ReadFile(cfg)
	if !strings.HasPrefix(string(c), "notify = ['"+testBin+"', 'hook', '--vendor', 'codex', 'notify']\n") {
		t.Errorf("config.toml:\n%s", c)
	}
	if out := mustRun(t, "hooks", "status"); !strings.Contains(out, "codex   installed: Codex runs its hooks only once you approve them") {
		t.Errorf("status: %s", out)
	}
	mustRun(t, "unwire", "--all")
	if d := diffSnapshots(before, snapshot(t, h)); len(d) > 0 {
		t.Errorf("unwire left: %v", d)
	}
}

func TestHooksCodexKeepsUserNotify(t *testing.T) {
	h := hooksHome(t)
	cfg := filepath.Join(h, ".codex", "config.toml")
	writeAt(t, cfg, "notify = [\"notify-send\", \"done\"]\n", 0o600)
	out := mustRun(t, "hooks", "install", "--vendor", "codex")
	if !strings.Contains(out, "already sets notify") {
		t.Errorf("no note:\n%s", out)
	}
	if c, _ := os.ReadFile(cfg); string(c) != "notify = [\"notify-send\", \"done\"]\n" {
		t.Errorf("config.toml changed:\n%s", c)
	}
	if _, err := os.Stat(filepath.Join(h, ".codex", "hooks.json")); err != nil {
		t.Error("hooks.json was not written")
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
		if !strings.Contains(string(b), "hook --vendor grok "+e+`"`) {
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
