package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/policyhook"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/store"
)

// fakeHookBin swaps the aggy path the decision hook names.
func fakeHookBin(t *testing.T, bin string, err error) {
	t.Helper()
	old := policyHookBin
	t.Cleanup(func() { policyHookBin = old })
	policyHookBin = func() (string, error) { return bin, err }
}

// onScreenReq is an on-screen Claude launch's request in repo, after
// addPolicy, as launchOnce has it.
func onScreenReq(t *testing.T, repo string) (adapter.InteractiveRequest, parity.PolicyPlan, permissions.Set) {
	t.Helper()
	s := loadPermissions(context.Background(), repo, config.Config{})
	plan := policyPlan(planOnScreen, repo, s)
	var req adapter.InteractiveRequest
	if _, err := addPolicy(adapter.Claude, []string{probe.CapSettings}, true, plan, &req); err != nil {
		t.Fatal(err)
	}
	return req, plan, s
}

// TestUsePolicyHook (T6976): an on-screen Claude launch moves its asks to
// aggy's decision hook, whose plan names the launch's settings, the CLI's
// version, and the exact pre-approvals from aggy's file. Any reason the hook
// might not run keeps the static asks.
func TestUsePolicyHook(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	fakeHookBin(t, "/usr/local/bin/aggy", nil)
	atKeyboard(t, "y\n")
	if code, out, errb := run("trust", repo); code != 0 {
		t.Fatalf("trust: %d %q %q", code, out, errb)
	}
	for _, r := range []string{"Bash(git push --force origin main)", "Bash(git push *)"} {
		atKeyboard(t, "y\n")
		if code, out, errb := run("permissions", "add", "allow", r); code != 0 {
			t.Fatalf("add %s: %d %q %q", r, code, out, errb)
		}
	}
	req, plan, s := onScreenReq(t, repo)
	asks := slices.Clone(req.Ask)
	dir := filepath.Join(t.TempDir(), "mem")
	if note := usePolicyHook(&req, plan, s, repo, "2.1.322", dir); note != "" {
		t.Fatalf("note %q", note)
	}
	if req.PolicyHook == nil || len(req.Ask) != 0 || !slices.Equal(req.PolicyHook.Fallback, asks) {
		t.Fatalf("hook %+v, asks %q", req.PolicyHook, req.Ask)
	}
	h := req.PolicyHook.Group.Hooks[0]
	if h.OnFailure != "block" || h.Timeout != policyhook.DecideTimeout || req.PolicyHook.Group.Matcher != policyhook.ClaudeHookMatcher() ||
		!strings.HasPrefix(h.Command, "/usr/local/bin/aggy hook decide --vendor claude --plan "+filepath.Join(dir, decidePlanFile)+" || ") {
		t.Errorf("hook entry %+v", h)
	}
	d, err := readDecidePlan(filepath.Join(dir, decidePlanFile))
	if err != nil {
		t.Fatal(err)
	}
	if d.VendorVersion != "2.1.322" || d.NoPrompt || !slices.Equal(d.Commands, []string{"git push --force origin main"}) {
		t.Errorf("plan %+v", d)
	}
	if note := policyHookNote(req, s, repo, plan.Branches); !strings.Contains(note, "1 exact pre-approval") {
		t.Errorf("note %q", note)
	}

	// A mode that shows no prompt makes the plan and the fallback deny.
	req, plan, s = onScreenReq(t, repo)
	req.Extra = []string{"--permission-mode", "dontAsk"}
	if note := usePolicyHook(&req, plan, s, repo, "", filepath.Join(t.TempDir(), "m")); note != "" || req.PolicyHook == nil ||
		!strings.Contains(req.PolicyHook.Group.Hooks[0].Command, `"permissionDecision":"deny"`) {
		t.Errorf("dontAsk: %q %+v", note, req.PolicyHook)
	}

	keeps := func(name string, mut func(*adapter.InteractiveRequest, *permissions.Set) string) {
		t.Helper()
		req, plan, s := onScreenReq(t, repo)
		r := mut(&req, &s)
		note := usePolicyHook(&req, plan, s, r, "", filepath.Join(t.TempDir(), "m"))
		if req.PolicyHook != nil || len(req.Ask) == 0 || !strings.Contains(note, "asks as before") {
			t.Errorf("%s: hook %+v asks %d note %q", name, req.PolicyHook, len(req.Ask), note)
		}
	}
	keeps("--safe-mode", func(req *adapter.InteractiveRequest, _ *permissions.Set) string {
		req.Extra = []string{"--safe-mode"}
		return repo
	})
	keeps("--bare", func(req *adapter.InteractiveRequest, _ *permissions.Set) string {
		req.Extra = []string{"--bare"}
		return repo
	})
	keeps("hooks off in Claude's settings", func(_ *adapter.InteractiveRequest, s *permissions.Set) string { s.HooksOff = "/x"; return repo })
	for _, f := range []string{"settings.json", "settings.local.json"} {
		other := tempRepo(t)
		writeAt(t, filepath.Join(other, ".claude", f), `{"disableAllHooks":true}`, 0o600)
		keeps("a repo's "+f, func(*adapter.InteractiveRequest, *permissions.Set) string { return other })
		bad := tempRepo(t)
		writeAt(t, filepath.Join(bad, ".claude", f), `{not json`, 0o600)
		keeps("a repo's unreadable "+f, func(*adapter.InteractiveRequest, *permissions.Set) string { return bad })
	}
	fakeHookBin(t, "", errors.New("go run"))
	keeps("no aggy path", func(*adapter.InteractiveRequest, *permissions.Set) string { return repo })
	fakeHookBin(t, "/a b/aggy", nil)
	keeps("an aggy path a hook cannot hold", func(*adapter.InteractiveRequest, *permissions.Set) string { return repo })

	// A headless launch is left as it was.
	var hreq adapter.InteractiveRequest
	if _, err := addPolicy(adapter.Claude, []string{probe.CapSettings}, true, policyPlan(planHeadless, repo, s), &hreq); err != nil {
		t.Fatal(err)
	}
	fakeHookBin(t, "/usr/local/bin/aggy", nil)
	if note := usePolicyHook(&hreq, policyPlan(planHeadless, repo, s), s, repo, "", t.TempDir()); note != "" || hreq.PolicyHook != nil {
		t.Errorf("headless: %q %+v", note, hreq.PolicyHook)
	}
}

// TestNothingInRepoPreApproves (T6976): allows a repo or a model can write
// (the repo's committed and local Claude settings, a permissions file under
// .aggy, the person's Claude settings, which a model's shell can edit too, and
// environment variables) never reach the plan, so the hook asks.
func TestNothingInRepoPreApproves(t *testing.T) {
	h := isolate(t)
	repo := inRepo(t)
	fakeHookBin(t, "/usr/local/bin/aggy", nil)
	atKeyboard(t, "y\n")
	if code, out, errb := run("trust", repo); code != 0 {
		t.Fatalf("trust: %d %q %q", code, out, errb)
	}
	const allows = `{"permissions":{"allow":["Bash(git push --force origin main)","Read(./k.pem)","Read(//etc/k.pem)"]}}`
	for _, p := range []string{
		filepath.Join(repo, ".claude", "settings.json"),
		filepath.Join(repo, ".claude", "settings.local.json"),
		filepath.Join(repo, ".aggy", "permissions.json"),
		filepath.Join(h, ".claude", "settings.json"),
	} {
		writeAt(t, p, allows, 0o600)
	}
	for _, k := range []string{"AGGY_PREAPPROVED", "AGGY_ALLOW", "AGGY_POLICY"} {
		t.Setenv(k, "git push --force origin main")
	}
	req, plan, s := onScreenReq(t, repo)
	dir := t.TempDir()
	if note := usePolicyHook(&req, plan, s, repo, "", dir); note != "" || req.PolicyHook == nil {
		t.Fatalf("%q %+v", note, req.PolicyHook)
	}
	d, err := readDecidePlan(filepath.Join(dir, decidePlanFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Commands)+len(d.Reads) != 0 {
		t.Errorf("pre-approved from outside aggy's file: %+v", d)
	}
	var out bytes.Buffer
	stdin := `{"tool_name":"Bash","tool_input":{"command":"git push --force origin main"}}`
	if code := hookWith([]string{"decide", "--vendor", "claude", "--plan", filepath.Join(dir, decidePlanFile)}, strings.NewReader(stdin),
		os.Getenv, &out, &bytes.Buffer{}); code != 0 || !strings.Contains(out.String(), `"permissionDecision":"ask"`) {
		t.Errorf("hook: %d %q", code, out.String())
	}
}

// TestHeadlessLayerUnchanged (T6976): a headless, print, or reopened-borrow
// Claude launch renders exactly what it rendered before: one inline layer
// with every policy rule a deny, ClaudeDeny in order, held by a golden file.
func TestHeadlessLayerUnchanged(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "claude_headless_layer.golden"))
	if err != nil {
		t.Fatal(err)
	}
	isolate(t)
	repo := inRepo(t)
	fakeHookBin(t, "/usr/local/bin/aggy", nil)
	s := loadPermissions(context.Background(), repo, config.Config{})
	var req adapter.InteractiveRequest
	if _, err := addPolicy(adapter.Claude, []string{probe.CapSettings}, true, policyPlan(planHeadless, repo, s), &req); err != nil {
		t.Fatal(err)
	}
	usePolicyHook(&req, policyPlan(planHeadless, repo, s), s, repo, "", t.TempDir())
	js, _, err := claude.SettingsJSON(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(js)+"\n" != string(golden) {
		t.Errorf("headless layer changed:\n got %s\nwant %s", js, golden)
	}
	want, _ := json.Marshal(map[string]any{"permissions": map[string]any{"deny": parity.ClaudeDeny}})
	if string(js) != string(want) {
		t.Errorf("headless layer is not ClaudeDeny: %s", js)
	}
}

// writePlan writes a decision plan for the hook tests.
func writePlan(t *testing.T, d policyhook.DecidePlan) string {
	t.Helper()
	p, err := writeDecidePlan(t.TempDir(), d)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestHookDecide (T6976): aggy hook decide answers from its plan and logs a
// start and an end per call, with the decision, group, code, CLI version, and
// duration, and none of the command or path. A plan it cannot read exits 1, so
// the wrapper's fallback answers, and decide-failed records that.
func TestHookDecide(t *testing.T) {
	isolate(t)
	freshRun(t)
	home := os.Getenv("AGGY_HOME")
	const canary = "dist-ocelot-canary-38"
	approved := "rm -rf " + canary
	plan := writePlan(t, policyhook.NewDecidePlan("claude", "2.1.322", false, parity.PolicyPlan{OnScreen: true},
		parity.PreApproved{Commands: []string{approved}}))
	call := func(args []string, stdin string) (int, string) {
		var out, errb bytes.Buffer
		code := hookWith(args, strings.NewReader(stdin), func(string) string { return "" }, &out, &errb)
		return code, out.String()
	}
	bash := func(cmd string) string {
		b, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": cmd}, "cwd": "/" + canary})
		return string(b)
	}
	for _, tt := range []struct {
		stdin, want string
	}{
		{bash(approved), "allow"},
		{bash(approved + " && rm -rf /" + canary), "ask"},
		{bash("ls " + canary), ""},
		{`{"tool_name":"Read","tool_input":{"file_path":"/` + canary + `/.env"}}`, "ask"},
		{"not json " + canary, "ask"},
		{strings.Repeat("x", 2<<20), "ask"},
	} {
		code, out := call([]string{"decide", "--vendor", "claude", "--plan", plan}, tt.stdin)
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if tt.want == "" {
			if out != "" {
				t.Errorf("no opinion printed %q", out)
			}
			continue
		}
		if !strings.Contains(out, `"permissionDecision":"`+tt.want+`"`) || strings.Contains(out, canary) {
			t.Errorf("%.60q: %q, want %s", tt.stdin, out, tt.want)
		}
	}
	if code, out := call([]string{"decide", "--vendor", "opencode", "--plan", plan}, `{"action":"shell","resources":["`+approved+`"],"effect":"ask"}`); code != 0 || out != "allow\n" {
		t.Errorf("opencode: %d %q", code, out)
	}
	if code, out := call([]string{"decide", "--vendor", "claude", "--plan", filepath.Join(t.TempDir(), "none.json")}, bash(approved)); code != 1 || out != "" {
		t.Errorf("no plan: %d %q", code, out)
	}
	if code, _ := call([]string{"decide", "--vendor", "codex", "--plan", plan}, ""); code == 0 {
		t.Error("codex took")
	}
	if code, _ := call([]string{"decide-failed", "--vendor", "claude", "--plan", plan}, ""); code != 0 {
		t.Errorf("decide-failed: %d", code)
	}

	if text := allDiagText(t, home); strings.Contains(text, canary) {
		t.Errorf("diag.db holds the command or path:\n%s", text)
	}
	_, exp, _ := runDiagCmd("export", "--jsonl")
	starts, ends := map[string]bool{}, map[string]store.DiagRecord{}
	var fallbacks int
	for _, r := range exportRecords(t, exp) {
		switch r.Event {
		case diag.EvDecideStarted.String():
			starts[r.ID] = true
		case diag.EvDecideEnded.String():
			ends[r.CauseID] = r
		case diag.EvDecideFallback.String():
			fallbacks++
		}
	}
	if len(starts) != 8 || len(ends) != 8 || fallbacks != 1 {
		t.Fatalf("%d starts, %d ends, %d fallbacks; want 8, 8, 1", len(starts), len(ends), fallbacks)
	}
	codes := map[string]int{}
	for id, e := range ends {
		if !starts[id] || e.DurationMS == nil || e.Vendor == "" {
			t.Errorf("end %+v", e)
		}
		var a map[string]any
		_ = json.Unmarshal(e.Attrs, &a)
		if e.Code != diag.CodeDecidePlan.String() && a["vendor_version"] != "2.1.322" {
			t.Errorf("end attrs %v", a)
		}
		codes[e.Code]++
	}
	for code, n := range map[diag.Code]int{diag.CodeDecidePreApproved: 2, diag.CodeDecideAsked: 2, diag.CodeDecideNoMatch: 1, diag.CodeDecideUnreadable: 2, diag.CodeDecidePlan: 1} {
		if codes[code.String()] != n {
			t.Errorf("%s: %d ends, want %d (%v)", code, codes[code.String()], n, codes)
		}
	}
	stats := decideStats(context.Background())
	var calls, failures int64
	for _, s := range stats {
		calls += s.Calls
		failures += s.Failures()
	}
	if calls != 8 || failures != 2 {
		t.Errorf("doctor's stats %+v", stats)
	}
	if fs := decideFindings(adapter.Claude, stats); len(fs) == 0 || !strings.Contains(fs[len(fs)-1].Message, "calls") {
		t.Errorf("findings %+v", fs)
	}
}

// TestStoreDecideNames: the store's copies of the event and code names are
// the registry's.
func TestStoreDecideNames(t *testing.T) {
	for got, want := range map[string]string{
		store.DecideStarted: diag.EvDecideStarted.String(), store.DecideEnded: diag.EvDecideEnded.String(),
		store.DecideFallback: diag.EvDecideFallback.String(), store.DecideFailedCode: diag.CodeDecidePlan.String(),
	} {
		if got != want {
			t.Errorf("store %q, registry %q", got, want)
		}
	}
}

// TestDecideWrapper (T6976): the hook command run by sh, as Claude runs it.
// When aggy is missing or exits non-zero, it prints the fixed fallback, ask (or
// deny where no prompt can show), whatever the payload; when aggy answers,
// that answer is all it prints.
func TestDecideWrapper(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.json")
	writeAt(t, plan, "{}", 0o600)
	stub := func(name, body string) string {
		p := filepath.Join(dir, name)
		writeAt(t, p, "#!/bin/sh\n"+body, 0o700)
		return p
	}
	fails := stub("aggy-fails", "cat >/dev/null; echo partial >&2; exit 3\n")
	answers := stub("aggy-answers", `cat >/dev/null; [ "$2" = decide ] && printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}\n'; exit 0`+"\n")
	for _, tt := range []struct {
		bin      string
		noPrompt bool
		want     string
	}{
		{filepath.Join(dir, "missing", "aggy"), false, "ask"},
		{fails, false, "ask"},
		{fails, true, "deny"},
		{answers, false, "allow"},
	} {
		cmd, err := policyhook.DecideCommand(tt.bin, plan, tt.noPrompt)
		if err != nil {
			t.Fatal(err)
		}
		c := exec.Command("/bin/sh", "-c", cmd)
		c.Stdin = strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"x'; rm -rf / #"}}`)
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%s: %v", tt.bin, err)
		}
		var v struct {
			H struct {
				D string `json:"permissionDecision"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out, &v); err != nil || v.H.D != tt.want {
			t.Errorf("%s (no prompt %v): %q, want %s", filepath.Base(tt.bin), tt.noPrompt, out, tt.want)
		}
	}
}
