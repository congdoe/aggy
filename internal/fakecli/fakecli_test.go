package fakecli

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

type result struct {
	stdout, stderr string
	code           int
}

type env struct {
	fakes  Fakes
	home   string
	record string
	cwd    string
	vars   []string
}

func newEnv(t *testing.T, fakes Fakes) *env {
	t.Helper()
	home := t.TempDir()
	cwd := t.TempDir()
	// Resolve /var -> /private/var on macOS so paths match the fake's cwd.
	cwd, _ = filepath.EvalSymlinks(cwd)
	e := &env{fakes: fakes, home: home, cwd: cwd, record: filepath.Join(t.TempDir(), "record.jsonl")}
	e.vars = []string{"HOME=" + home, "PATH=/usr/bin:/bin", EnvRecord + "=" + e.record}
	return e
}

func (e *env) run(t *testing.T, vendor, scenario, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(e.fakes.For(vendor), args...)
	cmd.Dir = e.cwd
	cmd.Env = append([]string{}, e.vars...)
	if scenario != "" {
		cmd.Env = append(cmd.Env, EnvScenario+"="+WriteScenario(t, scenario))
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{out.String(), errb.String(), code}
}

const abc = "0b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e"

func TestFakes(t *testing.T) {
	fakes := Build(t)

	t.Run("version and help", func(t *testing.T) {
		e := newEnv(t, fakes)
		for v, want := range map[string]string{"claude": "2.1.311", "codex": "0.164.0", "grok": "1.0.70"} {
			if r := e.run(t, v, "", "", "--version"); r.code != 0 || !strings.Contains(r.stdout, want) {
				t.Errorf("%s --version = %+v", v, r)
			}
			if r := e.run(t, v, "", "", "--help"); r.code != 0 || !strings.Contains(r.stdout, "Options:") {
				t.Errorf("%s --help = %+v", v, r)
			}
		}
		r := e.run(t, "codex", `{"version_output":"codex-cli 9.9.9\n"}`, "", "--version")
		if r.stdout != "codex-cli 9.9.9\n" {
			t.Errorf("version_output = %q", r.stdout)
		}
		r = e.run(t, "codex", "", "", "exec", "--help")
		if !strings.Contains(r.stdout, "--json") {
			t.Errorf("codex exec --help lacks --json: %q", r.stdout)
		}
	})

	t.Run("unknown flag fails", func(t *testing.T) {
		e := newEnv(t, fakes)
		r := e.run(t, "claude", "", "", "-p", "--bogus")
		if r.code != 2 || r.stderr != "error: unknown option '--bogus'\n" {
			t.Errorf("unknown flag = %+v", r)
		}
		// codex exec has no --search; clap's message.
		if r := e.run(t, "codex", "", "", "exec", "--search"); r.code != 2 || r.stderr != "error: unexpected argument '--search' found\n" {
			t.Errorf("codex exec --search = %+v", r)
		}
		// extra_flags lets a test allow an unrecorded flag.
		if r := e.run(t, "claude", `{"extra_flags":["--model"]}`, "hi", "-p", "--model", "m"); r.code != 0 {
			t.Errorf("extra flag = %+v", r)
		}
	})

	t.Run("claude headless json", func(t *testing.T) {
		e := newEnv(t, fakes)
		id := "11111111-2222-4333-8444-555555555555"
		r := e.run(t, "claude", `{"json":{"result":"done","session_id":"{session_id}"}}`, "the prompt",
			"-p", "--output-format", "json", "--permission-mode", "plan", "--session-id", id)
		var got map[string]any
		if err := json.Unmarshal([]byte(r.stdout), &got); err != nil || got["session_id"] != id || got["result"] != "done" {
			t.Fatalf("stdout = %q (%v)", r.stdout, err)
		}
		recs := Records(t, e.record)
		if len(recs) != 1 || recs[0].Mode != "headless" || recs[0].Stdin != "the prompt" || recs[0].SessionID != id {
			t.Errorf("records = %+v", recs)
		}
		if recs[0].Flags["--permission-mode"][0] != "plan" {
			t.Errorf("flags = %v", recs[0].Flags)
		}
	})

	t.Run("resume and fork", func(t *testing.T) {
		e := newEnv(t, fakes)
		e.run(t, "grok", `{"session_id":"new-id"}`, "next", "-p", "--resume", "old-id")
		e.run(t, "grok", `{"session_id":"new-id"}`, "next", "-p", "--resume", "old-id", "--fork-session")
		e.run(t, "codex", "", "next step", "exec", "--json", "resume", "thread-9")
		recs := Records(t, e.record)
		if recs[0].SessionID != "old-id" || recs[1].SessionID != "new-id" || recs[2].SessionID != "thread-9" {
			t.Errorf("session ids = %s %s %s", recs[0].SessionID, recs[1].SessionID, recs[2].SessionID)
		}
	})

	t.Run("grok images", func(t *testing.T) {
		e := newEnv(t, fakes)
		grokHome := t.TempDir()
		e.vars = append(e.vars, "GROK_HOME="+grokHome)
		r := e.run(t, "grok", `{"images":2,"session_id":"s1","json":{"text":"saved images/1.jpg"}}`, "draw",
			"-p", "--output-format", "json", "--tools", "image_gen")
		var got map[string]any
		if err := json.Unmarshal([]byte(r.stdout), &got); err != nil || got["sessionId"] != "s1" {
			t.Fatalf("stdout = %q", r.stdout)
		}
		dir := filepath.Join(grokHome, "sessions", url.QueryEscape(e.cwd), "s1", "images")
		for _, n := range []string{"1.jpg", "2.jpg"} {
			if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
				t.Errorf("image %s: %v", n, err)
			}
		}
		if _, err := os.Stat(filepath.Join(e.home, ".grok")); err == nil {
			t.Error("wrote ~/.grok although GROK_HOME was set")
		}
	})

	t.Run("codex exec json and images", func(t *testing.T) {
		e := newEnv(t, fakes)
		r := e.run(t, "codex", `{"images":2,"session_id":"t-1","events":[{"type":"item.completed","text":"{session_id}"}]}`, "draw",
			"exec", "--json", "--sandbox", "read-only", "-c", `web_search="live"`,
			"--disable", "shell_tool", "--disable", "unified_exec", "-i", "ref.png")
		lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
		if len(lines) != 2 || lines[0] != `{"thread_id":"t-1","type":"thread.started"}` || !strings.Contains(lines[1], `"t-1"`) {
			t.Fatalf("stdout = %q", r.stdout)
		}
		for _, n := range []string{"1.png", "2.png"} {
			if _, err := os.Stat(filepath.Join(e.home, ".codex", "generated_images", "t-1", n)); err != nil {
				t.Errorf("image %s: %v", n, err)
			}
		}
		rec := Records(t, e.record)[0]
		if got := rec.Flags["--disable"]; len(got) != 2 {
			t.Errorf("--disable = %v", got)
		}
	})

	t.Run("hooks", func(t *testing.T) {
		e := newEnv(t, fakes)
		sc := `{"stdout":"hello from the fake\n","hook_command":["` + fakes.Hook + `","hook"],
			"hooks":[{"event":"Stop","payload":{"session_id":"{session_id}","stop_hook_active":false}}],
			"interactive_without_tty":true}`
		r := e.run(t, "claude", sc, "", "--session-id", abc)
		if r.code != 0 || r.stdout != "hello from the fake\n" {
			t.Fatalf("interactive = %+v", r)
		}
		recs := Records(t, e.record)
		if len(recs) != 2 || recs[1].Vendor != "hook" {
			t.Fatalf("records = %+v", recs)
		}
		if got := recs[1].Args; strings.Join(got, " ") != "hook Stop" {
			t.Errorf("hook args = %v", got)
		}
		if !strings.Contains(recs[1].Stdin, `"session_id":"`+abc+`"`) {
			t.Errorf("hook payload = %q", recs[1].Stdin)
		}
	})

	t.Run("rate limit raises StopFailure", func(t *testing.T) {
		for _, v := range []string{"claude", "grok"} {
			e := newEnv(t, fakes)
			sc := `{"error":"rate_limit","hook_command":["` + fakes.Hook + `"]}`
			r := e.run(t, v, sc, "task", "-p")
			if r.code == 0 {
				t.Errorf("%s: exit 0 on a limit", v)
			}
			recs := Records(t, e.record)
			if len(recs) != 2 || strings.Join(recs[1].Args, " ") != "StopFailure" || !strings.Contains(recs[1].Stdin, `"rate_limit"`) {
				t.Errorf("%s: records = %+v", v, recs)
			}
		}
	})

	t.Run("codex usage limit", func(t *testing.T) {
		e := newEnv(t, fakes)
		sc := `{"error":"usage_limit_reached","error_event":{"type":"error","code":"usage_limit_reached"}}`
		r := e.run(t, "codex", sc, "task", "exec", "--json")
		if r.code != 1 || !strings.Contains(r.stderr, "You've hit your usage limit") || !strings.Contains(r.stdout, "usage_limit_reached") {
			t.Errorf("usage limit = %+v", r)
		}
		if len(Records(t, e.record)) != 1 {
			t.Error("codex has no failure hook, but a hook ran")
		}
	})

	t.Run("session files", func(t *testing.T) {
		e := newEnv(t, fakes)
		sc := `{"session_id":"s9","session_files":[{"path":"sessions/{session_id}.jsonl","content":"{\"id\":\"{session_id}\"}\n"}]}`
		e.run(t, "codex", sc, "task", "exec")
		b, err := os.ReadFile(filepath.Join(e.home, ".codex", "sessions", "s9.jsonl"))
		if err != nil || string(b) != `{"id":"s9"}`+"\n" {
			t.Errorf("session file = %q, %v", b, err)
		}
		r := e.run(t, "codex", `{"session_files":[{"path":"../escape","content":"x"}]}`, "task", "exec")
		if r.code == 0 {
			t.Error("session file outside the vendor home was written")
		}
	})

	t.Run("codex exec reads stdin to EOF", func(t *testing.T) {
		e := newEnv(t, fakes)
		e.run(t, "codex", "", "piped prompt", "exec")
		if rec := Records(t, e.record)[0]; rec.Stdin != "piped prompt" || rec.StdinTerminal {
			t.Errorf("record = %+v", rec)
		}
	})

	t.Run("refuses the real home", func(t *testing.T) {
		u, err := user.Current()
		if err != nil || u.HomeDir == "" {
			t.Skip("no user home")
		}
		e := newEnv(t, fakes)
		e.vars[0] = "HOME=" + u.HomeDir
		// The fake checks HOME before it reads or writes anything.
		if r := e.run(t, "claude", "", "", "-p"); r.code != 97 {
			t.Errorf("run with the real HOME = %+v", r)
		}
		e.vars[0] = "HOME=" + e.home
		e.vars = append(e.vars, "CODEX_HOME="+filepath.Join(u.HomeDir, ".codex"))
		if r := e.run(t, "codex", "", "", "exec"); r.code != 97 {
			t.Errorf("run with the real CODEX_HOME = %+v", r)
		}
	})
}
