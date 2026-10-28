package probe

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
)

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
