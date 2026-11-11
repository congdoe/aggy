//go:build unix

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/trustlist"
)

// agyWorldE2E is a world with fake-agy installed as agy, a git repo, aggy
// setup run, and, when ticked, Antigravity ticked in the repo's trust list.
func agyWorldE2E(t *testing.T, ticked bool) *world {
	t.Helper()
	w := newWorld(t)
	b, err := os.ReadFile(fakecli.Build(t).Agy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.bin, "agy"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	gi := exec.Command("/usr/bin/git", "init", "-q", w.cwd)
	gi.Env = []string{"HOME=" + w.home, "PATH=/usr/bin:/bin"}
	if out, err := gi.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	setup := exec.Command(buildAggy(t), "setup", "--yes")
	setup.Dir = w.cwd
	setup.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("aggy setup: %v\n%s", err, out)
	}
	t.Setenv("HOME", w.home)
	key, err := safefs.CanonicalPath(w.cwd)
	if err != nil {
		t.Fatal(err)
	}
	f := trustlist.Folder{Shown: []string{"claude", "antigravity"}}
	if ticked {
		f.On = []string{"antigravity"}
	}
	body, _ := json.Marshal(trustlist.File{Folders: map[string]trustlist.Folder{key: f}})
	if err := os.WriteFile(filepath.Join(w.home, ".aggy", "trust.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return w
}

// agyRecords are fake-agy's records of one mode.
func agyRecords(t *testing.T, w *world, mode string) []fakecli.Record {
	t.Helper()
	var out []fakecli.Record
	for _, r := range fakecli.Records(t, w.record) {
		if r.Vendor == "agy" && r.Mode == mode {
			out = append(out, r)
		}
	}
	return out
}

// TestAntigravityUnticked (T6957): a folder whose trust list does not tick
// Antigravity asks, at a terminal, to tick it; answered no, aggy does not
// start agy, writes no hooks file, and names aggy trust.
func TestAntigravityUnticked(t *testing.T) {
	w := agyWorldE2E(t, false)
	s := w.start(t, map[string]any{"raw_echo": true}, "run", "antigravity")
	s.waitFor([]byte("Tick Antigravity (Gemini) here and start agy? [y/N]"))
	s.answerUntil("n\r", []byte("aggy trust"))
	if code := s.exit(); code == 0 {
		t.Fatal("aggy run antigravity started in an unticked folder")
	}
	if !strings.Contains(s.screen(), "antigravity.not_ticked") {
		t.Errorf("screen: %q", s.screen())
	}
	if len(agyRecords(t, w, "interactive")) != 0 || len(agyRecords(t, w, "trust-dialog")) != 0 {
		t.Error("agy ran")
	}
	if _, err := os.Stat(filepath.Join(w.cwd, ".agents", "hooks.json")); err == nil {
		t.Error("a hooks file was written for an unticked folder")
	}
}

// TestAntigravityOnScreen (T6957): aggy run antigravity writes aggy's entry
// into the folder's .agents/hooks.json and passes its --settings; after agy's
// trust dialog the hooks run as agy runs them and decide (the policy's ask
// reaches agy as force_ask, which prompts), the turn end reaches the
// supervisor, and a switch to Claude carries a brief.
func TestAntigravityOnScreen(t *testing.T) {
	w := agyWorldE2E(t, true)
	sc := map[string]any{"stdout": "READY>", "agy": map[string]any{
		"tool_calls": []map[string]any{
			{"name": "run_command", "args": map[string]any{"CommandLine": "rm -rf build", "Cwd": w.cwd}},
			{"name": "run_command", "args": map[string]any{"CommandLine": "ls", "Cwd": w.cwd}},
		},
		"approve": "y",
	}}
	s := w.start(t, sc, "run", "antigravity")
	s.waitWithin([]byte("Do you trust the contents of this project?"), offerTimeout)
	shown := strings.Join(strings.Fields(s.screen()), " ")
	for _, want := range []string{"not supported under consumer plans", "load only once you do"} {
		if !strings.Contains(shown, want) {
			t.Errorf("no %q at launch: %q", want, shown)
		}
	}
	s.send([]byte("\r"))
	s.waitWithin([]byte("agy (fake)"), offerTimeout)
	s.send([]byte("work\r"))
	s.waitWithin([]byte("ok: work"), offerTimeout)
	// The turn end reached the supervisor: a switch goes through at once.
	s.send([]byte{0x1c})
	s.waitWithin([]byte("aggy: switch vendor"), offerTimeout)
	pickVendor(t, s, "claude", true)
	s.waitWithin([]byte("brief -> claude. Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
	w.waitRuns(t, s, 2)
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d; screen %q", code, s.screen())
	}

	runs := interactiveRuns(t, w)
	if len(runs) != 2 || runs[0].Vendor != "agy" || runs[1].Vendor != "claude" {
		t.Fatalf("runs = %+v", runs)
	}
	a := runs[0]
	if len(a.Args) != 2 || a.Args[0] != "--settings" || !filepath.IsAbs(a.Args[1]) {
		t.Fatalf("agy args = %q", a.Args)
	}
	if a.Env["AGGY_VENDOR"] != "antigravity" || a.Env["AGGY_DECIDE_PLAN"] == "" || len(a.Env["AGGY_DECIDE_SHA256"]) != 64 || a.Cwd != w.cwd {
		t.Errorf("env %v, cwd %q", a.Env, a.Cwd)
	}
	for _, k := range a.EnvKeys {
		if strings.HasSuffix(k, "_API_KEY") || strings.HasSuffix(k, "_BASE_URL") {
			t.Errorf("agy got %s", k)
		}
	}
	// The hooks file holds aggy's entry, and git does not see it.
	hf, err := os.ReadFile(filepath.Join(w.cwd, ".agents", "hooks.json"))
	if err != nil || !strings.Contains(string(hf), "hook antigravity --event PreToolUse") {
		t.Fatalf("hooks.json: %s, %v", hf, err)
	}
	if ex, _ := os.ReadFile(filepath.Join(w.cwd, ".git", "info", "exclude")); !strings.Contains(string(ex), ".agents/hooks.json") {
		t.Errorf("exclude: %s", ex)
	}
	// The hooks decided: the policy's ask prompted, the plain call fell to
	// agy's own prompt.
	var outcomes []string
	for _, r := range agyRecords(t, w, "tool") {
		outcomes = append(outcomes, strings.Join(r.Args, "="))
	}
	if want := []string{"run_command=ran=force_ask", "run_command=ran=none"}; !slices.Equal(outcomes, want) {
		t.Errorf("tool calls = %q, want %q", outcomes, want)
	}
	briefs, _ := filepath.Glob(filepath.Join(w.cwd, ".aggy", "briefs", "*.md"))
	if len(briefs) != 1 {
		t.Fatalf("briefs = %q", briefs)
	}
	if runs[1].Env["AGGY_PARENT"] != a.Env["AGGY_PARENT"] {
		t.Error("parents differ")
	}

	// aggy unwire would take aggy's entry out (it asks at a terminal, so the
	// dry run shows it here; manifest's tests cover the undo itself).
	un := exec.Command(buildAggy(t), "unwire", "--all", "--dry-run")
	un.Dir = w.cwd
	un.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
	out, err := un.CombinedOutput()
	if err != nil || !strings.Contains(string(out), filepath.Join(".agents", "hooks.json")) {
		t.Fatalf("aggy unwire --dry-run: %v\n%s", err, out)
	}
}

// TestAntigravitySwitchTo (T6957): a switch from Claude to Antigravity starts
// agy with the brief as its first prompt (-i), pointing at the brief file.
func TestAntigravitySwitchTo(t *testing.T) {
	w := agyWorldE2E(t, true)
	cfg := "[switch]\norder = [\"claude\", \"antigravity\"]\n"
	if err := os.WriteFile(filepath.Join(w.home, ".aggy", "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := map[string]any{"stdout": "READY>", "hook_command": []string{buildAggy(t), "hook", "--vendor", "claude"},
		"hooks": []map[string]any{{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}"}}}}
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte("work\r"))
	s.waitWithin([]byte("work"), offerTimeout)
	s.send([]byte{0x1c})
	s.waitWithin([]byte("aggy: switch vendor"), offerTimeout)
	pickVendor(t, s, "antigravity", true)
	s.waitWithin([]byte("brief -> antigravity. Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
	s.waitWithin([]byte("Do you trust the contents of this project?"), offerTimeout)
	s.send([]byte("\r"))
	s.waitWithin([]byte("ok: "), offerTimeout)
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d; screen %q", code, s.screen())
	}
	runs := interactiveRuns(t, w)
	if len(runs) != 2 || runs[1].Vendor != "agy" {
		t.Fatalf("runs = %+v", runs)
	}
	i := slices.Index(runs[1].Args, "-i")
	if i < 0 || i+1 >= len(runs[1].Args) || !strings.Contains(runs[1].Args[i+1], filepath.Join(".aggy", "briefs")) {
		t.Errorf("args = %q", runs[1].Args)
	}
}
