//go:build unix

package execrun

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// The fake proxy is this test binary run as
// "-test.run=^TestFakeProxyHelper$ -- fakeproxy <mode> <addr> <dir>":
// arguments, since ProxyEnv passes it no variable to say so.
func TestFakeProxyHelper(t *testing.T) {
	i := slices.Index(os.Args, "fakeproxy")
	if i < 0 || len(os.Args) < i+4 {
		return // not run as the helper
	}
	mode, addr, dir := os.Args[i+1], os.Args[i+2], os.Args[i+3]
	_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "env"), []byte(strings.Join(os.Environ(), "\n")), 0o600)
	if mode == "exit" {
		_, _ = os.Stderr.WriteString("boom\x1b[31m: no port\n")
		os.Exit(1)
	}
	if mode == "squat" {
		// HIGH-1: a process outside the proxy's group (its own session)
		// takes the port and answers the health check; the proxy itself
		// never listens.
		sq := filepath.Join(dir, "sq")
		_ = os.Mkdir(sq, 0o700)
		c := exec.Command(os.Args[0], "-test.run=^TestFakeProxyHelper$", "--", "fakeproxy", "ok", addr, sq)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if c.Start() != nil {
			os.Exit(4)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if mode == "holdfd" {
		// HIGH-2: holds the listener it inherited as fd 3, outside the
		// proxy's group, and accepts nothing.
		time.Sleep(20 * time.Second)
		os.Exit(0)
	}
	if mode == "attack" {
		// HIGH-2: retries bind every 100µs, as an attacker racing aggy
		// for the port once the proxy dies; it says so if it wins.
		for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Microsecond) {
			if ln, err := net.Listen("tcp", addr); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "won"), nil, 0o600)
				time.Sleep(3 * time.Second)
				_ = ln.Close()
				break
			}
		}
		os.Exit(0)
	}
	// A grandchild in the proxy's group, which Stop must kill too.
	sleep := exec.Command("/bin/sleep", "60")
	if err := sleep.Start(); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "child"), []byte(strconv.Itoa(sleep.Process.Pid)), 0o600)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		os.Exit(3)
	}
	var escape sync.Once
	_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "escape" {
			// HIGH-2: once aggy has checked it, hand the listener to a
			// process in its own session, which keeps the port when the
			// proxy dies (a worker that escaped its group).
			escape.Do(func() {
				go func() {
					time.Sleep(1500 * time.Millisecond)
					f, err := ln.(*net.TCPListener).File()
					if err != nil {
						return
					}
					esc := filepath.Join(dir, "esc")
					_ = os.Mkdir(esc, 0o700)
					c := exec.Command(os.Args[0], "-test.run=^TestFakeProxyHelper$", "--", "fakeproxy", "holdfd", addr, esc)
					c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
					c.ExtraFiles = []*os.File{f}
					if c.Start() == nil {
						_ = os.WriteFile(filepath.Join(dir, "escaped"), []byte(strconv.Itoa(c.Process.Pid)), 0o600)
					}
				}()
			})
		}
		switch {
		case mode == "redirect" && r.URL.Path == "/health":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case mode == "unhealthy" || r.URL.Path != "/health" && r.URL.Path != "/ok":
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	os.Exit(0)
}

// freeAddr is a loopback address nothing listens on, for now.
func freeAddr(t *testing.T) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := netip.MustParseAddrPort(ln.Addr().String())
	_ = ln.Close()
	return a
}

func fakeProxy(t *testing.T, mode string) (ProxySpec, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	addr := freeAddr(t)
	return ProxySpec{Vendor: adapter.Claude, Var: "ANTHROPIC_BASE_URL", Path: exe, Addr: addr, Health: "/health", Dir: dir, Wait: 5 * time.Second,
		Args: []string{"-test.run=^TestFakeProxyHelper$", "--", "fakeproxy", mode, addr.String(), dir}}, dir
}

// gone waits until pid is no more, and kills it if it outlives the test.
func gone(t *testing.T, what string, pid int) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("%s (pid %d) outlived the proxy", what, pid)
		}
	}
}

func readPid(t *testing.T, f string) int {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil || pid <= 1 {
		t.Fatalf("pid %q", b)
	}
	return pid
}

// TestProxyLifecycle (T6780): a healthy proxy gets a scrubbed env, makes
// its variable passable to that vendor's on-screen CLI only, and Stop
// kills its whole process group and takes the grant back.
func TestProxyLifecycle(t *testing.T) {
	spec, dir := fakeProxy(t, "ok")
	base := []string{"HOME=/h", "PATH=/bin:rel", "AGGY_SOCKET=/s", "AGGY_TRACE=x", "ANTHROPIC_API_KEY=k", "HTTP_PROXY=http://evil", "LD_PRELOAD=/x.so"}
	p, err := StartProxy(bg, spec, base)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	if got := strings.Split(string(env), "\n"); !slices.Equal(got, []string{"HOME=/h", "PATH=/bin", "DO_NOT_TRACK=1"}) {
		t.Errorf("proxy env = %q", got)
	}
	url := "http://" + spec.Addr.String()
	if p.Env() != "ANTHROPIC_BASE_URL="+url {
		t.Errorf("Env = %q", p.Env())
	}
	for _, c := range []struct {
		name  string
		extra []string
		ok    bool
	}{
		{"claude", []string{"AGGY_VENDOR=claude", p.Env()}, true},
		{"codex session", []string{"AGGY_VENDOR=codex", p.Env()}, false},
		{"no vendor", []string{p.Env()}, false},
		{"another port", []string{"AGGY_VENDOR=claude", "ANTHROPIC_BASE_URL=http://127.0.0.1:1"}, false},
		{"a path after it", []string{"AGGY_VENDOR=claude", p.Env() + "/v1"}, false},
		{"another variable", []string{"AGGY_VENDOR=claude", "OPENAI_BASE_URL=" + url}, false},
		{"twice", []string{"AGGY_VENDOR=claude", p.Env(), p.Env()}, false},
	} {
		got, err := PTYEnv([]string{"ANTHROPIC_BASE_URL=https://user.example"}, c.extra)
		if (err == nil) != c.ok {
			t.Errorf("%s: PTYEnv err = %v", c.name, err)
		}
		if c.ok && !slices.Equal(got, c.extra) {
			t.Errorf("%s: env = %q (the user's own must give way)", c.name, got)
		}
	}
	// A borrow child's env never takes it.
	if _, err := Env(nil, []string{p.Env()}); err == nil {
		t.Error("Env passed a proxy variable to a borrow child")
	}
	pid, child := readPid(t, filepath.Join(dir, "pid")), readPid(t, filepath.Join(dir, "child"))
	p.Stop()
	gone(t, "the proxy", pid)
	gone(t, "the proxy's child", child)
	if _, err := PTYEnv(nil, []string{"AGGY_VENDOR=claude", p.Env()}); err == nil {
		t.Error("a stopped proxy's variable still passes")
	}
	if portTaken(spec.Addr) {
		t.Error("the port is still taken after Stop")
	}
}

// TestProxyRefusals (T6780): a taken port starts nothing; a proxy that
// fails, answers non-2xx, or redirects is stopped and never registered.
func TestProxyRefusals(t *testing.T) {
	t.Run("port taken", func(t *testing.T) {
		spec, dir := fakeProxy(t, "ok")
		ln, err := net.Listen("tcp", spec.Addr.String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		if _, err := StartProxy(bg, spec, nil); !errors.Is(err, ErrProxyPortTaken) {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "pid")); err == nil {
			t.Error("the proxy started although its port was taken")
		}
	})
	for _, c := range []struct {
		mode string
		want error
	}{
		{"exit", ErrProxyExited},
		{"unhealthy", ErrProxyUnhealthy},
		{"redirect", ErrProxyUnhealthy},
	} {
		t.Run(c.mode, func(t *testing.T) {
			spec, dir := fakeProxy(t, c.mode)
			spec.Wait = 1500 * time.Millisecond
			p, err := StartProxy(bg, spec, nil)
			if !errors.Is(err, c.want) || p != nil {
				t.Fatalf("err = %v", err)
			}
			if c.mode == "exit" && !strings.Contains(err.Error(), "boom") {
				t.Errorf("err = %v, want its last output", err)
			}
			gone(t, "the proxy", readPid(t, filepath.Join(dir, "pid")))
			if c.mode != "exit" {
				gone(t, "the proxy's child", readPid(t, filepath.Join(dir, "child")))
			}
			if _, err := PTYEnv(nil, []string{"AGGY_VENDOR=claude", "ANTHROPIC_BASE_URL=http://" + spec.Addr.String()}); err == nil {
				t.Error("a failed proxy's variable passes")
			}
		})
	}
	t.Run("bad spec", func(t *testing.T) {
		spec, _ := fakeProxy(t, "ok")
		for _, edit := range []func(*ProxySpec){
			func(s *ProxySpec) { s.Addr = netip.MustParseAddrPort("0.0.0.0:8787") },
			func(s *ProxySpec) { s.Addr = netip.MustParseAddrPort("127.0.0.2:8787") },
			func(s *ProxySpec) { s.Var = "OPENAI_BASE_URL" },
			func(s *ProxySpec) { s.Vendor = adapter.Codex },
			func(s *ProxySpec) { s.Path = "fake" },
			func(s *ProxySpec) { s.Health = "//evil.example/x" },
		} {
			s := spec
			edit(&s)
			if _, err := StartProxy(bg, s, nil); err == nil || errors.Is(err, ErrProxyPortTaken) {
				t.Errorf("%+v: err = %v", s, err)
			}
		}
	})
}
