//go:build unix

package execrun

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

var bg = context.Background()

// TestListenerOwned (HIGH-1): the owner check matches a listener of the
// given process group and refuses another group's, on this system's own
// path (/proc on Linux, lsof on macOS).
func TestListenerOwned(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	addr := netip.MustParseAddrPort(ln.Addr().String())
	if err := listenerOwned(bg, addr, syscall.Getpgrp()); err != nil {
		t.Errorf("own listener: %v", err)
	}
	if err := listenerOwned(bg, addr, 1); err == nil {
		t.Error("another group's listener passed")
	}
	if err := listenerOwned(bg, freeAddr(t), syscall.Getpgrp()); !errors.Is(err, errNoListener) {
		t.Errorf("no listener: %v", err)
	}
}

// TestProxySquatter (HIGH-1): a process outside the proxy's group that
// holds the port and answers the health check is never registered.
func TestProxySquatter(t *testing.T) {
	spec, dir := fakeProxy(t, "squat")
	p, err := StartProxy(bg, spec, nil)
	sq := filepath.Join(dir, "sq")
	defer func() {
		if b, err := os.ReadFile(filepath.Join(sq, "pid")); err == nil {
			pid, _ := strconv.Atoi(string(b))
			_ = syscall.Kill(pid, syscall.SIGKILL)
			gone(t, "the squatter", pid)
		}
	}()
	if !errors.Is(err, ErrProxyNotOwned) || p != nil {
		t.Fatalf("err = %v", err)
	}
	gone(t, "the proxy", readPid(t, filepath.Join(dir, "pid")))
	if _, err := PTYEnv(nil, []string{"AGGY_VENDOR=claude", "ANTHROPIC_BASE_URL=http://" + spec.Addr.String()}); err == nil {
		t.Error("the squatter's address passes")
	}
}

// TestProxyDied (HIGH-2): a proxy that dies mid-session leaves its
// address held by aggy's placeholder, which reads nothing, until Stop.
func TestProxyDied(t *testing.T) {
	spec, dir := fakeProxy(t, "ok")
	p, err := StartProxy(bg, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	pid := readPid(t, filepath.Join(dir, "pid"))
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	select {
	case <-p.Died():
	case <-time.After(5 * time.Second):
		t.Fatal("Died not closed")
	}
	if err := p.HoldErr(); err != nil {
		t.Fatalf("placeholder: %v", err)
	}
	c, err := net.DialTimeout("tcp", spec.Addr.String(), time.Second)
	if err != nil {
		t.Fatalf("the placeholder does not hold %s: %v", spec.Addr, err)
	}
	_, _ = c.Write([]byte("GET / HTTP/1.0\r\nAuthorization: Bearer sk-ant-SECRET\r\n\r\n"))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _ := c.Read(make([]byte, 64)); n != 0 {
		t.Error("the placeholder answered")
	}
	_ = c.Close()
	if _, err := PTYEnv(nil, []string{"AGGY_VENDOR=claude", p.Env()}); err == nil {
		t.Error("a dead proxy's variable passes")
	}
	p.Stop()
	if portTaken(spec.Addr) {
		t.Error("Stop left the placeholder")
	}
	// A Stop is not a death.
	spec2, _ := fakeProxy(t, "ok")
	p2, err := StartProxy(bg, spec2, nil)
	if err != nil {
		t.Fatal(err)
	}
	p2.Stop()
	select {
	case <-p2.Died():
		t.Error("Stop counted as a death")
	default:
	}
	if portTaken(spec2.Addr) {
		t.Error("a stopped proxy's address is held")
	}
}

// fakeCLI starts a stand-in for the CLI, /bin/sleep in its own session
// as a PTY's CLI is, and returns its pid and a channel closed once it
// exits. It is killed at the end of the test.
func fakeCLI(t *testing.T) (int, <-chan struct{}) {
	t.Helper()
	c := exec.Command("/bin/sleep", "60")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = c.Wait(); close(exited) }()
	t.Cleanup(func() { _ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL); <-exited })
	return c.Process.Pid, exited
}

// pidFile waits for a helper to write its pid into f.
func pidFile(t *testing.T, f string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(f); err == nil {
			if pid, err := strconv.Atoi(string(b)); err == nil && pid > 1 {
				return pid
			}
		}
	}
	t.Fatalf("no pid in %s", f)
	return 0
}

// TestProxyHoldLost (HIGH-2, round 2): when the proxy dies and another
// process keeps its address, aggy cannot take it, so it kills the CLI's
// whole group at once rather than let it go on sending its login there.
// The other process is the proxy's listener handed to a process in its
// own session, so the loss is certain.
func TestProxyHoldLost(t *testing.T) {
	spec, dir := fakeProxy(t, "escape")
	p, err := StartProxy(bg, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	cli, cliExited := fakeCLI(t)
	p.Guard(cli, true)
	esc := pidFile(t, filepath.Join(dir, "escaped"))
	defer func() { _ = syscall.Kill(esc, syscall.SIGKILL); gone(t, "the escaped listener", esc) }()
	_ = syscall.Kill(-readPid(t, filepath.Join(dir, "pid")), syscall.SIGKILL)
	start := time.Now()
	select {
	case <-p.Died():
	case <-time.After(5 * time.Second):
		t.Fatal("Died not closed")
	}
	if p.HoldErr() == nil {
		t.Fatal("aggy took an address another process holds")
	}
	select {
	case <-cliExited:
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("the CLI was killed only after %s", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the CLI still runs while another process has its proxy's address")
	}
	// A CLI guarded after the loss is killed at once.
	late, lateExited := fakeCLI(t)
	p.Guard(late, true)
	select {
	case <-lateExited:
	case <-time.After(2 * time.Second):
		t.Fatal("a CLI guarded after the loss still runs")
	}
}

// TestProxyHoldRace (HIGH-2, round 2): an attacker retrying bind every
// 100µs races aggy for the port as the proxy dies. Whoever wins, the CLI
// never keeps running while the attacker has the address: aggy holds it
// (and the attacker never gets it while it does), or the CLI is killed
// within two seconds.
func TestProxyHoldRace(t *testing.T) {
	spec, dir := fakeProxy(t, "ok")
	p, err := StartProxy(bg, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	cli, cliExited := fakeCLI(t)
	p.Guard(cli, true)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ad := t.TempDir()
	a := exec.Command(exe, "-test.run=^TestFakeProxyHelper$", "--", "fakeproxy", "attack", spec.Addr.String(), ad)
	a.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	attackDone := make(chan struct{})
	go func() { _ = a.Wait(); close(attackDone) }()
	defer func() { _ = syscall.Kill(a.Process.Pid, syscall.SIGKILL); <-attackDone }()
	_ = pidFile(t, filepath.Join(ad, "pid")) // it is retrying
	_ = syscall.Kill(-readPid(t, filepath.Join(dir, "pid")), syscall.SIGKILL)
	start := time.Now()
	select {
	case <-p.Died():
	case <-time.After(5 * time.Second):
		t.Fatal("Died not closed")
	}
	if err := p.HoldErr(); err != nil {
		t.Logf("the attacker won the port: %v", err)
		select {
		case <-cliExited:
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("the CLI was killed only after %s", d)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the CLI still runs while the attacker has its proxy's address")
		}
		return
	}
	t.Log("aggy won the port")
	time.Sleep(300 * time.Millisecond) // the attacker keeps trying
	if _, err := os.Stat(filepath.Join(ad, "won")); err == nil {
		t.Error("the attacker bound the address aggy holds")
	}
	select {
	case <-cliExited:
		t.Error("the CLI was killed though aggy holds the address")
	default:
	}
}

// TestPTYEnvStaleProxy (MED-2): an inherited proxy variable on loopback
// that no proxy here runs on never reaches a CLI; the user's own remote
// base URL and other variables pass as before.
func TestPTYEnvStaleProxy(t *testing.T) {
	// Nested (round 2 MED): an outer session's proxy is dropped.
	base := []string{"ANTHROPIC_BASE_URL=http://127.0.0.1:8787", "GROK_MODELS_BASE_URL=http://localhost:1/v1", "HOME=/h", "AGGY_PARENT=p1"}
	env, err := PTYEnv(base, []string{"AGGY_VENDOR=claude"})
	if err != nil || len(env) != 3 || env[0] != "HOME=/h" {
		t.Errorf("nested: env = %q, %v", env, err)
	}
	// Not nested: the user's own local gateway (LiteLLM) passes.
	env, err = PTYEnv(base[:3], []string{"AGGY_VENDOR=claude"})
	if err != nil || len(env) != 4 {
		t.Errorf("the user's own loopback base URL was dropped: %q, %v", env, err)
	}
	env, _ = PTYEnv([]string{"ANTHROPIC_BASE_URL=https://gateway.example.com"}, nil)
	if len(env) != 1 {
		t.Errorf("the user's own remote base URL was dropped: %q", env)
	}
}

// TestProxyInterrupted (MED-1): a signal during the health wait stops the
// proxy and returns.
func TestProxyInterrupted(t *testing.T) {
	spec, dir := fakeProxy(t, "unhealthy")
	ctx, cancel := context.WithCancel(bg)
	time.AfterFunc(500*time.Millisecond, cancel)
	start := time.Now()
	if _, err := StartProxy(ctx, spec, nil); !errors.Is(err, ErrProxyInterrupted) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %s", d)
	}
	gone(t, "the proxy", readPid(t, filepath.Join(dir, "pid")))
	gone(t, "its child", readPid(t, filepath.Join(dir, "child")))
}
