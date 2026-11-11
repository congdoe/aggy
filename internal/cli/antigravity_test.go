package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/antigravity"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/policyhook"
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
// entries only on a yes at the terminal, since they will run with the session
// (#144 re-review, MEDIUM-B), refuses one it cannot parse, and changes one git
// tracks only on a second yes.
func TestAgyHooksFile(t *testing.T) {
	const theirs = "{\n  \"mine\": {\"Stop\": [{\"type\": \"command\", \"command\": \"echo hi\", \"timeout\": 5}]}\n}\n"
	for _, c := range []struct {
		name    string
		src     string
		tracked bool
		typed   string
		term    bool
		ok      bool
		code    diag.Code
	}{
		{"merged", theirs, false, "y\n", true, true, diag.Code{}},
		{"merged no terminal", theirs, false, "", false, false, diag.CodeAgyHooksForeign},
		{"merged no", theirs, false, "n\n", true, false, diag.CodeAgyHooksForeign},
		{"unparseable", "{\"mine\": ", false, "", false, false, diag.CodeAgyHooksFile},
		{"not an object", "[]", false, "", false, false, diag.CodeAgyHooksFile},
		{"tracked no terminal", theirs, true, "y\n", false, false, diag.CodeAgyHooksForeign},
		{"tracked no", theirs, true, "y\nn\n", true, false, diag.CodeAgyHooksFile},
		{"tracked enter", theirs, true, "y\n\n", true, false, diag.CodeAgyHooksFile},
		{"tracked yes", theirs, true, "y\ny\n", true, true, diag.Code{}},
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
				if agyCode(err, diag.Code{}) != c.code || string(b) != c.src {
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
	if got := codes(antigravityFindings(checkup.Vendor{Capabilities: agyCaps})); got != "antigravity-scope,antigravity-entry,antigravity-updates" {
		t.Errorf("reachable: %s", got)
	}
	if got := codes(antigravityFindings(checkup.Vendor{Capabilities: []string{probe.CapResume}})); got != "antigravity-scope,antigravity-unreachable,antigravity-entry,antigravity-updates" {
		t.Errorf("unreachable: %s", got)
	}
}

// TestAgyHooksLink (#141 review, M1): a .agents that is a link, or a
// hooks.json that is one, is refused before anything is written, and the
// folder it leads to is left as it was.
func TestAgyHooksLink(t *testing.T) {
	for _, c := range []string{"agents link", "hooks link", "agents a file"} {
		t.Run(c, func(t *testing.T) {
			repo, dir := agyWorld(t, false)
			tickTrustRows(t, repo, trustRowAntigravity)
			outside := t.TempDir()
			switch c {
			case "agents link":
				if err := os.Symlink(outside, filepath.Join(repo, ".agents")); err != nil {
					t.Fatal(err)
				}
			case "hooks link":
				if err := os.Mkdir(filepath.Join(repo, ".agents"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(outside, "hooks.json"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "hooks.json"), filepath.Join(repo, ".agents", "hooks.json")); err != nil {
					t.Fatal(err)
				}
			case "agents a file":
				if err := os.WriteFile(filepath.Join(repo, ".agents"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := prepareAntigravity(context.Background(), agyIn(repo, dir))
			if agyCode(err, diag.Code{}) != diag.CodeAgyHooksLink {
				t.Fatalf("err = %v", err)
			}
			if b, _ := os.ReadFile(filepath.Join(outside, "hooks.json")); strings.Contains(string(b), "aggy") {
				t.Errorf("wrote through the link: %s", b)
			}
		})
	}
}

// TestAgyForeignAggyEntry (#141 review, L6): an entry named aggy that aggy
// did not write is refused, never taken over.
func TestAgyForeignAggyEntry(t *testing.T) {
	repo, dir := agyWorld(t, false)
	tickTrustRows(t, repo, trustRowAntigravity)
	file := filepath.Join(repo, ".agents", "hooks.json")
	const src = "{\"aggy\": {\"Stop\": [{\"type\": \"command\", \"command\": \"echo mine\", \"timeout\": 5}]}}\n"
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := prepareAntigravity(context.Background(), agyIn(repo, dir))
	if agyCode(err, diag.Code{}) != diag.CodeAgyHooksFile || !strings.Contains(err.Error(), "did not write") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(file); string(b) != src {
		t.Errorf("file changed: %s", b)
	}
}

// TestAgyForeignEntryAppeared (#141 review, M3): an entry that appears after
// aggy's last write asks before agy starts (default no); a change to the
// person's own entry only rewrites aggy's; the hook and the exit say when the
// file changed during a session.
func TestAgyForeignEntryAppeared(t *testing.T) {
	repo, dir := agyWorld(t, false)
	tickTrustRows(t, repo, trustRowAntigravity)
	ctx := context.Background()
	if _, err := prepareAntigravity(ctx, agyIn(repo, dir)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(repo, ".agents", "hooks.json")
	if agyExitCheck(repo) != "" {
		t.Errorf("unchanged file reported: %q", agyExitCheck(repo))
	}
	b, _ := os.ReadFile(file)
	planted := strings.Replace(string(b), "{\n", "{\n  \"x\": {\"PreToolUse\": []},\n", 1)
	if err := os.WriteFile(file, []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := agyExitCheck(repo); !strings.Contains(n, `"x"`) {
		t.Errorf("exit check = %q", n)
	}
	// The SessionStart or Stop hook, run in .agents, says so.
	t.Chdir(filepath.Join(repo, ".agents"))
	var stderr bytes.Buffer
	agyHookCheck(&stderr)
	if !strings.Contains(stderr.String(), "antigravity.hooks_changed") {
		t.Errorf("hook check: %q", stderr.String())
	}
	// No terminal: refused.
	if _, err := prepareAntigravity(ctx, agyIn(repo, dir)); agyCode(err, diag.Code{}) != diag.CodeAgyHooksForeign {
		t.Fatalf("no terminal: %v", err)
	}
	// At a terminal, no: refused; yes: started, and recorded.
	for _, typed := range []string{"n\n", "y\n"} {
		inPlaceWorld(t, typed, true, false)
		in := agyIn(repo, dir)
		var out bytes.Buffer
		in.getenv, in.in, in.out = os.Getenv, offerIn, &out
		_, err := prepareAntigravity(ctx, in)
		if typed == "n\n" && agyCode(err, diag.Code{}) != diag.CodeAgyHooksForeign {
			t.Fatalf("no: %v", err)
		}
		if typed == "y\n" && err != nil {
			t.Fatalf("yes: %v", err)
		}
	}
	inPlaceWorld(t, "", false, false)
	if _, err := prepareAntigravity(ctx, agyIn(repo, dir)); err != nil {
		t.Fatalf("after yes: %v", err)
	}
}

// TestAntigravityHookIgnoresGrok (#141 review, L1): Grok's variables do not
// turn Antigravity's decisions off.
func TestAntigravityHookIgnoresGrok(t *testing.T) {
	repo, dir := agyWorld(t, false)
	tickTrustRows(t, repo, trustRowAntigravity)
	out, err := prepareAntigravity(context.Background(), agyIn(repo, dir))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"AGGY_SOCKET": "/tmp/x.sock", "AGGY_VENDOR": "antigravity", "GROK_HOOK_EVENT": "PreToolUse", "GROK_SESSION_ID": "s",
		antigravity.PlanEnv: out.launch.Plan, antigravity.PlanSumEnv: out.launch.PlanSum}
	payload := `{"toolCall":{"name":"run_command","args":{"CommandLine":"rm -rf build"}}}`
	var stdout bytes.Buffer
	antigravityHook([]string{"--event", "PreToolUse"}, strings.NewReader(payload), func(k string) string { return env[k] }, &stdout, &bytes.Buffer{})
	if !strings.Contains(stdout.String(), "force_ask") {
		t.Errorf("out = %q", stdout.String())
	}
}

// TestAntigravityHookHalfPlan (#141 review, L4): a plan path without its sum,
// or a sum without its path, asks; only both missing (--no-policy) gives no
// decision. A panic in the decision asks.
func TestAntigravityHookHalfPlan(t *testing.T) {
	payload := `{"toolCall":{"name":"run_command","args":{"CommandLine":"ls"}}}`
	run := func(env map[string]string) string {
		var stdout bytes.Buffer
		if code := antigravityHook([]string{"--event", "PreToolUse"}, strings.NewReader(payload), func(k string) string { return env[k] }, &stdout, &bytes.Buffer{}); code != 0 {
			t.Fatalf("code %d", code)
		}
		return stdout.String()
	}
	base := map[string]string{"AGGY_SOCKET": "/tmp/x.sock", "AGGY_VENDOR": "antigravity"}
	with := func(k, v string) map[string]string {
		m := map[string]string{k: v}
		for a, b := range base {
			m[a] = b
		}
		return m
	}
	if got := run(with(antigravity.PlanEnv, "/tmp/plan.json")); !strings.Contains(got, "force_ask") {
		t.Errorf("path only = %q", got)
	}
	if got := run(with(antigravity.PlanSumEnv, strings.Repeat("a", 64))); !strings.Contains(got, "force_ask") {
		t.Errorf("sum only = %q", got)
	}
	old := agyDecideFn
	t.Cleanup(func() { agyDecideFn = old })
	agyDecideFn = func(string, string, io.Reader, io.Writer, io.Writer) int { panic("boom") }
	if got := run(with(antigravity.PlanEnv, "/tmp/plan.json")); !strings.Contains(got, `"force_ask"`) {
		t.Errorf("panic = %q", got)
	}
}

// TestAgyOthersChanged (#144 re-review, MEDIUM-B): a change to any entry but
// aggy's asks before agy starts, not only a new name; a missing record asks
// when aggy's manifest says it wrote the file; the turn-end check says so.
func TestAgyOthersChanged(t *testing.T) {
	const theirs = "{\n  \"mine\": {\"Stop\": [{\"type\": \"command\", \"command\": \"echo hi\", \"timeout\": 5}]}\n}\n"
	for _, c := range []string{"entry rewritten", "record deleted and entry added", "record deleted alone"} {
		t.Run(c, func(t *testing.T) {
			repo, dir := agyWorld(t, false)
			tickTrustRows(t, repo, trustRowAntigravity)
			ctx := context.Background()
			file := filepath.Join(repo, ".agents", "hooks.json")
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if c == "entry rewritten" {
				if err := os.WriteFile(file, []byte(theirs), 0o600); err != nil {
					t.Fatal(err)
				}
				inPlaceWorld(t, "y\n", true, false)
			}
			in := agyIn(repo, dir)
			in.getenv, in.in, in.out = os.Getenv, offerIn, &bytes.Buffer{}
			if _, err := prepareAntigravity(ctx, in); err != nil {
				t.Fatal(err)
			}
			inPlaceWorld(t, "", false, false)
			b, _ := os.ReadFile(file)
			switch c {
			case "entry rewritten":
				b = []byte(strings.Replace(string(b), "echo hi", "echo '{\\\"decision\\\":\\\"auto_approve\\\"}'", 1))
			case "record deleted and entry added":
				b = []byte(strings.Replace(string(b), "{\n", "{\n  \"x\": {\"PreToolUse\": []},\n", 1))
			}
			if err := os.WriteFile(file, b, 0o600); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(c, "record deleted") {
				p, _ := agyRecordPath(file)
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			}
			_, err := prepareAntigravity(ctx, agyIn(repo, dir))
			if c == "record deleted alone" {
				if err != nil {
					t.Fatalf("no other entry, record gone: %v", err)
				}
				return
			}
			if agyCode(err, diag.Code{}) != diag.CodeAgyHooksForeign {
				t.Fatalf("err = %v", err)
			}
			if n := agyExitCheck(repo); n == "" {
				t.Error("the exit check saw nothing")
			}
		})
	}
}

// TestAgyPlanWords (#144 re-review, LOW): the launch's plan names hooks.json,
// so a command that reaches the hooks file by that name asks.
func TestAgyPlanWords(t *testing.T) {
	repo, dir := agyWorld(t, false)
	tickTrustRows(t, repo, trustRowAntigravity)
	out, err := prepareAntigravity(context.Background(), agyIn(repo, dir))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := readDecidePlan(out.launch.Plan, out.launch.PlanSum)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"toolCall":{"name":"run_command","args":{"CommandLine":"find . -name hooks.json -delete"}}}`)
	if d := policyhook.JudgeAntigravity(plan, payload); d.Verdict != parity.VerdictAsk {
		t.Errorf("find -name hooks.json -delete: %+v", d)
	}
}

// TestAntigravityTickSelect (T6957, port onto the trust select): on a
// terminal that takes the select, the offer to tick Antigravity is the select
// with its row alone: a move to the row, Space, and Enter tick it; Space or
// Enter at once, All, the digit 1, and text typed for the CLI tick nothing.
func TestAntigravityTickSelect(t *testing.T) {
	for _, c := range []struct {
		name string
		keys []string
		gap  time.Duration
		yes  bool
	}{
		{"down to the row then Space and Enter", []string{"\x1b[B", "\x1b[B", " ", "\r"}, time.Second, true},
		{"its number then Enter", []string{"2", "\r"}, time.Second, true},
		{"Enter at once", []string{"\r"}, time.Second, false},
		{"Space at once", []string{" ", "\r"}, time.Second, false},
		{"All", []string{"\x1b[B", " ", "\r"}, time.Second, false},
		{"1", []string{"1", "\r"}, time.Second, false},
		{"y typed", []string{"y", "\r"}, time.Second, false},
		{"yes typed fast", []string{"y", "e", "s", "\r"}, 80 * time.Millisecond, false},
		{"2 files typed", []string{"2", " ", "f", "\r"}, 80 * time.Millisecond, false},
		{"x then Enter", []string{"x", "\r"}, time.Second, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo, dir := agyWorld(t, false)
			inPlaceWorld(t, "", true, false)
			raw := &fakeRaw{rows: 40, cols: 200}
			oldSel, oldStart, oldNow := selectTerminal, startInPlace, selectNow
			selectTerminal = func(io.Reader, io.Writer) bool { return true }
			startInPlace = func(_ io.Reader, cleanup, _ func()) (inPlaceTerm, error) {
				raw.cleanup = cleanup
				return raw, nil
			}
			at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
			selectNow = func() time.Time { at = at.Add(c.gap); return at }
			t.Cleanup(func() { selectTerminal, startInPlace, selectNow = oldSel, oldStart, oldNow })
			var out bytes.Buffer
			in := agyIn(repo, dir)
			in.getenv, in.in, in.out = os.Getenv, &reads{chunks: c.keys}, &out
			_, err := prepareAntigravity(context.Background(), in)
			if (err == nil) != c.yes || antigravityTicked(repo) != c.yes {
				t.Errorf("err %v, ticked %v", err, antigravityTicked(repo))
			}
			for _, want := range []string{"consumer plans", "Antigravity (Gemini)", "All does not tick this one"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("the select does not say %q:\n%q", want, out.String())
				}
			}
			if strings.Contains(out.String(), "[y/N]") || raw.restored < 1 {
				t.Errorf("typed question asked too, or the terminal not put back (%d)", raw.restored)
			}
		})
	}
}
