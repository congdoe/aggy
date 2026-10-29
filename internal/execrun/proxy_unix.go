//go:build unix

package execrun

import (
	"bytes"
	"context"
	"errors"
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

// StartProxy starts the proxy s describes for one on-screen session, as
// the leader of its own session and process group, with ProxyEnv(base)
// as its whole environment, no stdin, and its output kept only as a
// capped tail. It refuses when something already listens on s.Addr. It
// returns once the proxy answers its health check and the listener on
// s.Addr is a socket of a process in the proxy's group (listenerOwned):
// a process that bound the port while the proxy started, and answered
// the check, is never trusted. It fails, and stops the proxy, when the
// proxy does not get there within s.Wait, exits, or ctx ends (a signal:
// ErrProxyInterrupted). A proxy that comes up is registered, so PTYEnv
// passes s.Var pointing at it; the caller must Stop it.
func StartProxy(ctx context.Context, s ProxySpec, base []string) (*Proxy, error) {
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
	cmd.SysProcAttr = proxyAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Proxy{spec: s, url: proxyURL(s.Addr), done: make(chan struct{}), died: make(chan struct{}), lostC: make(chan struct{})}
	pid := cmd.Process.Pid
	p.kill = func(graceful bool) {
		sig := syscall.SIGKILL
		if graceful {
			sig = syscall.SIGTERM
		}
		_ = syscall.Kill(-pid, sig)
	}
	go func() {
		// HIGH-2: the kernel frees the port the moment the leader exits,
		// so aggy takes it first, before it kills what is left of the
		// group and reaps the leader (reap's order otherwise).
		if leaderExited(pid) {
			p.leaderGone(func() { killGroup(cmd) })
			killGroup(cmd)
			_ = cmd.Wait()
		} else {
			_ = cmd.Wait()
			killGroup(cmd)
			p.leaderGone(func() {})
		}
		p.reaped()
	}()
	wait := s.Wait
	if wait <= 0 {
		wait = ProxyWait
	}
	hctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for !healthy(hctx, s.Addr, s.Health) {
		select {
		case <-p.done:
			p.Stop()
			return nil, out.failed(fmt.Errorf("%w before it answered its health check", ErrProxyExited))
		case <-hctx.Done():
			p.Stop()
			if ctx.Err() != nil {
				return nil, ErrProxyInterrupted
			}
			return nil, out.failed(fmt.Errorf("%w at %s within %s", ErrProxyUnhealthy, s.Addr, wait))
		case <-time.After(100 * time.Millisecond):
		}
	}
	// HIGH-1: the health check proves only that something answers.
	if err := listenerOwned(ctx, s.Addr, pid); err != nil {
		p.Stop()
		return nil, fmt.Errorf("%w: %v", ErrProxyNotOwned, err)
	}
	p.mu.Lock()
	if p.gone || p.Exited() {
		p.mu.Unlock()
		p.Stop()
		return nil, out.failed(fmt.Errorf("%w right after its health check passed", ErrProxyExited))
	}
	p.live = true
	p.mu.Unlock()
	proxies.Lock()
	proxies.m[p] = true
	proxies.Unlock()
	return p, nil
}

// leaderGone runs as soon as the proxy's leader has exited, before the
// rest of its group is killed or it is reaped. An exit nobody asked for,
// while a CLI may point at the proxy, puts the placeholder on its
// address at once (takeAddr); when the first try fails, another process
// may have the port the CLI sends its login to, and the CLI is killed
// then, before any retry. p.mu is not held across the retries.
//
// TODO(T6780): the CLI keeps running between the proxy's exit and this
// take, so a request it sends in that gap can reach a process that took
// the port and is then killed with it. Stopping the CLI (SIGSTOP) first
// would close the gap, but the supervisor treats a stopped CLI as Ctrl+Z
// and suspends aggy (PTY.Wait, WUNTRACED).
func (p *Proxy) leaderGone(killRest func()) {
	p.mu.Lock()
	p.gone = true
	if !p.live || p.stopping {
		p.mu.Unlock()
		return
	}
	addr := p.spec.Addr
	p.mu.Unlock()
	// Round 3: the CLI dies on the first failed take, not after the
	// retries: its SDK retries a failed request within half a second.
	hold, err := takeAddr(addr, killRest, func(err error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.loseCLI(err)
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.loseCLI(err) // whatever the failure: never a CLI on a free port
	}
	p.hold, p.holdErr = hold, err
	if p.lost != nil && err == nil {
		// The proxy's own worker let go in time, but the CLI is dead.
		p.holdErr = p.lost
	}
	close(p.died)
}

// takeWait bounds how long takeAddr retries while the proxy's own
// processes still hold its address.
const takeWait = 500 * time.Millisecond

// holdAddr is holdLoopback; a test replaces it to make the take fail.
var holdAddr = holdLoopback

// takeAddr puts the placeholder on addr. When the first try fails, for
// any reason, it calls lost at once (which kills the CLI). When the
// address is taken, it then kills what is left of the proxy's group
// (killRest), whose own processes may still hold it, and tries again
// every millisecond, for at most takeWait, so the placeholder still
// lands once they are gone. Any other error ends it at once.
func takeAddr(addr netip.AddrPort, killRest func(), lost func(error)) (func(), error) {
	hold, err := holdAddr(addr)
	if err == nil {
		return hold, nil
	}
	lost(err)
	if !errors.Is(err, syscall.EADDRINUSE) {
		return nil, err
	}
	killRest()
	for deadline := time.Now().Add(takeWait); time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
		if hold, err = holdAddr(addr); err == nil || !errors.Is(err, syscall.EADDRINUSE) {
			return hold, err
		}
	}
	return nil, err
}

// loseCLI kills the guarded CLI and closes Lost, once: the proxy died
// and aggy could not take its address. The caller holds p.mu.
func (p *Proxy) loseCLI(err error) {
	if p.lost != nil {
		return
	}
	p.lost = err
	p.killCLI()
	close(p.lostC)
}

// killCLI kills the guarded CLI (its group when it leads one). The
// caller holds p.mu.
func (p *Proxy) killCLI() {
	switch {
	case p.cli <= 1:
	case p.cliGroup:
		_ = syscall.Kill(-p.cli, syscall.SIGKILL)
	default:
		_ = syscall.Kill(p.cli, syscall.SIGKILL)
	}
}

// reaped runs once the process is reaped.
func (p *Proxy) reaped() {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.done)
}

// errNoListener: nothing listens on the address the proxy should.
var errNoListener = errors.New("no process listens on the address")

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
