//go:build unix

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/policyhook"
)

// pluginHarness loads aggy's OpenCode plugin with a stand-in for OpenCode's
// plugin context (the hooks it registers, as server.js uses them), fires one
// permission evaluate event and then the tool call's execute.before, and
// prints the effect the evaluate hook left and whether the guard let the call
// run. It is not OpenCode: what OpenCode does with the effect is in
// docs/verification.md.
const pluginHarness = `
import { pathToFileURL } from "node:url"
const plugin = (await import(pathToFileURL(process.argv[2]).href)).default
const opts = JSON.parse(process.argv[3])
const hooks = {}
const ctx = {
  options: opts.options,
  location: { directory: opts.directory },
  tool: { hook: async (name, fn) => { hooks["tool." + name] = fn } },
  event: { subscribe: () => ({ [Symbol.asyncIterator]() { return { next: () => new Promise(() => {}) } } }) },
}
if (opts.hookAPI) ctx.permission = { hook: async (name, fn) => { hooks["permission." + name] = fn } }
await plugin.setup(ctx)
const out = { effect: null, guard: "" }
if (opts.evaluate && hooks["permission.evaluate"]) {
  const e = { action: opts.action, resources: [opts.resource], effect: opts.effect }
  await hooks["permission.evaluate"](e)
  out.effect = e.effect
}
try {
  await hooks["tool.execute.before"]({ tool: opts.tool, input: opts.input })
  out.guard = "ran"
} catch (err) {
  out.guard = "refused"
}
console.log(JSON.stringify(out))
process.exit(0)
`

// TestOpenCodePluginStrict (PR #129 review, HIGH-2, and the owner decision of
// 2026-11-10): on a strict OpenCode launch, with a saved Always allow (the
// evaluate event's effect is allow), a force push to main or a private key
// read never runs unasked: with the hook API absent, the guard refuses it;
// with aggy failing, timing out, or its plan removed, the evaluate hook turns
// the allow into an ask, the prompt the person answers; with an exact
// pre-approval it runs. It runs the real plugin under node with the real aggy,
// and is skipped without node.
func TestOpenCodePluginStrict(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	aggy := buildAggy(t)
	_, file, _, _ := runtime.Caller(0)
	server := filepath.Join(filepath.Dir(file), "..", "..", "internal", "adapter", "opencode", "plugin", "server.js")
	home, err := os.MkdirTemp("/tmp", "aggy-oc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	home, _ = filepath.EvalSymlinks(home)
	repo := filepath.Join(home, "repo")
	write := func(p, s string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(repo, "certs", "server.key"), "k", 0o600)
	harness := filepath.Join(home, "harness.mjs")
	write(harness, pluginHarness, 0o600)
	const force = "git push --force origin main"
	plan := func(name string, pre []string) (string, string) {
		d := policyhook.NewDecidePlan("opencode", "", false, parity.PolicyPlan{OnScreen: true}, parity.PreApproved{Commands: pre})
		d.Root = repo
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(home, name)
		write(p, string(b), 0o600)
		return p, policyhook.PlanSum(b)
	}
	plain, plainSum := plan("plain.json", nil)
	approved, approvedSum := plan("approved.json", []string{force})
	wrapper := func(name, onDecide string) string {
		p := filepath.Join(home, name)
		write(p, "#!/bin/sh\nif [ \"$2\" = decide ]; then cat >/dev/null; "+onDecide+"; fi\nexec '"+aggy+"' \"$@\"\n", 0o700)
		return p
	}
	fails, slow := wrapper("aggy-fails", "exit 1"), wrapper("aggy-slow", "sleep 8; exit 0")
	shell := map[string]any{"command": force}
	read := map[string]any{"path": "certs/server.key"}
	for _, tt := range []struct {
		name           string
		bin, plan, sum string
		hookAPI, eval  bool
		action, tool   string
		resource       string
		input          map[string]any
		effect         string
		wantEffect     any
		wantGuard      string
	}{
		{"push, hook API absent", aggy, plain, plainSum, false, true, "shell", "shell", force, shell, "allow", nil, "refused"},
		{"push, evaluate never ran", aggy, plain, plainSum, true, false, "shell", "shell", force, shell, "allow", nil, "refused"},
		{"push, aggy fails", fails, plain, plainSum, true, true, "shell", "shell", force, shell, "allow", "ask", "ran"},
		{"push, aggy times out", slow, plain, plainSum, true, true, "shell", "shell", force, shell, "allow", "ask", "ran"},
		{"push, plan removed", aggy, filepath.Join(home, "gone.json"), plainSum, true, true, "shell", "shell", force, shell, "allow", "ask", "ran"},
		{"push, plan changed", aggy, approved, plainSum, true, true, "shell", "shell", force, shell, "allow", "ask", "ran"},
		{"push, exact pre-approval", aggy, approved, approvedSum, true, true, "shell", "shell", force, shell, "ask", "allow", "ran"},
		{"key read, hook API absent", aggy, plain, plainSum, false, true, "read", "read", "certs/server.key", read, "allow", nil, "refused"},
		{"key read, aggy fails", fails, plain, plainSum, true, true, "read", "read", "certs/server.key", read, "allow", "ask", "ran"},
		{"key read, asked", aggy, plain, plainSum, true, true, "read", "read", "certs/server.key", read, "allow", "ask", "ran"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts, _ := json.Marshal(map[string]any{
				"options":   map[string]string{"aggy": tt.bin, "root": repo, "decide": tt.plan, "sum": tt.sum, "strict": "1"},
				"directory": repo, "hookAPI": tt.hookAPI, "evaluate": tt.eval, "action": tt.action, "resource": tt.resource,
				"effect": tt.effect, "tool": tt.tool, "input": tt.input,
			})
			cmd := exec.Command(node, harness, server, string(opts))
			cmd.Dir = repo
			cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "TMPDIR=" + home}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("node: %v\n%s", err, out)
			}
			var got struct {
				Effect any    `json:"effect"`
				Guard  string `json:"guard"`
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
				t.Fatalf("%q: %v", out, err)
			}
			if got.Effect != tt.wantEffect || got.Guard != tt.wantGuard {
				t.Errorf("effect %v, guard %s; want %v, %s", got.Effect, got.Guard, tt.wantEffect, tt.wantGuard)
			}
		})
	}
}

// TestOpenCodePluginOutside (T6979): on a strict OpenCode launch (trusted,
// not --auto, the evaluate hook there), a read of a file outside the repo,
// whose effect is allow under the trusted repo's read rule, is turned into an
// ask by aggy hook decide, cleared, and let through by the guard. With the
// hook API absent or the evaluate never run, on an untrusted launch, for an
// edit, through a link inside the repo, or for aggy's own state, it is
// refused. The real plugin under node with the real aggy; skipped without
// node.
func TestOpenCodePluginOutside(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	aggy := buildAggy(t)
	_, file, _, _ := runtime.Caller(0)
	server := filepath.Join(filepath.Dir(file), "..", "..", "internal", "adapter", "opencode", "plugin", "server.js")
	home, err := os.MkdirTemp("/tmp", "aggy-oc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	home, _ = filepath.EvalSymlinks(home)
	repo, other := filepath.Join(home, "repo"), filepath.Join(home, "other")
	write := func(p, s string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(repo, "main.go"), "package main", 0o600)
	write(filepath.Join(other, "notes.txt"), "n", 0o600)
	state := filepath.Join(home, ".aggy", "permissions.json")
	write(state, "{}", 0o600)
	if err := os.Symlink(other, filepath.Join(repo, "inside")); err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(home, "harness.mjs")
	write(harness, pluginHarness, 0o600)
	d := policyhook.NewDecidePlan("opencode", "", false, parity.PolicyPlan{OnScreen: true}, parity.PreApproved{})
	d.Root = repo
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(home, "plan.json")
	write(plan, string(b), 0o600)
	sum := policyhook.PlanSum(b)
	notes := filepath.Join(other, "notes.txt")
	for _, tt := range []struct {
		name            string
		strict, hookAPI bool
		eval            bool
		action, tool    string
		path            string
		wantEffect      any
		wantGuard       string
	}{
		{"asked, then read", true, true, true, "read", "read", notes, "ask", "ran"},
		{"asked, by a relative path", true, true, true, "read", "read", "../other/notes.txt", "ask", "ran"},
		{"hook API absent", true, false, true, "read", "read", notes, nil, "refused"},
		{"evaluate never ran", true, true, false, "read", "read", notes, nil, "refused"},
		{"untrusted launch", false, true, true, "read", "read", notes, "ask", "refused"},
		{"an edit", true, true, true, "edit", "edit", notes, "allow", "refused"},
		{"an inside name that leads out", true, true, true, "read", "read", "inside/notes.txt", "allow", "refused"},
		{"aggy's own state", true, true, true, "read", "read", state, "ask", "refused"},
		{"inside the repo", true, true, true, "read", "read", "main.go", "allow", "ran"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			options := map[string]string{"aggy": aggy, "root": repo, "decide": plan, "sum": sum}
			if tt.strict {
				options["strict"] = "1"
			}
			opts, _ := json.Marshal(map[string]any{
				"options": options, "directory": repo, "hookAPI": tt.hookAPI, "evaluate": tt.eval, "action": tt.action,
				"resource": tt.path, "effect": "allow", "tool": tt.tool, "input": map[string]any{"path": tt.path},
			})
			cmd := exec.Command(node, harness, server, string(opts))
			cmd.Dir = repo
			cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "TMPDIR=" + home}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("node: %v\n%s", err, out)
			}
			var got struct {
				Effect any    `json:"effect"`
				Guard  string `json:"guard"`
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
				t.Fatalf("%q: %v", out, err)
			}
			if got.Effect != tt.wantEffect || got.Guard != tt.wantGuard {
				t.Errorf("effect %v, guard %s; want %v, %s", got.Effect, got.Guard, tt.wantEffect, tt.wantGuard)
			}
		})
	}
}
