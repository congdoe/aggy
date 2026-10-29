//go:build unix

package execrun

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/tooldef"
)

// StartProxy starts the proxy s describes for one on-screen session, in
// its own process group, with ProxyEnv(base) as its whole environment,
// no stdin, and its output kept only as a capped tail. It refuses when
// something already listens on s.Addr, and returns only once the proxy
// answers its health check, or fails (and is stopped) when it does not
// within s.Wait, or exits. A healthy proxy is registered, so PTYEnv
// passes s.Var pointing at it; the caller must Stop it.
//
// Limit: a process that binds the address between the port check and
// the proxy's own bind would answer the health check while the proxy
// exits for want of its port; the proxy's exit is checked after the
// health check, which catches a proxy that gives up at once.
func StartProxy(s ProxySpec, base []string) (*Proxy, error) {
	if err := checkProxySpec(s); err != nil {
		return nil, err
	}
	if err := safefs.Check(s.Path); err != nil {
		return nil, err
	}
	if portTaken(s.Addr) {
		return nil, fmt.Errorf("%w: something already listens on %s, and aggy did not start it", ErrProxyPortTaken, s.Addr)
	}
	out := &lockedTail{c: capped{max: proxyLog, tail: true}}
	cmd := exec.Command(s.Path, s.Args...)
	cmd.Env = ProxyEnv(base)
	cmd.Dir = s.Dir
	cmd.Stdout, cmd.Stderr = out, out
	// A process that escaped the group may hold the output pipe open.
	cmd.WaitDelay = time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Proxy{spec: s, url: proxyURL(s.Addr), done: make(chan struct{})}
	pid := cmd.Process.Pid
	p.kill = func(graceful bool) {
		sig := syscall.SIGKILL
		if graceful {
			sig = syscall.SIGTERM
		}
		_ = syscall.Kill(-pid, sig)
	}
	go func() {
		// reap kills what is left of the group once the leader exits,
		// before it reaps it.
		_ = reap(cmd)
		close(p.done)
	}()
	wait := s.Wait
	if wait <= 0 {
		wait = ProxyWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	for !healthy(ctx, s.Addr, s.Health) {
		select {
		case <-p.done:
			p.Stop()
			return nil, out.failed(fmt.Errorf("%w before it answered its health check", ErrProxyExited))
		case <-ctx.Done():
			p.Stop()
			return nil, out.failed(fmt.Errorf("%w at %s within %s", ErrProxyUnhealthy, s.Addr, wait))
		case <-time.After(100 * time.Millisecond):
		}
	}
	// One more beat: a proxy that could not bind (another process
	// answered the check) exits at once.
	select {
	case <-p.done:
		p.Stop()
		return nil, out.failed(fmt.Errorf("%w right after its health check passed: another process may hold %s", ErrProxyExited, s.Addr))
	case <-time.After(200 * time.Millisecond):
	}
	proxies.Lock()
	proxies.m[p] = true
	proxies.Unlock()
	return p, nil
}

// proxyLog caps what is kept of a proxy's output: its last bytes, shown
// (through termsafe) only when it fails to come up. It never reaches the
// terminal while the CLI runs.
const proxyLog = 4 << 10

// proxyURL is the base URL of a proxy at addr.
func proxyURL(addr netip.AddrPort) string { return "http://" + addr.String() }

// checkProxySpec refuses a spec that tooldef's validator would not have
// produced: aggy routes a CLI only to loopback, through its own
// variable.
func checkProxySpec(s ProxySpec) error {
	if want, ok := tooldef.ProxyVars[s.Vendor]; !ok || s.Var != want {
		return fmt.Errorf("execrun: %s is not %s's proxy variable", s.Var, s.Vendor)
	}
	if a, err := tooldef.ParseListen(s.Addr.String()); err != nil || a != s.Addr {
		return fmt.Errorf("execrun: a proxy listens on 127.0.0.1 or ::1 only")
	}
	if !filepath.IsAbs(s.Path) || filepath.Clean(s.Path) != s.Path {
		return fmt.Errorf("execrun: %q is not a clean absolute path", s.Path)
	}
	if s.Dir == "" || !filepath.IsAbs(s.Dir) {
		return fmt.Errorf("execrun: working directory %q is not absolute", s.Dir)
	}
	if !strings.HasPrefix(s.Health, "/") || strings.HasPrefix(s.Health, "//") || strings.ContainsAny(s.Health, "?#@\\ ") {
		return fmt.Errorf("execrun: %q is not a health path", s.Health)
	}
	return nil
}

// proxyGrant reports whether a running proxy aggy started allows k=v in
// an interactive CLI of vendor: k its variable, v exactly its URL.
func proxyGrant(k, v string, vendor adapter.Vendor) bool {
	proxies.Lock()
	defer proxies.Unlock()
	for p := range proxies.m {
		if p.spec.Var == k && p.url == v && p.spec.Vendor == vendor && !p.Exited() {
			return true
		}
	}
	return false
}

// lockedTail is a capped tail safe to read while the process writes.
type lockedTail struct {
	mu sync.Mutex
	c  capped
}

func (l *lockedTail) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.c.Write(p)
}

// last is the last non-empty line written, at most 200 bytes.
func (l *lockedTail) last() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	lines := bytes.Split(bytes.TrimSpace(l.c.bytes()), []byte("\n"))
	s := string(lines[len(lines)-1])
	if len(s) > 200 {
		s = s[len(s)-200:]
	}
	return s
}

// failed wraps err with the proxy's last line of output, if any.
func (l *lockedTail) failed(err error) error {
	if s := l.last(); s != "" {
		return fmt.Errorf("%w (its last output: %s)", err, s)
	}
	return err
}
