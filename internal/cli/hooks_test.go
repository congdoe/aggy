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
