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

// hermesWorldE2E is a world with fake-hermes installed as hermes, a git repo,
// aggy setup run, and, when ticked, Hermes ticked in the repo's trust list.
func hermesWorldE2E(t *testing.T, ticked bool) *world {
	t.Helper()
	w := newWorld(t)
	b, err := os.ReadFile(fakecli.Build(t).Hermes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.bin, "hermes"), b, 0o755); err != nil {
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
	// The trust list answered for this folder: every harness row shown, so
	// no start asks, and Hermes ticked or not.
	t.Setenv("HOME", w.home)
	key, err := safefs.CanonicalPath(w.cwd)
	if err != nil {
		t.Fatal(err)
	}
	f := trustlist.Folder{Shown: []string{"claude", "hermes"}}
	if ticked {
		f.On = []string{"hermes"}
	}
	body, _ := json.Marshal(trustlist.File{Folders: map[string]trustlist.Folder{key: f}})
	if err := os.WriteFile(filepath.Join(w.home, ".aggy", "trust.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return w
}

// hermesRecords are fake-hermes's records of one mode.
func hermesRecords(t *testing.T, w *world, mode string) []fakecli.Record {
	t.Helper()
	var out []fakecli.Record
	for _, r := range fakecli.Records(t, w.record) {
		if r.Vendor == "hermes" && r.Mode == mode {
			out = append(out, r)
		}
	}
	return out
}

// TestHermesUnticked: a folder whose trust list does not tick Hermes asks, at
// a terminal, to tick it; answered no, aggy does not start it, makes no
// profile, and names aggy trust.
func TestHermesUnticked(t *testing.T) {
	w := hermesWorldE2E(t, false)
	s := w.start(t, map[string]any{"raw_echo": true}, "run", "hermes")
	s.waitFor([]byte("Tick Hermes Agent here and start it? [y/N]"))
	s.answerUntil("n\r", []byte("aggy trust"))
	if code := s.exit(); code == 0 {
		t.Fatal("aggy run hermes started in an unticked folder")
	}
	if !strings.Contains(s.screen(), "hermes.not_ticked") {
		t.Errorf("screen: %q", s.screen())
	}
	if len(hermesRecords(t, w, "interactive")) != 0 || len(hermesRecords(t, w, "profile-create")) != 0 {
		t.Error("Hermes ran, or a profile was made")
	}
}

// TestHermesOnScreen (T6933): aggy run hermes makes aggy's profile, Hermes
// asks once for each of aggy's hooks, the hooks run as Hermes' daemon runs
// them (no AGGY_* variable, another folder) and still find the launch, the
// policy's ask reaches Hermes' approval gate, the turn end reaches the
// supervisor, and a switch to Claude carries a brief read with hermes
// sessions export.
func TestHermesOnScreen(t *testing.T) {
	w := hermesWorldE2E(t, true)
	sc := map[string]any{"stdout": "READY>", "hermes": map[string]any{
		"tool_calls": []map[string]any{
			{"tool": "terminal", "input": map[string]any{"command": "rm -rf build"}},
			{"tool": "terminal", "input": map[string]any{"command": "ls"}},
		},
		"approve": "y",
	}}
	s := w.start(t, sc, "run", "hermes")
	s.waitWithin([]byte("Event:   on_session_end"), offerTimeout)
	s.send([]byte("y\r"))
	s.waitWithin([]byte("Event:   pre_tool_call"), offerTimeout)
	s.send([]byte("y\r"))
	s.waitWithin([]byte("Hermes Agent (fake)"), offerTimeout)
	shown := s.screen()
	for _, want := range []string{"terminal commands only", "asks you once"} {
		if !strings.Contains(strings.Join(strings.Fields(shown), " "), want) {
			t.Errorf("no %q at launch: %q", want, shown)
		}
	}
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
	if len(runs) != 2 || runs[0].Vendor != "hermes" || runs[1].Vendor != "claude" {
		t.Fatalf("runs = %+v", runs)
	}
	h := runs[0]
	if len(h.Args) < 4 || h.Args[0] != "-p" || !strings.HasPrefix(h.Args[1], "aggy-") || h.Args[2] != "chat" || h.Args[3] != "--cli" {
		t.Fatalf("hermes args = %q", h.Args)
	}
	profile := h.Args[1]
	// aggy's block answers the shared-metrics offer, so Hermes' first-run
	// screen never takes the first line typed.
	if n := len(hermesRecords(t, w, "metrics-offer")); n != 0 {
		t.Errorf("the metrics offer showed %d times", n)
	}
	if h.Env["AGGY_VENDOR"] != "hermes" || h.Cwd != w.cwd {
		t.Errorf("env %v, cwd %q", h.Env, h.Cwd)
	}
	// The profile's config carries aggy's block, and the consent Hermes asked
	// for is its own record, in the profile.
	cfg, err := os.ReadFile(filepath.Join(w.home, ".hermes", "profiles", profile, "config.yaml"))
	if err != nil || !strings.Contains(string(cfg), "fail_closed: true") || !strings.Contains(string(cfg), "mode: manual") {
		t.Fatalf("config.yaml: %s, %v", cfg, err)
	}
	if _, err := os.Stat(filepath.Join(w.home, ".hermes", "profiles", profile, "shell-hooks-allowlist.json")); err != nil {
		t.Errorf("no consent recorded: %v", err)
	}
	// The hooks ran as the daemon runs them, and decided.
	var outcomes []string
	for _, r := range hermesRecords(t, w, "tool") {
		outcomes = append(outcomes, strings.Join(r.Args, "="))
	}
	if want := []string{"terminal=approved", "terminal=ran"}; !slices.Equal(outcomes, want) {
		t.Errorf("tool calls = %q, want %q", outcomes, want)
	}
	hooks := hermesRecords(t, w, "hook")
	if len(hooks) != 3 {
		t.Fatalf("hooks = %+v", hooks)
	}
	if !strings.Contains(hooks[0].Args[1], `"action":"approve"`) || hooks[1].Args[1] != "" {
		t.Errorf("pre_tool_call answers = %q, %q", hooks[0].Args[1], hooks[1].Args[1])
	}
	if hooks[2].Args[0] != "on_session_end" || hooks[2].Args[1] != "" || hooks[2].Args[2] != "" {
		t.Errorf("turn end hook = %q", hooks[2].Args)
	}
	// The brief has the session's turns, from hermes sessions export.
	if len(hermesRecords(t, w, "export")) == 0 {
		t.Error("no session export ran")
	}
	briefs, _ := filepath.Glob(filepath.Join(w.cwd, ".aggy", "briefs", "*.md"))
	if len(briefs) != 1 {
		t.Fatalf("briefs = %q", briefs)
	}
	if b, _ := os.ReadFile(briefs[0]); !strings.Contains(string(b), "ok: work") {
		t.Errorf("brief has no Hermes turns:\n%s", b)
	}
	// The launch's row is gone with it.
	if runs[1].Env["AGGY_PARENT"] != h.Env["AGGY_PARENT"] {
		t.Error("parents differ")
	}
}

// TestHermesSwitchTo: a switch from Claude to Hermes starts it with the brief
// as its first turn (--query-file) in aggy's profile.
func TestHermesSwitchTo(t *testing.T) {
	w := hermesWorldE2E(t, true)
	cfg := "[switch]\norder = [\"claude\", \"hermes\"]\n"
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
	pickVendor(t, s, "hermes", true)
	s.waitWithin([]byte("brief -> hermes. Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
	s.waitWithin([]byte("Event:   on_session_end"), offerTimeout)
	s.send([]byte("y\r"))
	s.waitWithin([]byte("Event:   pre_tool_call"), offerTimeout)
	s.send([]byte("y\r"))
	s.waitWithin([]byte("ok: "), offerTimeout)
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d; screen %q", code, s.screen())
	}
	runs := interactiveRuns(t, w)
	if len(runs) != 2 || runs[1].Vendor != "hermes" {
		t.Fatalf("runs = %+v", runs)
	}
	i := slices.Index(runs[1].Args, "--query-file")
	if i < 0 || i+1 >= len(runs[1].Args) || !strings.Contains(runs[1].Args[i+1], filepath.Join(".aggy", "briefs")) {
		t.Errorf("args = %q", runs[1].Args)
	}
}
