//go:build unix

package execrun

import (
	"context"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestPTYEnvOpenCode holds the changed rule: only an OpenCode launch may
// set these, and it removes what would change its rules or providers.
func TestPTYEnvOpenCode(t *testing.T) {
	base := []string{"HOME=/h", "PATH=/bin", "TERM=xterm",
		"OPENCODE_CONFIG_PROJECT_DISABLE=0", "OPENCODE_CONFIG=/evil", "OPENCODE_DB=/x.db", "OPENCODE_PASSWORD=p", "OPENCODE_CLI_CONFIG_CONTENT={}",
		"ANTHROPIC_API_KEY=k", "OPENAI_API_KEY=k", "XAI_API_KEY=k", "CLOUDFLARE_API_TOKEN=k", "ANTHROPIC_AUTH_TOKEN=k",
		"GIT_CONFIG_COUNT=5", "GIT_CONFIG_PARAMETERS='x=y'", "GIT_AUTHOR_NAME=me", "PWD=/elsewhere"}
	extra := []string{"AGGY_VENDOR=opencode", "OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_CONFIG=/l/c.json", "OPENCODE_CONFIG_DIR=/l/d",
		"OPENCODE_DISABLE_AUTOUPDATE=1", "PWD=/repo", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=false"}
	env, err := PTYEnv(base, extra)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"OPENCODE_CONFIG_PROJECT_DISABLE=", "OPENCODE_CONFIG=/evil", "OPENCODE_DB=", "OPENCODE_PASSWORD=", "OPENCODE_CLI_CONFIG_CONTENT=",
		"ANTHROPIC_API_KEY=", "OPENAI_API_KEY=", "XAI_API_KEY=", "CLOUDFLARE_API_TOKEN=", "ANTHROPIC_AUTH_TOKEN=",
		"GIT_CONFIG_COUNT=5", "GIT_CONFIG_PARAMETERS=", "PWD=/elsewhere"} {
		if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, gone) }) {
			t.Errorf("%s survived: %q", gone, env)
		}
	}
	for _, kept := range []string{"HOME=/h", "PATH=/bin", "TERM=xterm", "GIT_AUTHOR_NAME=me"} {
		if !slices.Contains(env, kept) {
			t.Errorf("the user's own %s was dropped", kept)
		}
	}
	for _, kv := range extra {
		if !slices.Contains(env, kv) {
			t.Errorf("%s was not set", kv)
		}
	}
}

func TestPTYEnvRefuses(t *testing.T) {
	cases := []struct {
		name  string
		extra []string
	}{
		{"another vendor sets OpenCode's config", []string{"AGGY_VENDOR=claude", "OPENCODE_CONFIG=/x"}},
		{"no vendor named", []string{"OPENCODE_CONFIG=/x"}},
		{"OpenCode with a key", []string{"AGGY_VENDOR=opencode", "XAI_API_KEY=k"}},
		{"OpenCode with a base url", []string{"AGGY_VENDOR=opencode", "OPENAI_BASE_URL=http://x"}},
		{"OpenCode with the content variable", []string{"AGGY_VENDOR=opencode", "OPENCODE_CONFIG_CONTENT={}"}},
		{"OpenCode turning project config on", []string{"AGGY_VENDOR=opencode", "OPENCODE_CONFIG_PROJECT_DISABLE=0"}},
		{"OpenCode with another git variable", []string{"AGGY_VENDOR=opencode", "GIT_CONFIG_KEY_1=x"}},
		{"another vendor sets PWD", []string{"AGGY_VENDOR=grok", "PWD=/x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := PTYEnv(nil, c.extra); err == nil {
				t.Fatalf("%q passed", c.extra)
			}
		})
	}
}

// TestPTYEnvOthersKeepTheirs: the other vendors' sessions are as before.
func TestPTYEnvOthersKeepTheirs(t *testing.T) {
	base := []string{"ANTHROPIC_API_KEY=k", "OPENCODE_CONFIG=/x", "GIT_CONFIG_COUNT=2"}
	env, err := PTYEnv(base, []string{"AGGY_VENDOR=claude"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range base {
		if !slices.Contains(env, kv) {
			t.Errorf("%s was dropped for claude", kv)
		}
	}
}

func TestOpenCodeRunner(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/d")
	t.Setenv("OPENCODE_CONFIG", "/evil")
	t.Setenv("XAI_API_KEY", "k")
	dir := t.TempDir()
	run := OpenCodeRunner(10 * time.Second)
	out, err := run(context.Background(), adapter.Command{Path: "/usr/bin/env", Dir: dir, Env: []string{"OPENCODE_DISABLE_PROJECT_CONFIG=1", "PWD=" + dir}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out.Stdout)
	for _, want := range []string{"XDG_DATA_HOME=/d", "OPENCODE_DISABLE_PROJECT_CONFIG=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %s in %s", want, got)
		}
	}
	for _, not := range []string{"OPENCODE_CONFIG=", "XAI_API_KEY"} {
		if strings.Contains(got, not) {
			t.Errorf("%s reached it: %s", not, got)
		}
	}
	for _, bad := range []string{"XAI_API_KEY=k", "OPENCODE_CONFIG_PROJECT_DISABLE=0", "AGGY_HOME=/x"} {
		if _, err := run(context.Background(), adapter.Command{Path: "/usr/bin/env", Dir: dir, Env: []string{bad}}); err == nil {
			t.Errorf("%s was passed", bad)
		}
	}
}

// TestDirectKillsOpenCode (M7): with --direct too, a signal OpenCode ignores
// does not leave it running; another CLI still decides for itself.
func TestDirectKillsOpenCode(t *testing.T) {
	old := directKillWait
	t.Cleanup(func() { directKillWait = old })
	directKillWait = 50 * time.Millisecond
	ignores := []string{"-c", `trap "" TERM HUP; while :; do sleep 0.05; done`}
	done := make(chan int, 1)
	go func() {
		code, _ := RunDirectStarted(adapter.Command{Path: "/bin/sh", Args: ignores, Dir: t.TempDir(), Env: []string{"AGGY_VENDOR=opencode"}}, func(int) {
			time.Sleep(50 * time.Millisecond) // for its trap to be set
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		})
		done <- code
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("it ran on")
	}
}
