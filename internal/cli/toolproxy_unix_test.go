//go:build unix

package cli

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/fakecli"
)

func pidIn(t *testing.T, f string) int {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	if pid <= 1 {
		t.Fatalf("pid %q", b)
	}
	return pid
}

func waitGone(t *testing.T, what string, pid int) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("%s (pid %d) outlived the session", what, pid)
		}
	}
}

// TestStartToolProxy (T6780): a launch starts the confirmed proxy for its
// vendor only, gives the CLI exactly its variable, stops its whole group
// at the end, and starts the CLI without it when its port is taken or it
// fails its health check.
func TestStartToolProxy(t *testing.T) {
	bin := fakecli.BuildProxy(t)
	isolate(t)
	t.Setenv("AGGY_SOCKET", "")
	t.Setenv("AGGY_PARENT", "")
	t.Chdir(t.TempDir())
	t.Cleanup(func() { proxyWait = execrun.ProxyWait })
	proxyWait = 3 * time.Second
	port := 30000 + rand.IntN(30000)

	launch, dir := proxyHome(t, bin, "ok", port)
	var stderr bytes.Buffer
	if px := startToolProxy(context.Background(), adapter.Codex, ".", launch, nil, &stderr, "aggy"); px.env != "" {
		t.Errorf("codex got %q", px.env)
	}
	px := startToolProxy(context.Background(), adapter.Claude, ".", launch, nil, &stderr, "aggy")
	defer px.stop()
	if px.env != fmt.Sprintf("ANTHROPIC_BASE_URL=http://127.0.0.1:%d", port) {
		t.Fatalf("env %q; stderr %s", px.env, stderr.String())
	}
	if !strings.Contains(stderr.String(), "login token included") {
		t.Errorf("no notice: %s", stderr.String())
	}
	if _, err := execrun.PTYEnv(nil, append(childEnv("claude", ""), px.env)); err != nil {
		t.Errorf("the on-screen claude CLI cannot get it: %v", err)
	}
	if _, err := execrun.PTYEnv(nil, append(childEnv("codex", ""), px.env)); err == nil {
		t.Error("a codex CLI got claude's proxy variable")
	}
	if _, err := execrun.Env(nil, []string{px.env}); err == nil {
		t.Error("a borrow child's env takes the proxy variable")
	}
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	for _, l := range strings.Split(string(env), "\n") {
		if k, _, _ := strings.Cut(l, "="); strings.HasPrefix(k, "AGGY_") || strings.Contains(k, "BASE_URL") {
			t.Errorf("the proxy got %s", k)
		}
	}

	// Port taken: the running fake holds it, so a second launch refuses.
	stderr.Reset()
	if again := startToolProxy(context.Background(), adapter.Claude, ".", launch, nil, &stderr, "aggy"); again.env != "" {
		again.stop()
		t.Error("a second proxy started on a taken port")
	}
	if !strings.Contains(stderr.String(), "tool.proxy_port_taken") || !strings.Contains(stderr.String(), "starts without it") {
		t.Errorf("port taken: %s", stderr.String())
	}

	pid, child := pidIn(t, filepath.Join(dir, "pid")), pidIn(t, filepath.Join(dir, "child"))
	px.stop()
	waitGone(t, "the proxy", pid)
	waitGone(t, "its child", child)
	if _, err := execrun.PTYEnv(nil, append(childEnv("claude", ""), px.env)); err == nil {
		t.Error("a stopped proxy's variable still passes")
	}

	// Health fails: the CLI starts without it, and nothing is left.
	launch, dir = proxyHome(t, bin, "unhealthy", port)
	stderr.Reset()
	if bad := startToolProxy(context.Background(), adapter.Claude, ".", launch, nil, &stderr, "aggy"); bad.env != "" {
		bad.stop()
		t.Error("an unhealthy proxy was used")
	}
	if !strings.Contains(stderr.String(), "tool.proxy_unhealthy") {
		t.Errorf("unhealthy: %s", stderr.String())
	}
	waitGone(t, "the unhealthy proxy", pidIn(t, filepath.Join(dir, "pid")))
	waitGone(t, "its child", pidIn(t, filepath.Join(dir, "child")))
}

// proxyHome turns on a user proxy tool in mode for claude, and returns
// the launch folder and the fake's own folder.
func proxyHome(t *testing.T, bin, mode string, port int) (launch, dir string) {
	t.Helper()
	dir = t.TempDir()
	writeAt(t, filepath.Join(os.Getenv("AGGY_HOME"), "tools", "px.toml"), pxTool(bin, mode, port, dir), 0o600)
	if code, _, errOut := toolRun("px claude\n", true, "on", "px", "--vendor", "claude"); code != 0 {
		t.Fatalf("on: %d %s", code, errOut)
	}
	launch = filepath.Join(t.TempDir(), "launch")
	return launch, dir
}
