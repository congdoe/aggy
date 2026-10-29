package execrun

import (
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// A tool's local proxy (T6780): StartProxy starts it for one on-screen
// session, health-checks it, checks the listener on its address is its
// own, and registers it, and only then may PTYEnv pass that vendor's
// base-URL variable pointing at it. Stop unregisters it and kills its
// process group. Borrow children never get one: Env, their only
// environment, still refuses every base-URL variable.

// ProxySpec is one proxy to start.
type ProxySpec struct {
	Vendor adapter.Vendor
	// Var is the vendor's base-URL variable: tooldef.ProxyVars[Vendor].
	Var string
	// Path is the program's clean absolute path, Args its arguments.
	Path string
	Args []string
	// Addr is the loopback address it listens on (tooldef.ParseListen).
	Addr netip.AddrPort
	// Health is the HTTP path that answers 2xx once it is ready.
	Health string
	// Dir is the private folder it runs in.
	Dir string
	// Wait bounds how long it may take to answer its health check;
	// zero is ProxyWait.
	Wait time.Duration
}

// ProxyWait is how long a proxy may take to come up. Headroom's own
// --version takes about 7 seconds cold (docs/verification.md).
const ProxyWait = 15 * time.Second

// The ways StartProxy refuses or fails. aggy then launches the CLI
// without the proxy.
var (
	// ErrProxyPortTaken: something already listens on the address, and
	// aggy never routes a CLI to a process it did not start.
	ErrProxyPortTaken = errors.New("execrun: the proxy's port is already in use")
	// ErrProxyUnhealthy: it did not answer its health check in time.
	ErrProxyUnhealthy = errors.New("execrun: the proxy did not answer its health check")
	// ErrProxyExited: it exited before it was healthy.
	ErrProxyExited = errors.New("execrun: the proxy exited")
	// ErrProxyNotOwned: the process listening on the address is not in
	// the proxy's process group (another process bound the port while
	// the proxy started), or this system cannot tell (T6780).
	ErrProxyNotOwned = errors.New("execrun: the listener on the proxy's address is not the proxy aggy started")
	// ErrProxyInterrupted: a signal arrived while aggy waited for it.
	ErrProxyInterrupted = errors.New("execrun: interrupted while starting the proxy")
)

// Proxy is a running proxy.
type Proxy struct {
	spec ProxySpec
	url  string
	done chan struct{} // closed once the process is reaped
	died chan struct{} // closed when it exits on its own while in use
	stop sync.Once
	// kill signals the process group; nil on systems with none.
	kill func(graceful bool)

	mu       sync.Mutex
	live     bool   // registered: a CLI may point at it
	stopping bool   // Stop began: its exit is expected
	hold     func() // closes the placeholder listener (holdLoopback)
	holdErr  error
}

// Env is the variable that points the CLI at the proxy.
func (p *Proxy) Env() string { return p.spec.Var + "=" + p.url }

// Exited reports whether the proxy's process has exited.
func (p *Proxy) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Died is closed when the proxy exits on its own while a CLI may point
// at it. By then aggy holds its address with a placeholder that accepts
// and closes each connection, reading nothing, so no other process can
// take the port and the CLI's requests fail rather than go elsewhere;
// HoldErr says whether that worked. The placeholder lasts until Stop.
func (p *Proxy) Died() <-chan struct{} { return p.died }

// HoldErr is why the placeholder could not take the address, once Died
// is closed; nil when it holds it.
func (p *Proxy) HoldErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.holdErr
}

// Stop unregisters the proxy, so no CLI started after it gets its
// variable, and ends its process group: SIGTERM, then SIGKILL after
// two seconds. It returns once the process is reaped, and then closes
// the placeholder, if one holds the address.
func (p *Proxy) Stop() {
	p.stop.Do(func() {
		p.mu.Lock()
		p.stopping, p.live = true, false
		p.mu.Unlock()
		proxies.Lock()
		delete(proxies.m, p)
		proxies.Unlock()
		if p.kill != nil {
			if !p.Exited() {
				p.kill(true)
			}
			select {
			case <-p.done:
			case <-time.After(2 * time.Second):
				if !p.Exited() {
					p.kill(false)
				}
				<-p.done
			}
		}
		p.mu.Lock()
		if p.hold != nil {
			p.hold()
		}
		p.mu.Unlock()
	})
}

// ProxyEnv is a proxy's environment, built from scratch as a tool
// server's is (ToolEnv): HOME, USER, LOGNAME, PATH, TMPDIR, and the
// locale from base, and DO_NOT_TRACK=1. Nothing else: no AGGY_*
// variable (the socket, the trace id), no vendor key or base URL, no
// HTTP proxy.
func ProxyEnv(base []string) []string {
	env, _ := ToolEnv(base, nil) // no names: it cannot fail
	return append(env, "DO_NOT_TRACK=1")
}

// proxies are the proxies running in this process, which PTYEnv checks.
var proxies = struct {
	sync.Mutex
	m map[*Proxy]bool
}{m: map[*Proxy]bool{}}
