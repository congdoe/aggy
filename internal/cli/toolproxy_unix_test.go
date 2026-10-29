//go:build unix

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
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
	port := freePort(t, bin)
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("ALL_PROXY", "")
	t.Setenv("all_proxy", "")

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

	// MED-3: an HTTP proxy in the environment, not bypassed for loopback.
	t.Setenv("HTTP_PROXY", "http://corp.example:3128")
	stderr.Reset()
	if bad := startToolProxy(context.Background(), adapter.Claude, ".", launch, nil, &stderr, "aggy"); bad.env != "" {
		bad.stop()
		t.Error("a proxy started under HTTP_PROXY")
	}
	if !strings.Contains(stderr.String(), "tool.proxy_env_proxy") {
		t.Errorf("HTTP_PROXY: %s", stderr.String())
	}
	t.Setenv("NO_PROXY", "localhost,127.0.0.1,::1")
	ok := startToolProxy(context.Background(), adapter.Claude, ".", launch, nil, &stderr, "aggy")
	if ok.env == "" {
		t.Fatalf("NO_PROXY covers it: %s", stderr.String())
	}
	t.Setenv("HTTP_PROXY", "")

	// HIGH-2: it dies mid-session; aggy holds its address, records it,
	// offers the resume without it, and says so at the end.
	_ = syscall.Kill(-pidIn(t, filepath.Join(dir, "pid")), syscall.SIGKILL)
	select {
	case <-ok.died:
	case <-time.After(5 * time.Second):
		t.Fatal("died not closed")
	}
	ok.onDied()
	set := map[string]string{"px": "proxy", "other": "mcp"}
	ok.withoutProxy(set)
	if _, has := set["px"]; has || set["other"] != "mcp" {
		t.Errorf("set = %v", set)
	}
	if n := ok.diedNote("aggy"); !strings.Contains(n, "exited during the claude session") || !strings.Contains(n, "held its address") {
		t.Errorf("note = %q", n)
	}
	ok.stop()

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

// TestToolProxyHoldLost (T6780 HIGH-2, round 2): the proxy dies while
// another process keeps its address, so aggy kills the guarded CLI's
// group at once, records tool.proxy_hold_failed, and says why.
func TestToolProxyHoldLost(t *testing.T) {
	bin := fakecli.BuildProxy(t)
	isolate(t)
	t.Setenv("AGGY_SOCKET", "")
	t.Setenv("AGGY_PARENT", "")
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
	}
	t.Chdir(t.TempDir())
	launch, dir := proxyHome(t, bin, "escape", freePort(t, bin))
	var stderr bytes.Buffer
	px := startToolProxy(context.Background(), adapter.Claude, ".", launch, nil, &stderr, "aggy")
	defer px.stop()
	if px.env == "" {
		t.Fatalf("no proxy: %s", stderr.String())
	}
	cli := exec.Command("/bin/sleep", "60")
	cli.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cli.Start(); err != nil {
		t.Fatal(err)
	}
	cliExited := make(chan struct{})
	go func() { _ = cli.Wait(); close(cliExited) }()
	defer func() { _ = syscall.Kill(-cli.Process.Pid, syscall.SIGKILL); <-cliExited }()
	px.guard(cli.Process.Pid, true)
	var esc int
	for deadline := time.Now().Add(10 * time.Second); esc == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(filepath.Join(dir, "escaped")); err == nil {
			esc, _ = strconv.Atoi(string(b))
		}
	}
	if esc <= 1 {
		t.Fatal("the fake never handed off its listener")
	}
	defer func() { _ = syscall.Kill(esc, syscall.SIGKILL); waitGone(t, "the escaped listener", esc) }()
	_ = syscall.Kill(-pidIn(t, filepath.Join(dir, "pid")), syscall.SIGKILL)
	start := time.Now()
	select {
	case <-cliExited:
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("the CLI was killed only after %s", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the CLI still runs while another process has its proxy's address")
	}
	n := px.diedNote("aggy")
	for _, want := range []string{"WARNING: px's proxy stopped and another process took its address", "aggy stopped claude so it would not send your login there", "aggy resume", "tool.proxy_hold_failed"} {
		if !strings.Contains(n, want) {
			t.Errorf("note lacks %q: %s", want, n)
		}
	}
	if text := allDiagText(t, os.Getenv("AGGY_HOME")); !strings.Contains(text, "tool.proxy_hold_failed") || !strings.Contains(text, "tool.proxy.exited") {
		t.Errorf("diag.db: %s", text)
	}
}

// TestToolProxyInterrupted (T6780 round 2 LOW): a signal while the proxy
// comes up ends the launch (nothing starts), never a CLI without it.
func TestToolProxyInterrupted(t *testing.T) {
	bin := fakecli.BuildProxy(t)
	isolate(t)
	t.Setenv("AGGY_SOCKET", "")
	t.Setenv("AGGY_PARENT", "")
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
	}
	t.Chdir(t.TempDir())
	launch, dir := proxyHome(t, bin, "unhealthy", freePort(t, bin))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(700*time.Millisecond, cancel)
	var stderr bytes.Buffer
	px := startToolProxy(ctx, adapter.Claude, ".", launch, nil, &stderr, "aggy")
	defer px.stop()
	if !px.interrupted || px.env != "" || strings.Contains(stderr.String(), "starts without it") {
		t.Errorf("interrupted %v, env %q: %s", px.interrupted, px.env, stderr.String())
	}
	waitGone(t, "the proxy", pidIn(t, filepath.Join(dir, "pid")))
}

// freePort asks the fake proxy for a loopback port nothing listens on.
func freePort(t *testing.T, bin string) int {
	t.Helper()
	out, err := exec.Command(bin, "freeport").Output()
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// proxyHome turns on a user proxy tool in mode for claude, and returns
// the launch folder and the fake's own folder.
func proxyHome(t *testing.T, bin, mode string, port int) (launch, dir string) {
	t.Helper()
	dir = t.TempDir()
	addedAt(t, filepath.Join(os.Getenv("AGGY_HOME"), "tools", "px.toml"), pxTool(bin, mode, port, dir))
	if code, _, errOut := toolRun("px claude\n", true, "on", "px", "--vendor", "claude", "--proxy"); code != 0 {
		t.Fatalf("on: %d %s", code, errOut)
	}
	launch = filepath.Join(t.TempDir(), "launch")
	return launch, dir
}
