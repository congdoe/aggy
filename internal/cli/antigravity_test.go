package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/antigravity"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/probe"
)

var agyCaps = []string{probe.CapSettings, probe.CapResume, probe.CapSandbox}

// agyWorld is openCodeWorld with no terminal to ask at, and git's answer on
// whether the hooks file is tracked.
func agyWorld(t *testing.T, tracked bool) (repo, dir string) {
	t.Helper()
	repo, dir = openCodeWorld(t)
	gi := exec.Command("git", "init", "-q", repo)
	gi.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	if b, err := gi.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, b)
	}
	inPlaceWorld(t, "", false, false)
	old := agyHooksTracked
	agyHooksTracked = func(context.Context, string, string) (bool, error) { return tracked, nil }
	t.Cleanup(func() { agyHooksTracked = old })
	return repo, dir
}

func agyIn(repo, dir string) agyInput {
	return agyInput{caps: agyCaps, cwd: repo, dir: dir, plan: parity.PolicyPlan{OnScreen: true}, version: "1.5.5"}
}

// TestPrepareAntigravity (T6957): refused until ticked; then aggy's entry goes
// into the folder's .agents/hooks.json with a git exclude line, the settings
// file holds the policy's asks and denies, and the plan the hook reads is the
// launch's; a second launch writes nothing; --no-policy writes no plan.
func TestPrepareAntigravity(t *testing.T) {
	repo, dir := agyWorld(t, false)
	ctx := context.Background()
	in := agyIn(repo, dir)
	if _, err := prepareAntigravity(ctx, in); agyCode(err, diag.Code{}) != diag.CodeAgyUntrusted || !strings.Contains(err.Error(), "aggy trust") {
		t.Fatalf("unticked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".agents")); err == nil {
		t.Fatal("an unticked launch wrote")
	}
	tickTrustRows(t, repo, trustRowAntigravity)
	out, err := prepareAntigravity(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	hf, err := os.ReadFile(filepath.Join(repo, ".agents", "hooks.json"))
	if err != nil || !strings.Contains(string(hf), filepath.Join(filepath.Dir(repo), "aggy")+" hook antigravity --event PreToolUse") {
		t.Fatalf("hooks.json: %s, %v", hf, err)
	}
	if ex, _ := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude")); !strings.Contains(string(ex), ".agents/hooks.json") {
		t.Errorf("exclude: %q", ex)
	}
	s, err := os.ReadFile(out.launch.SettingsFile)
	if err != nil || !strings.Contains(string(s), `"deny"`) && !strings.Contains(string(s), `"ask"`) || strings.Contains(string(s), `"allow"`) {
		t.Errorf("settings: %s, %v", s, err)
	}
	plan, err := readDecidePlan(out.launch.Plan, out.launch.PlanSum)
	if err != nil || plan.Root != repo || !strings.Contains(strings.Join(plan.Protected, "\n"), filepath.Join(repo, ".agents")) {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	warns := strings.Join(out.warns, "\n")
	for _, w := range []string{"not supported under consumer plans", "load only once you do"} {
		if !strings.Contains(warns, w) {
			t.Errorf("warns lack %q:\n%s", w, warns)
		}
	}
	again, err := prepareAntigravity(ctx, in)
	if err != nil || strings.Contains(strings.Join(again.warns, "\n"), "consumer plans") {
		t.Fatalf("second launch: %+v, %v", again, err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, ".agents", "hooks.json")); !bytes.Equal(b, hf) {
		t.Errorf("rewritten:\n%s", b)
	}
	in.noPolicy = true
	np, err := prepareAntigravity(ctx, in)
	if err != nil || np.launch.Plan != "" {
		t.Fatalf("--no-policy: %+v, %v", np, err)
	}
	if b, _ := os.ReadFile(np.launch.SettingsFile); string(b) != `{"permissions":{}}` {
		t.Errorf("--no-policy settings: %s", b)
	}
	// A CLI whose help lacks --settings is refused.
	in.caps = []string{probe.CapResume}
	if _, err := prepareAntigravity(ctx, in); agyCode(err, diag.Code{}) != diag.CodeAgyUnreachable {
		t.Errorf("unreachable: %v", err)
	}
}

// TestAgyHooksFile (T6957): aggy merges into a file holding the person's own
// entries, refuses one it cannot parse, and changes one git tracks only on a
// yes at the terminal.
func TestAgyHooksFile(t *testing.T) {
	const theirs = "{\n  \"mine\": {\"Stop\": [{\"type\": \"command\", \"command\": \"echo hi\", \"timeout\": 5}]}\n}\n"
	for _, c := range []struct {
		name    string
		src     string
		tracked bool
		typed   string
		term    bool
		ok      bool
	}{
		{"merged", theirs, false, "", false, true},
		{"unparseable", "{\"mine\": ", false, "", false, false},
		{"not an object", "[]", false, "", false, false},
		{"tracked no terminal", theirs, true, "y\n", false, false},
		{"tracked no", theirs, true, "n\n", true, false},
		{"tracked enter", theirs, true, "\n", true, false},
		{"tracked yes", theirs, true, "y\n", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo, dir := agyWorld(t, c.tracked)
			tickTrustRows(t, repo, trustRowAntigravity)
			inPlaceWorld(t, c.typed, c.term, false)
			file := filepath.Join(repo, ".agents", "hooks.json")
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(c.src), 0o600); err != nil {
				t.Fatal(err)
			}
			in := agyIn(repo, dir)
			var out bytes.Buffer
			in.getenv, in.in, in.out = os.Getenv, offerIn, &out
			_, err := prepareAntigravity(context.Background(), in)
			b, _ := os.ReadFile(file)
			if !c.ok {
				if agyCode(err, diag.Code{}) != diag.CodeAgyHooksFile || string(b) != c.src {
					t.Fatalf("err %v, file %s", err, b)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]json.RawMessage
			if json.Unmarshal(b, &m) != nil || m["mine"] == nil || m["aggy"] == nil {
				t.Fatalf("file %s", b)
			}
			if c.tracked && !strings.Contains(out.String(), "tracked by git") {
				t.Errorf("no ask: %q", out.String())
			}
			if ex, _ := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude")); strings.Contains(string(ex), "hooks.json") {
				t.Error("a file aggy did not make went into git's exclude file")
			}
		})
	}
}

// TestAntigravityHookOutsideAggy (T6957): a plain agy in a folder with aggy's
// entry runs as without aggy: the hook reads the payload, prints nothing, and
// exits 0 for every event, with no aggy variable or another vendor's.
func TestAntigravityHookOutsideAggy(t *testing.T) {
	payload := `{"hookEventName":"PreToolUse","toolCall":{"name":"run_command","args":{"CommandLine":"rm -rf /"}}}`
	for _, env := range []map[string]string{
		{},
		{"AGGY_SOCKET": "/tmp/x.sock", "AGGY_VENDOR": "claude"},
		{"AGGY_VENDOR": "antigravity"},
	} {
		for _, ev := range antigravity.Events {
			var stdout, stderr bytes.Buffer
			code := antigravityHook([]string{"--event", ev}, strings.NewReader(payload), func(k string) string { return env[k] }, &stdout, &stderr)
			if code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Errorf("%v %s: code %d, out %q, err %q", env, ev, code, stdout.String(), stderr.String())
			}
		}
	}
	var stderr bytes.Buffer
	if code := antigravityHook([]string{"--event", "PostToolUse"}, strings.NewReader(""), func(string) string { return "" }, &bytes.Buffer{}, &stderr); code == 0 {
		t.Error("took an event aggy does not hook")
	}
}

// TestAntigravityHookDecides (T6957): inside an aggy Antigravity session the
// PreToolUse hook answers from the launch's plan: force_ask for the policy's
// ask, nothing for no opinion, nothing at all under --no-policy (no plan),
// and force_ask when the plan is not the launch's.
func TestAntigravityHookDecides(t *testing.T) {
	repo, dir := agyWorld(t, false)
	tickTrustRows(t, repo, trustRowAntigravity)
	out, err := prepareAntigravity(context.Background(), agyIn(repo, dir))
	if err != nil {
		t.Fatal(err)
	}
	call := func(cmd string) string {
		b, _ := json.Marshal(map[string]any{"hookEventName": "PreToolUse", "toolCall": map[string]any{"name": "run_command", "args": map[string]any{"CommandLine": cmd}}})
		return string(b)
	}
	env := map[string]string{"AGGY_SOCKET": "/tmp/x.sock", "AGGY_VENDOR": "antigravity", antigravity.PlanEnv: out.launch.Plan, antigravity.PlanSumEnv: out.launch.PlanSum}
	run := func(env map[string]string, payload string) string {
		var stdout, stderr bytes.Buffer
		if code := antigravityHook([]string{"--event", "PreToolUse"}, strings.NewReader(payload), func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
			t.Fatalf("code %d", code)
		}
		return stdout.String()
	}
	if got := run(env, call("rm -rf build")); !strings.Contains(got, `"decision":"force_ask"`) {
		t.Errorf("policy ask = %q", got)
	}
	if got := run(env, call("ls")); got != "" {
		t.Errorf("no opinion = %q", got)
	}
	if got := run(env, "not json"); !strings.Contains(got, `"force_ask"`) {
		t.Errorf("unreadable = %q", got)
	}
	changed := map[string]string{"AGGY_SOCKET": "/tmp/x.sock", "AGGY_VENDOR": "antigravity", antigravity.PlanEnv: out.launch.Plan, antigravity.PlanSumEnv: strings.Repeat("0", 64)}
	if got := run(changed, call("ls")); !strings.Contains(got, `"force_ask"`) {
		t.Errorf("changed plan = %q", got)
	}
	noPolicy := map[string]string{"AGGY_SOCKET": "/tmp/x.sock", "AGGY_VENDOR": "antigravity"}
	if got := run(noPolicy, call("rm -rf build")); got != "" {
		t.Errorf("--no-policy = %q", got)
	}
}

// TestAntigravityTickInPlace: at a terminal an unticked folder is offered the
// tick, with the terms note; a no refuses.
func TestAntigravityTickInPlace(t *testing.T) {
	for _, c := range []struct {
		typed string
		yes   bool
	}{{"y\n", true}, {"n\n", false}} {
		repo, dir := agyWorld(t, false)
		inPlaceWorld(t, c.typed, true, false)
		var out bytes.Buffer
		in := agyIn(repo, dir)
		in.getenv, in.in, in.out = os.Getenv, offerIn, &out
		_, err := prepareAntigravity(context.Background(), in)
		if !strings.Contains(out.String(), "Tick Antigravity (Gemini) here") || !strings.Contains(out.String(), "consumer plans") {
			t.Errorf("ask: %q", out.String())
		}
		if (err == nil) != c.yes || antigravityTicked(repo) != c.yes {
			t.Errorf("%q: err %v, ticked %v", c.typed, err, antigravityTicked(repo))
		}
	}
}

// TestAntigravityLaunchRefusals: print mode and a bare first prompt are
// refused before anything starts, each saying why.
func TestAntigravityLaunchRefusals(t *testing.T) {
	if !strings.Contains(agyPrintRefusal(), "consumer plans") {
		t.Error("print refusal")
	}
	if agyExitNote(1, false) == "" || agyExitNote(1, true) != "" || agyExitNote(0, false) != "" {
		t.Error("exit note")
	}
}

// TestAntigravityDoctor: the scope and update lines always, the unreachable
// line when the help lacks a flag aggy needs.
func TestAntigravityDoctor(t *testing.T) {
	codes := func(fs []checkup.Finding) string {
		var out []string
		for _, f := range fs {
			if f.Vendor != adapter.Antigravity {
				t.Errorf("finding for %s", f.Vendor)
			}
			out = append(out, f.Code)
		}
		return strings.Join(out, ",")
	}
	if got := codes(antigravityFindings(checkup.Vendor{Capabilities: agyCaps})); got != "antigravity-scope,antigravity-updates" {
		t.Errorf("reachable: %s", got)
	}
	if got := codes(antigravityFindings(checkup.Vendor{Capabilities: []string{probe.CapResume}})); got != "antigravity-scope,antigravity-unreachable,antigravity-updates" {
		t.Errorf("unreachable: %s", got)
	}
}
