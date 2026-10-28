package execrun

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
)

type fixture struct {
	fakes  fakecli.Fakes
	record string
}

func setup(t *testing.T) fixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	// The runner passes these to the CLI: never let a real one through.
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		t.Setenv(v, "")
	}
	return fixture{fakes: fakecli.Build(t), record: filepath.Join(t.TempDir(), "rec.jsonl")}
}

// runner is ExecRunner with the fake's scenario and record file passed
// through the scrubbed environment.
func (f fixture) runner(t *testing.T, scenario string, timeout time.Duration) adapter.Runner {
	t.Helper()
	extra := []string{fakecli.EnvRecord + "=" + f.record}
	if scenario != "" {
		extra = append(extra, fakecli.EnvScenario+"="+fakecli.WriteScenario(t, scenario))
	}
	real := ExecRunner(timeout)
	return func(ctx context.Context, c adapter.Command) (adapter.Output, error) {
		c.Env = append(c.Env, extra...)
		return real(ctx, c)
	}
}

func TestRunnerScrubsEnvAndStdin(t *testing.T) {
	f := setup(t)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_BASE_URL", "http://proxy")
	t.Setenv("SOME_OTHER_VAR", "x")
	run := f.runner(t, "", 10*time.Second)
	out, err := run(context.Background(), adapter.Command{Path: f.fakes.Codex, Args: []string{"exec", "a prompt"}})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("run = %+v, %v", out, err)
	}
	recs := fakecli.Records(t, f.record)
	if len(recs) != 1 {
		t.Fatalf("records = %+v", recs)
	}
	r := recs[0]
	// codex exec read /dev/null to EOF instead of waiting.
	if r.StdinTerminal || r.Stdin != "" {
		t.Errorf("stdin = %q (terminal %v)", r.Stdin, r.StdinTerminal)
	}
	for _, k := range []string{"OPENAI_API_KEY", "ANTHROPIC_BASE_URL", "SOME_OTHER_VAR"} {
		if slices.Contains(r.EnvKeys, k) {
			t.Errorf("%s reached the CLI", k)
		}
	}
	for _, k := range []string{"HOME", "TERM", "NO_COLOR"} {
		if !slices.Contains(r.EnvKeys, k) {
			t.Errorf("%s missing from the CLI's env", k)
		}
	}
}

func TestRunnerRefusals(t *testing.T) {
	f := setup(t)
	run := ExecRunner(5 * time.Second)
	ctx := context.Background()
	for name, c := range map[string]adapter.Command{
		"bare name":      {Path: "codex"},
		"relative":       {Path: "./fake-codex"},
		"unclean":        {Path: f.fakes.Dir + "/../x/fake-codex"},
		"terminal stdin": {Path: f.fakes.Codex, Stdin: adapter.StdinTerminal},
		"api key":        {Path: f.fakes.Codex, Env: []string{"XAI_API_KEY=x"}},
		"base url":       {Path: f.fakes.Codex, Env: []string{"OPENAI_BASE_URL=http://x"}},
		"bad env":        {Path: f.fakes.Codex, Env: []string{"NOEQUALS"}},
	} {
		if _, err := run(ctx, c); err == nil {
			t.Errorf("%s: run succeeded", name)
		}
	}
	if len(fakecli.Records(t, f.record)) != 0 {
		t.Error("a refused command ran")
	}
}

// TestGitRunnerStdin (N53-9): the git runner feeds bytes from memory as
// stdin, still with the scrubbed environment, keeps up to its limit, and
// never hangs on a child that does not read them; every other runner
// refuses bytes, and the git runner refuses a file or terminal.
func TestGitRunnerStdin(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	extra := []string{fakecli.EnvRecord + "=" + f.record}
	git := GitRunner(10*time.Second, 2*MaxOutput)
	out, err := git(ctx, adapter.Command{Path: f.fakes.Codex, Args: []string{"exec", "a prompt"}, Env: extra,
		Stdin: adapter.StdinBytes, StdinData: []byte("base tree\n")})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("run = %+v, %v", out, err)
	}
	if recs := fakecli.Records(t, f.record); len(recs) != 1 || recs[0].Stdin != "base tree\n" {
		t.Errorf("records = %+v", recs)
	}
	// 8 MiB the child never reads: the run still ends.
	start := time.Now()
	if _, err := git(ctx, adapter.Command{Path: f.fakes.Claude, Args: []string{"--version"}, Env: extra,
		Stdin: adapter.StdinBytes, StdinData: make([]byte, 8<<20)}); err != nil || time.Since(start) > 8*time.Second {
		t.Errorf("unread stdin: %v after %s", err, time.Since(start))
	}
	big := strings.Repeat("x", MaxOutput+100)
	sc := []string{fakecli.EnvScenario + "=" + fakecli.WriteScenario(t, `{"stdout":"`+big+`"}`)}
	if out, err := git(ctx, adapter.Command{Path: f.fakes.Grok, Args: []string{"-p", "x"}, Env: append(sc, extra...)}); err != nil || len(out.Stdout) != MaxOutput+100 {
		t.Errorf("limit: %d bytes, %v", len(out.Stdout), err)
	}
	for name, run := range map[string]adapter.Runner{"exec": ExecRunner(time.Second), "check": CheckRunner(time.Second),
		"headless": HeadlessRunner(time.Second), "notify": NotifyRunner(time.Second)} {
		if _, err := run(ctx, adapter.Command{Path: f.fakes.Codex, Dir: t.TempDir(), Stdin: adapter.StdinBytes, StdinData: []byte("x")}); err == nil {
			t.Errorf("%s runner took stdin bytes", name)
		}
	}
	for _, s := range []adapter.Stdin{adapter.StdinFile, adapter.StdinTerminal} {
		if _, err := git(ctx, adapter.Command{Path: f.fakes.Codex, Stdin: s, StdinPath: f.record}); err == nil {
			t.Errorf("git runner took stdin %d", s)
		}
	}
}

func TestRunnerTimeout(t *testing.T) {
	f := setup(t)
	run := f.runner(t, `{"sleep_ms":5000}`, 200*time.Millisecond)
	start := time.Now()
	_, err := run(context.Background(), adapter.Command{Path: f.fakes.Claude, Args: []string{"-p", "a prompt"}})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("run = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("timeout took %s", d)
	}
}

func TestRunnerExitCodeAndCap(t *testing.T) {
	f := setup(t)
	big := strings.Repeat("x", MaxOutput+100)
	run := f.runner(t, `{"stdout":"`+big+`","exit_code":3}`, 10*time.Second)
	out, err := run(context.Background(), adapter.Command{Path: f.fakes.Grok, Args: []string{"-p", "x"}})
	if err != nil || out.ExitCode != 3 || len(out.Stdout) != MaxOutput {
		t.Errorf("run = exit %d, %d bytes, %v", out.ExitCode, len(out.Stdout), err)
	}
}

// The notifier gets the session bus and display, and nothing else
// beyond ExecRunner's environment.
func TestNotifyRunnerEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/x")
	t.Setenv("DISPLAY", ":1")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	run := NotifyRunner(5 * time.Second)
	out, err := run(context.Background(), adapter.Command{Path: "/usr/bin/env"})
	if err != nil {
		t.Skipf("no /usr/bin/env: %v", err)
	}
	env := string(out.Stdout)
	for _, k := range []string{"DBUS_SESSION_BUS_ADDRESS=unix:path=/x", "DISPLAY=:1", "HOME="} {
		if !strings.Contains(env, k) {
			t.Errorf("env lacks %s:\n%s", k, env)
		}
	}
	if strings.Contains(env, "OPENAI_API_KEY") {
		t.Error("an API key reached the notifier")
	}
	for name, c := range map[string]adapter.Command{
		"added env": {Path: "/usr/bin/env", Env: []string{"AGGY_VENDOR=x"}},
		"stdin":     {Path: "/usr/bin/env", Stdin: adapter.StdinTerminal},
	} {
		if _, err := run(context.Background(), c); err == nil {
			t.Errorf("%s: run succeeded", name)
		}
	}
}
