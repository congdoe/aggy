//go:build unix

package e2e

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
)

// aggyCode runs aggy (not on a terminal) in the world's home and dir and
// returns its exit code and output.
func (w *world) aggyCode(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(buildAggy(t), args...)
	cmd.Dir = w.cwd
	cmd.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

// aggyIn runs aggy and fails the test unless it succeeds.
func (w *world) aggyIn(t *testing.T, args ...string) {
	t.Helper()
	if code, out := w.aggyCode(t, args...); code != 0 {
		t.Fatalf("aggy %q: exit %d\n%s", args, code, out)
	}
}

// launchRecord runs aggy run <vendor> to its prompt, exits, and returns
// the CLI's record (the last interactive one).
func (w *world) launchRecord(t *testing.T, vendor string) fakecli.Record {
	t.Helper()
	_ = os.Remove(w.record)
	s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "run", vendor)
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	s.exit()
	w.lastOut = s.screen()
	return interactiveRecord(t, w)
}

// traces lists the files under the world's home, outside aggy's state
// dir and the fakes' record, that name the tool's server: a vendor config
// aggy wrote it into.
func (w *world) traces(t *testing.T, server string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(w.home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (p == filepath.Join(w.home, ".aggy") || p == w.bin) {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() && p != w.record {
			if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), server) {
				found = append(found, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// launchLeftovers lists what a launch left in aggy's run folder: its
// private folder and the tool files in it are removed at exit.
func (w *world) launchLeftovers(t *testing.T) []string {
	t.Helper()
	des, err := os.ReadDir(filepath.Join(w.home, ".aggy", "run"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		if strings.HasSuffix(de.Name(), ".memory") {
			out = append(out, de.Name())
		}
	}
	return out
}

// TestToolHookWired: RTK's Claude hook rides in the session's --settings
// layer, next to the policy, through aggy hook-tool; off, it is gone.
func TestToolHookWired(t *testing.T) {
	w := newWorld(t)
	w.aggyIn(t, "tool", "on", "rtk")
	f := w.launchRecord(t, "claude").Flags["--settings"]
	if len(f) != 1 || !strings.Contains(f[0], `"permissions":{"deny":[`) || !strings.Contains(f[0], "/aggy hook-tool --vendor claude rtk 0 ") {
		t.Errorf("--settings %q", f)
	}
	w.aggyIn(t, "tool", "off", "rtk")
	if f := w.launchRecord(t, "claude").Flags["--settings"]; len(f) != 1 || strings.Contains(f[0], "hook-tool") {
		t.Errorf("off: --settings %q", f)
	}
}

// TestToolForOneSession: aggy run --tool wires an off tool for that
// session only.
func TestToolForOneSession(t *testing.T) {
	w := newWorld(t)
	s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "run", "--tool", "context7", "claude")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	s.exit()
	r := interactiveRecord(t, w)
	if f := r.Flags["--mcp-config"]; len(f) != 1 {
		t.Errorf("--tool: --mcp-config %q", f)
	}
	// W76-2: the file holds context7, and only it.
	var file struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(r.MCPConfig, &file); err != nil || len(file.MCPServers) != 1 || file.MCPServers["context7"] == nil {
		t.Errorf("--tool: the --mcp-config file %s (%v)", r.MCPConfig, err)
	}
	if r := w.launchRecord(t, "claude"); slices.Contains(r.Args, "--mcp-config") {
		t.Errorf("--tool outlived its session: %q", r.Args)
	}
}

// TestToolSessionSwitch (W76-2): aggy run --tool on Grok wires nothing
// (gated) and says so; a switch away from that session drops the tool.
func TestToolSessionSwitch(t *testing.T) {
	w := newWorld(t)
	w.aggyIn(t, "setup", "--yes")
	_ = os.Remove(w.record)
	sc := map[string]any{
		"stdout":       "READY>",
		"hook_command": []string{"/bin/sh", "-c", aggySh(t) + " switch \"$1\" >/dev/null 2>&1 || true", "sh"},
		"hooks":        []map[string]any{{"event": "claude", "payload": map[string]any{}}},
	}
	s := w.start(t, sc, "run", "--tool", "context7", "grok")
	s.waitWithin([]byte("-> claude. Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
	deadline := time.Now().Add(timeout)
	for len(interactiveRuns(t, w)) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	if !strings.Contains(s.screen(), "tool.vendor_unverified") {
		t.Errorf("no Grok gate warning:\n%s", s.screen())
	}
	runs := interactiveRuns(t, w)
	if len(runs) != 2 || runs[1].Vendor != "claude" {
		t.Fatalf("runs = %+v", runs)
	}
	if slices.Contains(runs[0].Args, "--agent") {
		t.Errorf("grok got an agent file: %q", runs[0].Args)
	}
	if slices.Contains(runs[1].Args, "--mcp-config") {
		t.Errorf("--tool outlived its session across a switch: %q", runs[1].Args)
	}
}

// TestToolsWiredAtLaunch (T6708): a tool turned on reaches the CLI aggy
// starts on its command line only, for that session, started through
// aggy tool exec; turned off, it leaves no flag, no file, and nothing in
// any vendor config. Grok gets no tools (W71-4).
func TestToolsWiredAtLaunch(t *testing.T) {
	for _, v := range []string{"claude", "codex", "grok"} {
		t.Run(v, func(t *testing.T) {
			w := newWorld(t)
			if v == "grok" {
				if code, out := w.aggyCode(t, "tool", "on", "context7", "--vendor", v); code != 1 || !strings.Contains(out, "tool.vendor_unverified") {
					t.Errorf("on --vendor grok: %d %s", code, out)
				}
				w.aggyIn(t, "tool", "on", "context7")
			} else {
				w.aggyIn(t, "tool", "on", "context7", "--vendor", v)
			}
			r := w.launchRecord(t, v)
			switch v {
			case "claude":
				f := r.Flags["--mcp-config"]
				if len(f) != 1 || !strings.HasPrefix(f[0], filepath.Join(w.home, ".aggy", "run")+"/") || slices.Contains(r.Args, "--strict-mcp-config") {
					t.Errorf("args %q\nterminal: %q", r.Args, w.lastOut)
				} else if _, err := os.Stat(f[0]); !os.IsNotExist(err) {
					t.Errorf("%s left after the session: %v", f[0], err)
				}
			case "codex":
				if !slices.Contains(r.Flags["-c"], `mcp_servers.context7.command="`+buildAggy(t)+`"`) ||
					!slices.ContainsFunc(r.Flags["-c"], func(c string) bool {
						return strings.HasPrefix(c, `mcp_servers.context7.args=["tool", "exec", `) && strings.HasSuffix(c, `"--", "context7-mcp"]`)
					}) {
					t.Errorf("-c %q\nterminal: %q", r.Flags["-c"], w.lastOut)
				}
			case "grok":
				if slices.Contains(r.Args, "--agent") {
					t.Errorf("grok got tools: %q", r.Args)
				}
			}
			if tr := w.traces(t, "context7-mcp"); len(tr) != 0 {
				t.Errorf("vendor config written: %q", tr)
			}
			if lo := w.launchLeftovers(t); len(lo) != 0 {
				t.Errorf("left in run: %q", lo)
			}

			w.aggyIn(t, "tool", "off", "context7")
			r = w.launchRecord(t, v)
			for _, a := range r.Args {
				if strings.Contains(a, "context7") || a == "--mcp-config" || a == "--agent" {
					t.Errorf("an off tool reached %s: %q", v, r.Args)
				}
			}
			if tr := w.traces(t, "context7"); len(tr) != 0 {
				t.Errorf("an off tool left a trace: %q", tr)
			}
		})
	}
}

// probeTool is a user tool whose server is fake-mcp, which dumps its
// arguments, working directory, and environment to the file it is given.
const probeTool = `name = "probe"
description = "e2e probe server"
version = "1.0.0"
scope = "global"

[install]
check = ["{bin}/fake-mcp", "--version"]

[[mcp]]
vendors = ["claude", "codex"]
argv = ["{bin}/fake-mcp", "{dump}"]
env = ["PROBE_HOME"]

[cost]
method = "none"
`

type mcpDump struct {
	Args []string `json:"args"`
	Cwd  string   `json:"cwd"`
	Env  []string `json:"env"`
}

// TestToolShimSwitchResume (W71-2, W71-8): a user tool is off by
// default; on, it reaches Claude after a switch with a brief, the brief
// staying Claude's prompt, through a file whose server is aggy tool
// exec; the server runs in the launch's private folder with only the
// basics and its declared variable; the launch leaves nothing in run;
// a resume wires it again; a repo's tools_off turns it off.
func TestToolShimSwitchResume(t *testing.T) {
	w := newWorld(t)
	w.env = []string{"PROBE_HOME=/opt/probe", "PROBE_SECRET_TOKEN=leak-canary", "NODE_OPTIONS=--require /tmp/x.js"}
	w.aggyIn(t, "setup", "--yes")
	dump := filepath.Join(w.home, "dump.jsonl")
	tools := filepath.Join(w.home, ".aggy", "tools")
	if err := os.MkdirAll(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	def := strings.NewReplacer("{bin}", w.bin, "{dump}", dump).Replace(probeTool)
	if err := os.WriteFile(filepath.Join(tools, "probe.toml"), []byte(def), 0o600); err != nil {
		t.Fatal(err)
	}

	// Off by default.
	if r := w.launchRecord(t, "claude"); len(r.Flags["--mcp-config"]) != 0 {
		t.Fatalf("an off user tool was wired: %q", r.Args)
	}
	w.aggyIn(t, "tool", "on", "probe")

	// Grok asks for a switch to Claude, which starts with a brief.
	_ = os.Remove(w.record)
	sc := map[string]any{
		"stdout":        "READY>",
		"hook_command":  []string{"/bin/sh", "-c", aggySh(t) + " switch --brief \"$1\" >/dev/null 2>&1 || true", "sh"},
		"hooks":         []map[string]any{{"event": "claude", "payload": map[string]any{}}},
		"start_mcp":     true,
		"session_files": []map[string]any{{"path": "projects/p/{session_id}.jsonl", "content": "{}"}},
	}
	s := w.start(t, sc, "run", "grok")
	s.waitWithin([]byte("-> claude. Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
	deadline := time.Now().Add(timeout)
	for len(interactiveRuns(t, w)) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	runs := interactiveRuns(t, w)
	if len(runs) != 2 || runs[1].Vendor != "claude" {
		t.Fatalf("runs = %+v", runs)
	}
	c := runs[1]
	f := c.Flags["--mcp-config"]
	if len(f) != 1 || c.Args[0] != "--mcp-config" || !slices.Contains(c.Args, "--") ||
		len(c.Positional) != 1 || !strings.HasPrefix(c.Positional[0], "Read the handoff brief at ") {
		t.Fatalf("claude after the switch: args %q, prompt %q\nterminal: %q", c.Args, c.Positional, s.screen())
	}
	if want := adapter.BriefPrompt(strings.TrimSuffix(strings.TrimPrefix(c.Positional[0], "Read the handoff brief at "), " before anything else, then continue the work it describes.")); c.Positional[0] != want {
		t.Errorf("prompt %q", c.Positional[0])
	}
	launch := filepath.Dir(f[0])
	var file struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(c.MCPConfig, &file); err != nil {
		t.Fatalf("mcp config %s: %v", c.MCPConfig, err)
	}
	p := file.MCPServers["probe"]
	if len(file.MCPServers) != 1 || p.Command != buildAggy(t) || len(p.Args) != 9 ||
		!slices.Equal(p.Args[:4], []string{"tool", "exec", launch, "probe"}) ||
		!slices.Equal(p.Args[6:], []string{"--", filepath.Join(w.bin, "fake-mcp"), dump}) {
		t.Errorf("claude's file: %s", c.MCPConfig)
	}

	// The server ran through the shim: in the launch folder, with only
	// the basics and PROBE_HOME.
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the server never ran: %v", err)
	}
	var d mcpDump
	if err := json.Unmarshal(bytes.SplitN(b, []byte("\n"), 2)[0], &d); err != nil {
		t.Fatal(err)
	}
	if d.Cwd != launch || !slices.Equal(d.Args, []string{dump}) {
		t.Errorf("server cwd %q args %q, want %q", d.Cwd, d.Args, launch)
	}
	basics := []string{"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE"}
	for _, kv := range d.Env {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(basics, k) && kv != "PROBE_HOME=/opt/probe" {
			t.Errorf("server env has %q", kv)
		}
	}
	if !slices.Contains(d.Env, "PROBE_HOME=/opt/probe") || !slices.Contains(d.Env, "HOME="+w.home) {
		t.Errorf("server env %q", d.Env)
	}
	if lo := w.launchLeftovers(t); len(lo) != 0 {
		t.Errorf("left in run: %q", lo)
	}
	if tr := w.traces(t, "fake-mcp"); len(tr) != 0 {
		t.Errorf("vendor config written: %q", tr)
	}

	// A resume wires it again, --mcp-config first.
	_ = os.Remove(w.record)
	s = w.start(t, map[string]any{"stdout": "READY>"})
	s.waitFor([]byte("READY>"))
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	r := interactiveRecord(t, w)
	if r.Vendor != "claude" || len(r.Args) < 4 || r.Args[0] != "--mcp-config" || r.Args[2] != "--resume" {
		t.Errorf("resume args %q", r.Args)
	}

	// A repo's tools_off turns it off there.
	gitInit := exec.Command("git", "init", "-q")
	gitInit.Dir = w.cwd
	gitInit.Env = []string{"HOME=" + w.home, "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1"}
	if out, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(w.cwd, ".aggy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.cwd, ".aggy", "config.toml"), []byte("tools_off = [\"probe\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := w.launchRecord(t, "claude"); len(r.Flags["--mcp-config"]) != 0 {
		t.Errorf("tools_off: %q", r.Args)
	}
}

// TestToolOnResumes (T6729): a tool turned on, from another terminal,
// while a session runs is offered at the turn end as a resume of the
// same session; the switch key restarts the CLI with its resume flag and
// the tool's --mcp-config.
func TestToolOnResumes(t *testing.T) {
	w := newWorld(t)
	sc := map[string]any{
		"stdout":        "READY>",
		"hook_command":  []string{buildAggy(t), "hook", "--vendor", "claude"},
		"hooks":         []map[string]any{{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}"}}},
		"session_files": []map[string]any{{"path": "projects/p/{session_id}.jsonl", "content": "{}"}},
	}
	// rtk is on for this session only: it is the same session, so the
	// resume keeps it.
	s := w.start(t, sc, "run", "--tool", "rtk", "claude")
	s.waitFor([]byte("READY>"))
	s.send([]byte("work\r")) // a turn; the fake's Stop hook ends it
	s.waitFor([]byte("work"))
	code, out := w.aggyCode(t, "tool", "on", "context7", "--vendor", "claude")
	if code != 0 || !strings.Contains(out, "Told 1 running aggy session") {
		t.Fatalf("aggy tool on: %d\n%s", code, out)
	}
	s.waitWithin([]byte("tools +context7, same session -> claude. Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
	deadline := time.Now().Add(timeout)
	for len(interactiveRuns(t, w)) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	runs := interactiveRuns(t, w)
	if len(runs) != 2 || runs[1].Vendor != "claude" {
		t.Fatalf("runs = %+v\nterminal: %q", runs, s.screen())
	}
	id := runs[0].Flags["--session-id"]
	if len(id) != 1 || !slices.Equal(runs[1].Flags["--resume"], id) {
		t.Errorf("the resume's args %q: want --resume %q", runs[1].Args, id)
	}
	if len(runs[0].Flags["--mcp-config"]) != 0 || len(runs[1].Flags["--mcp-config"]) != 1 {
		t.Errorf("--mcp-config: %q, then %q", runs[0].Args, runs[1].Args)
	}
	for i, r := range runs {
		if f := r.Flags["--settings"]; len(f) != 1 || !strings.Contains(f[0], " hook-tool --vendor claude rtk 0 ") {
			t.Errorf("run %d: --tool rtk's hook is not in --settings %q", i, f)
		}
	}
	if runs[1].Env["AGGY_PARENT"] != runs[0].Env["AGGY_PARENT"] {
		t.Errorf("another parent: %v", runs[1].Env)
	}
}
