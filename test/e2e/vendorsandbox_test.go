//go:build unix

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/safefs"
)

// TestClaudeSandboxLaunch (T6980): a Claude launch in a folder whose trust
// list ticks Claude's sandbox carries sandbox.enabled, failIfUnavailable, and
// autoAllowBashIfSandboxed false in its inline --settings layer, with
// --no-policy too (PR #135 review); in a folder that does not, the layer has
// no sandbox key. T7015: the launch says neither on screen, and aggy doctor
// says which holds for the folder. Claude's sandbox can start on the test machine: macOS
// always, Linux only with bubblewrap and socat, so the ticked half is skipped
// on a Linux machine without them.
func TestClaudeSandboxLaunch(t *testing.T) {
	w := newWorld(t)
	gi := exec.Command("/usr/bin/git", "init", "-q", w.cwd)
	gi.Env = []string{"HOME=" + w.home, "PATH=/usr/bin:/bin"}
	if out, err := gi.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	key, err := safefs.CanonicalPath(w.cwd)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(w.home, ".aggy")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	sc := map[string]any{"stdout": "READY>"}
	launch := func(t *testing.T, args ...string) (map[string]any, string) {
		t.Helper()
		_ = os.Remove(w.record)
		s := w.start(t, sc, append([]string{"claude"}, args...)...)
		s.waitWithin([]byte("READY>"), offerTimeout)
		s.send([]byte("/exit\r"))
		if code := s.exit(); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		runs := interactiveRuns(t, w)
		if len(runs) != 1 {
			t.Fatalf("runs %+v", runs)
		}
		var layer map[string]any
		if err := json.Unmarshal([]byte(runs[0].Flags["--settings"][0]), &layer); err != nil {
			t.Fatal(err)
		}
		return layer, s.screen()
	}
	t.Run("not chosen", func(t *testing.T) {
		layer, out := launch(t)
		if _, ok := layer["sandbox"]; ok {
			t.Errorf("a sandbox key with nothing ticked: %v", layer["sandbox"])
		}
		if strings.Contains(out, "Claude's sandbox") {
			t.Errorf("a sandbox line at launch:\n%s", out)
		}
		if _, doc := w.aggyCode(t, "doctor"); !strings.Contains(doc, "Claude's sandbox: not chosen for this folder") {
			t.Errorf("doctor does not say it:\n%s", doc)
		}
	})
	t.Run("ticked", func(t *testing.T) {
		if runtime.GOOS != "darwin" {
			for _, bin := range []string{"/usr/bin/bwrap", "/usr/bin/socat"} {
				if _, err := os.Stat(bin); err != nil {
					t.Skip("Claude's sandbox cannot start here without bubblewrap and socat on aggy's PATH")
				}
			}
		}
		body, _ := json.Marshal(map[string]any{"repos": []string{},
			"folders": map[string]any{key: map[string]any{"shown": []string{"claude", "claude-sandbox"}, "on": []string{"claude-sandbox"}}}})
		if err := os.WriteFile(filepath.Join(state, "trust.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{nil, {"--no-policy"}} {
			layer, out := launch(t, args...)
			sb, _ := layer["sandbox"].(map[string]any)
			if sb["enabled"] != true || sb["failIfUnavailable"] != true || sb["autoAllowBashIfSandboxed"] != false {
				t.Errorf("%v: sandbox layer %v", args, layer["sandbox"])
			}
			if strings.Contains(out, "Claude's sandbox") {
				t.Errorf("%v: a sandbox line at launch:\n%s", args, out)
			}
		}
		if _, doc := w.aggyCode(t, "doctor"); !strings.Contains(doc, "Claude's sandbox: on for this folder's sessions") {
			t.Errorf("doctor does not say it:\n%s", doc)
		}
	})
}

// TestCodexSandboxLaunchLine (T6980): a level as tight as the default passes
// unasked to the CLI. T7015: a launch that keeps the sandbox on says nothing
// of it, and aggy doctor says how aggy treats Codex's sandbox.
func TestCodexSandboxLaunchLine(t *testing.T) {
	w := newWorld(t)
	sc := map[string]any{"stdout": "READY>"}
	for _, c := range []struct {
		args []string
	}{
		{[]string{"codex"}},
		{[]string{"codex", "-c", "sandbox_mode=read-only"}},
	} {
		_ = os.Remove(w.record)
		s := w.start(t, sc, c.args...)
		s.waitWithin([]byte("READY>"), offerTimeout)
		s.send([]byte{0x04})
		if code := s.exit(); code != 0 {
			t.Fatalf("%v: exit = %d", c.args, code)
		}
		if out := s.screen(); strings.Contains(out, "Codex's sandbox") {
			t.Errorf("%v: a sandbox line at launch:\n%s", c.args, out)
		}
		recs := fakecli.Records(t, w.record)
		if len(c.args) > 1 && (len(recs) == 0 || !slices.Contains(recs[len(recs)-1].Args, "sandbox_mode=read-only")) {
			t.Errorf("%v: args %+v", c.args, recs)
		}
	}
	if _, doc := w.aggyCode(t, "doctor"); !strings.Contains(doc, "Codex's sandbox: on by default") {
		t.Errorf("doctor does not say it:\n%s", doc)
	}
}
