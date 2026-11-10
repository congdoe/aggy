package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	if note := usePolicyHook(&req, hookIn(plan, s, repo, repo, "2.1.322", dir)); note != "" {
		t.Fatalf("note %q", note)
	}
	if req.PolicyHook == nil || !slices.Equal(req.Ask, settingsGuardAsks(repo, repo)) || !slices.Equal(req.PolicyHook.Fallback, asks) {
		t.Fatalf("hook %+v, asks %q", req.PolicyHook, req.Ask)
	}
	// PR #129 review, live check of 2026-11-10: a settings file written
	// mid-session turns hooks off, so static asks guard those writes beside
	// the hook, and the static asks the hook replaced are all kept for when it
	// cannot run.
	for _, r := range []string{"Edit(//**/.claude/settings.local.json)", "Edit(//**/.claude/settings.json)",
		"Edit(/" + filepath.ToSlash(filepath.Join(repo, ".claude", "settings.local.json")) + ")", "Bash(*.claude/settings*)"} {
		if !slices.Contains(req.Ask, r) {
			t.Errorf("static asks lack %s: %q", r, req.Ask)
		}
	}
	js, _, err := claude.SettingsJSON(req)
	if err != nil {
		t.Fatal(err)
	}
	var layer struct {
		Permissions struct{ Ask []string } `json:"permissions"`
	}
	if err := json.Unmarshal(js, &layer); err != nil || !slices.Equal(layer.Permissions.Ask, req.Ask) {
		t.Errorf("rendered asks %q, want %q (%v)", layer.Permissions.Ask, req.Ask, err)
	}
	off := req
	off.UserSettings = map[string]any{"disableAllHooks": true}
	if js, _, err := claude.SettingsJSON(off); err != nil || !strings.Contains(string(js), `"Bash(*.claude/settings*)"`) {
		t.Errorf("hooks off: %s %v", js, err)
	} else if err := json.Unmarshal(js, &layer); err != nil || !slices.Equal(layer.Permissions.Ask, append(slices.Clone(req.Ask), asks...)) {
		t.Errorf("hooks off: asks %q", layer.Permissions.Ask)
	}
	h := req.PolicyHook.Group.Hooks[0]
	if h.OnFailure != "block" || h.Timeout != policyhook.DecideTimeout || req.PolicyHook.Group.Matcher != "*" ||
		!strings.HasPrefix(h.Command, "out=$(/usr/local/bin/aggy hook decide --vendor claude --plan "+filepath.Join(dir, decidePlanFile)+" --sha256 ") {
		t.Errorf("hook entry %+v", h)
	}
	path, sum := planOf(t, req)
	d, err := readDecidePlan(path, sum)
	if err != nil {
		t.Fatal(err)
	}
	if d.VendorVersion != "2.1.322" || d.NoPrompt || !slices.Equal(d.Commands, []string{"git push --force origin main"}) || d.Root != repo || len(d.Protected) != 1 {
		t.Errorf("plan %+v", d)
	}
	if note := policyHookNote(req, d.PreApproved(), plan.Branches); !strings.Contains(note, "1 exact pre-approval") {
		t.Errorf("note %q", note)
	}

	// A mode that shows no prompt makes the plan and the fallback deny.
	req, plan, s = onScreenReq(t, repo)
	req.Extra = []string{"--permission-mode", "dontAsk"}
	if note := usePolicyHook(&req, hookIn(plan, s, repo, repo, "", filepath.Join(t.TempDir(), "m"))); note != "" || req.PolicyHook == nil ||
		!strings.Contains(req.PolicyHook.Group.Hooks[0].Command, `"permissionDecision":"deny"`) {
		t.Errorf("dontAsk: %q %+v", note, req.PolicyHook)
	}

	keeps := func(name string, mut func(*adapter.InteractiveRequest, *policyHookIn)) {
		t.Helper()
		req, plan, s := onScreenReq(t, repo)
		in := hookIn(plan, s, repo, repo, "", filepath.Join(t.TempDir(), "m"))
		mut(&req, &in)
		note := usePolicyHook(&req, in)
		if req.PolicyHook != nil || len(req.Ask) == 0 || !strings.Contains(note, "asks as before") {
			t.Errorf("%s: hook %+v asks %d note %q", name, req.PolicyHook, len(req.Ask), note)
		}
	}
	keeps("--safe-mode", func(req *adapter.InteractiveRequest, _ *policyHookIn) { req.Extra = []string{"--safe-mode"} })
	keeps("--bare", func(req *adapter.InteractiveRequest, _ *policyHookIn) { req.Extra = []string{"--bare"} })
	keeps("hooks off in Claude's settings", func(_ *adapter.InteractiveRequest, in *policyHookIn) { in.rules.HooksOff = "/x" })
	// PR #129 review, HIGH-1: a --settings file would hold the hash too.
	keeps("a --settings file", func(_ *adapter.InteractiveRequest, in *policyHookIn) { in.settingsFile = true })
	// PR #129 review, MEDIUM-3.
	keeps(claudeSimpleVar+" in --settings env", func(req *adapter.InteractiveRequest, _ *policyHookIn) {
		req.UserSettings = map[string]any{"env": map[string]any{claudeSimpleVar: "1"}}
	})
	func() {
		old := policyHookEnv
		defer func() { policyHookEnv = old }()
		policyHookEnv = func(k string) string {
			if k == claudeSimpleVar {
				return "1"
			}
			return ""
		}
		keeps(claudeSimpleVar+" in aggy's env", func(*adapter.InteractiveRequest, *policyHookIn) {})
	}()
	// PR #129 review, MEDIUM-2: managed settings that turn hooks off.
	func() {
		old := managedSettingsPath
		defer func() { managedSettingsPath = old }()
		managed := filepath.Join(t.TempDir(), "managed-settings.json")
		writeAt(t, managed, `{"allowManagedHooksOnly":true}`, 0o600)
		managedSettingsPath = func() string { return managed }
		keeps("managed settings", func(*adapter.InteractiveRequest, *policyHookIn) {})
	}()
	for _, f := range []string{"settings.json", "settings.local.json"} {
		other := tempRepo(t)
		writeAt(t, filepath.Join(other, ".claude", f), `{"disableAllHooks":true}`, 0o600)
		keeps("a repo's "+f, func(_ *adapter.InteractiveRequest, in *policyHookIn) { in.root, in.cwd = other, other })
		bad := tempRepo(t)
		writeAt(t, filepath.Join(bad, ".claude", f), `{not json`, 0o600)
		keeps("a repo's unreadable "+f, func(_ *adapter.InteractiveRequest, in *policyHookIn) { in.root, in.cwd = bad, bad })
		// PR #129 review, MEDIUM-1: a launch in a folder below the root
		// reads that folder's settings, and every one up to the root.
		sub := tempRepo(t)
		writeAt(t, filepath.Join(sub, "a", "b", ".claude", f), `{"disableAllHooks":true}`, 0o600)
		keeps("a sub-folder's "+f, func(_ *adapter.InteractiveRequest, in *policyHookIn) {
			in.root, in.cwd = sub, filepath.Join(sub, "a", "b")
		})
		keeps("a middle folder's "+f, func(_ *adapter.InteractiveRequest, in *policyHookIn) {
			in.root, in.cwd = sub, filepath.Join(sub, "a", "b", "c")
		})
	}
	fakeHookBin(t, "", errors.New("go run"))
	keeps("no aggy path", func(*adapter.InteractiveRequest, *policyHookIn) {})
	fakeHookBin(t, "/a b/aggy", nil)
	keeps("an aggy path a hook cannot hold", func(*adapter.InteractiveRequest, *policyHookIn) {})

	// A headless launch is left as it was.
	var hreq adapter.InteractiveRequest
	if _, err := addPolicy(adapter.Claude, []string{probe.CapSettings}, true, policyPlan(planHeadless, repo, s), &hreq); err != nil {
		t.Fatal(err)
	}
	fakeHookBin(t, "/usr/local/bin/aggy", nil)
	if note := usePolicyHook(&hreq, hookIn(policyPlan(planHeadless, repo, s), s, repo, repo, "", t.TempDir())); note != "" || hreq.PolicyHook != nil {
		t.Errorf("headless: %q %+v", note, hreq.PolicyHook)
	}
}

// TestSettingsGuardAsks: the launch folder and each folder up to the root get
// their own rules; a folder outside the root, or a path with a glob byte,
// only the any-depth ones.
func TestSettingsGuardAsks(t *testing.T) {
	for _, tt := range []struct {
		cwd, root string
		want      []string
	}{
		{"/r/a", "/r", []string{"Edit(//r/a/.claude/settings.json)", "Edit(//r/.claude/settings.local.json)"}},
		{"/other", "/r", nil},
		{"/r/a*b", "/r", []string{"Edit(//r/.claude/settings.json)"}},
	} {
		got := settingsGuardAsks(tt.cwd, tt.root)
		if got[0] != "Edit(//**/.claude/settings.json)" || got[len(got)-1] != "Bash(*.claude/settings*)" {
			t.Errorf("%s: %q", tt.cwd, got)
		}
		for _, w := range tt.want {
			if !slices.Contains(got, w) {
				t.Errorf("%s: lacks %s: %q", tt.cwd, w, got)
			}
		}
		if tt.want == nil && len(got) != 3 {
			t.Errorf("%s: %q", tt.cwd, got)
		}
		if strings.Contains(strings.Join(got, " "), "a*b") {
			t.Errorf("a glob byte reached a rule: %q", got)
		}
	}
}

// hookIn is usePolicyHook's input for a launch in cwd, with no identities.
func hookIn(plan parity.PolicyPlan, s permissions.Set, root, cwd, version, dir string) policyHookIn {
	return policyHookIn{plan: plan, rules: s, root: root, cwd: cwd, version: version, dir: dir}
}

// planArgsRE finds the plan and its sum in a decision hook's command.
var planArgsRE = regexp.MustCompile(`--plan (\S+) --sha256 ([0-9a-f]{64})`)

// planOf is the plan path and sum the hook in req names.
func planOf(t *testing.T, req adapter.InteractiveRequest) (path, sum string) {
	t.Helper()
	if req.PolicyHook == nil {
		t.Fatal("no decision hook")
	}
	m := planArgsRE.FindStringSubmatch(req.PolicyHook.Group.Hooks[0].Command)
	if m == nil {
		t.Fatalf("no plan in %q", req.PolicyHook.Group.Hooks[0].Command)
	}
	return m[1], m[2]
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
	if note := usePolicyHook(&req, hookIn(plan, s, repo, repo, "", dir)); note != "" || req.PolicyHook == nil {
		t.Fatalf("%q %+v", note, req.PolicyHook)
	}
	path, sum := planOf(t, req)
	d, err := readDecidePlan(path, sum)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Commands)+len(d.Reads) != 0 {
		t.Errorf("pre-approved from outside aggy's file: %+v", d)
	}
	var out bytes.Buffer
	stdin := `{"tool_name":"Bash","tool_input":{"command":"git push --force origin main"}}`
	if code := hookWith([]string{"decide", "--vendor", "claude", "--plan", path, "--sha256", sum}, strings.NewReader(stdin),
		os.Getenv, &out, &bytes.Buffer{}); code != 0 || !strings.Contains(out.String(), `"permissionDecision":"ask"`) {
		t.Errorf("hook: %d %q", code, out.String())
	}
}

// TestPlanRewriteAsks (PR #129 review, HIGH-1, confirmed on d991153): the
// model's shell rewrites the launch's plan to pre-approve a force push to main.
// The hook checks the plan against the SHA-256 in its own command, so the push
// still asks, with its own failure code; a removed plan asks too, and a
// dontAsk session refuses. Neither ever allows.
func TestPlanRewriteAsks(t *testing.T) {
	isolate(t)
	freshRun(t)
	repo := inRepo(t)
	fakeHookBin(t, "/usr/local/bin/aggy", nil)
	req, plan, s := onScreenReq(t, repo)
	if note := usePolicyHook(&req, hookIn(plan, s, repo, repo, "", t.TempDir())); note != "" {
		t.Fatalf("note %q", note)
	}
	path, sum := planOf(t, req)
	d, err := readDecidePlan(path, sum)
	if err != nil {
		t.Fatal(err)
	}
	const force = "git push --force origin main"
	d.Commands = append(d.Commands, force)
	evil, _ := json.Marshal(d)
	writeAt(t, path, string(evil), 0o600)
	decide := func(mode string) (int, string) {
		var out bytes.Buffer
		b, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": force}, "permission_mode": mode})
		code := hookWith([]string{"decide", "--vendor", "claude", "--plan", path, "--sha256", sum}, bytes.NewReader(b), os.Getenv, &out, &bytes.Buffer{})
		return code, out.String()
	}
	if code, out := decide("default"); code != 0 || !strings.Contains(out, `"permissionDecision":"ask"`) {
		t.Errorf("rewritten plan: %d %q, want ask", code, out)
	}
	if code, out := decide("dontAsk"); code != 0 || !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("rewritten plan, dontAsk: %d %q, want deny", code, out)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if code, out := decide("default"); code != 0 || !strings.Contains(out, `"permissionDecision":"ask"`) {
		t.Errorf("removed plan: %d %q, want ask", code, out)
	}
	// PR #129 re-review: a call the hook would not judge gets no opinion.
	var out bytes.Buffer
	web := `{"tool_name":"WebFetch","tool_input":{"url":"https://example.com"},"permission_mode":"default"}`
	if code := hookWith([]string{"decide", "--vendor", "claude", "--plan", path, "--sha256", sum}, strings.NewReader(web), os.Getenv, &out, &bytes.Buffer{}); code != 0 || out.Len() != 0 {
		t.Errorf("removed plan, WebFetch: %d %q, want no opinion", code, out.String())
	}
	stats := decideStats(context.Background())
	var failures int64
	for _, st := range stats {
		failures += st.Failures()
	}
	if failures != 4 {
		t.Errorf("failures %d, want 4: %+v", failures, stats)
	}
}

// TestReadPreApprovalIdentity (PR #129 review, MEDIUM-4, confirmed on
// d991153): aggy permissions add allow records a key file's identity; the
// hook allows reading that file only, in a trusted repo; a symlink put in its
// place, pointing at another key, asks; an untrusted repo pre-approves no read.
func TestReadPreApprovalIdentity(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	fakeHookBin(t, "/usr/local/bin/aggy", nil)
	key, other := filepath.Join(repo, "certs", "server.key"), filepath.Join(repo, "other.key")
	writeAt(t, key, "k1", 0o600)
	writeAt(t, other, "k2", 0o600)
	atKeyboard(t, "y\n")
	if code, out, errb := run("permissions", "add", "allow", "Read(./certs/server.key)"); code != 0 || !strings.Contains(out, "trusted") {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	launch := func() (string, string) {
		t.Helper()
		db, err := store.OpenDefault(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		req, plan, s := onScreenReq(t, repo)
		in := hookIn(plan, s, repo, repo, "", t.TempDir())
		in.ids = readIdentities(context.Background(), db)
		if note := usePolicyHook(&req, in); note != "" {
			t.Fatalf("note %q", note)
		}
		return planOf(t, req)
	}
	read := func(path, sum string) string {
		var out bytes.Buffer
		b, _ := json.Marshal(map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": key}})
		hookWith([]string{"decide", "--vendor", "claude", "--plan", path, "--sha256", sum}, bytes.NewReader(b), os.Getenv, &out, &bytes.Buffer{})
		return out.String()
	}
	// Untrusted: no read pre-approval in the plan.
	if out := read(launch()); !strings.Contains(out, `"permissionDecision":"ask"`) {
		t.Errorf("untrusted: %q, want ask", out)
	}
	atKeyboard(t, "y\n")
	if code, out, errb := run("trust", repo); code != 0 {
		t.Fatalf("trust: %d %q %q", code, out, errb)
	}
	path, sum := launch()
	if out := read(path, sum); !strings.Contains(out, `"permissionDecision":"allow"`) {
		t.Errorf("trusted, same file: %q, want allow", out)
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, key); err != nil {
		t.Fatal(err)
	}
	if out := read(path, sum); !strings.Contains(out, `"permissionDecision":"ask"`) {
		t.Errorf("a symlink to another key: %q, want ask", out)
	}
	if out := read(launch()); !strings.Contains(out, `"permissionDecision":"ask"`) {
		t.Errorf("a symlink, at the next launch: %q, want ask", out)
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
	usePolicyHook(&req, hookIn(policyPlan(planHeadless, repo, s), s, repo, repo, "", t.TempDir()))
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

// writePlan writes a decision plan for the hook tests, rooted in a temporary
// folder unless it has a root, and returns its path and sum.
func writePlan(t *testing.T, d policyhook.DecidePlan) (string, string) {
	t.Helper()
	if d.Root == "" {
		d.Root = t.TempDir()
	}
	p, sum, err := writeDecidePlan(t.TempDir(), d)
	if err != nil {
		t.Fatal(err)
	}
	return p, sum
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
	plan, sum := writePlan(t, policyhook.NewDecidePlan("claude", "2.1.322", false, parity.PolicyPlan{OnScreen: true},
		parity.PreApproved{Commands: []string{approved}}))
	args := func(vendor, path string) []string {
		return []string{"decide", "--vendor", vendor, "--plan", path, "--sha256", sum}
	}
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
		code, out := call(args("claude", plan), tt.stdin)
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
	if code, out := call(args("opencode", plan), `{"action":"shell","resources":["`+approved+`"],"effect":"ask"}`); code != 0 || out != "allow\n" {
		t.Errorf("opencode: %d %q", code, out)
	}
	// A missing plan, or one that is not what the launch wrote, asks.
	if code, out := call(args("claude", filepath.Join(t.TempDir(), "none.json")), bash(approved)); code != 0 || !strings.Contains(out, `"permissionDecision":"ask"`) {
		t.Errorf("no plan: %d %q", code, out)
	}
	other, _ := writePlan(t, policyhook.NewDecidePlan("claude", "2.1.322", false, parity.PolicyPlan{OnScreen: true},
		parity.PreApproved{Commands: []string{approved}}))
	if code, out := call(args("opencode", other), `{"action":"shell","resources":["`+approved+`"],"effect":"ask"}`); code != 0 || out != "ask\n" {
		t.Errorf("another plan: %d %q", code, out)
	}
	if code, _ := call([]string{"decide", "--vendor", "codex", "--plan", plan, "--sha256", sum}, ""); code == 0 {
		t.Error("codex took")
	}
	if code, _ := call([]string{"decide", "--vendor", "claude", "--plan", plan}, ""); code == 0 {
		t.Error("no --sha256 took")
	}
	if code, _ := call(append([]string{"decide-failed"}, args("claude", plan)[1:]...), ""); code != 0 {
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
	if len(starts) != 9 || len(ends) != 9 || fallbacks != 1 {
		t.Fatalf("%d starts, %d ends, %d fallbacks; want 9, 9, 1", len(starts), len(ends), fallbacks)
	}
	codes := map[string]int{}
	for id, e := range ends {
		if !starts[id] || e.DurationMS == nil || e.Vendor == "" {
			t.Errorf("end %+v", e)
		}
		var a map[string]any
		_ = json.Unmarshal(e.Attrs, &a)
		if e.Code != diag.CodeDecidePlan.String() && e.Code != diag.CodeDecidePlanChanged.String() && a["vendor_version"] != "2.1.322" {
			t.Errorf("end attrs %v", a)
		}
		codes[e.Code]++
	}
	for code, n := range map[diag.Code]int{diag.CodeDecidePreApproved: 2, diag.CodeDecideAsked: 2, diag.CodeDecideNoMatch: 1, diag.CodeDecideUnreadable: 2, diag.CodeDecidePlan: 1, diag.CodeDecidePlanChanged: 1} {
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
	if calls != 9 || failures != 3 {
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
		store.DecidePlanChangedCode: diag.CodeDecidePlanChanged.String(),
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
	// PR #129 review, LOW-1: an answer printed before a non-zero exit must not
	// reach Claude beside the fallback, as two JSON objects.
	answersThenFails := stub("aggy-answers-fails", `cat >/dev/null; [ "$2" = decide ] && printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}\n'; exit 1`+"\n")
	sum := policyhook.PlanSum([]byte("{}"))
	for _, tt := range []struct {
		bin      string
		noPrompt bool
		want     string
	}{
		{filepath.Join(dir, "missing", "aggy"), false, "ask"},
		{fails, false, "ask"},
		{fails, true, "deny"},
		{answers, false, "allow"},
		{answersThenFails, false, "ask"},
	} {
		cmd, err := policyhook.DecideCommand(tt.bin, plan, sum, tt.noPrompt)
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
