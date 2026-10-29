//go:build unix

package execrun

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
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

// TestPTYEnvStaleProxy (MED-2): an inherited proxy variable on loopback
// that no proxy here runs on never reaches a CLI; the user's own remote
// base URL and other variables pass as before.
func TestPTYEnvStaleProxy(t *testing.T) {
	base := []string{"ANTHROPIC_BASE_URL=http://127.0.0.1:8787", "GROK_MODELS_BASE_URL=http://localhost:1/v1", "HOME=/h"}
	env, err := PTYEnv(base, []string{"AGGY_VENDOR=claude"})
	if err != nil || len(env) != 2 || env[0] != "HOME=/h" {
		t.Errorf("env = %q, %v", env, err)
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
