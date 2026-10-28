//go:build unix

package supervisor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const pid = "0b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e"

// shortDir is a temp dir short enough for a socket path on darwin.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "aggys")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	d, err = filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func listen(t *testing.T, runDir string) *Server {
	t.Helper()
	s, err := Listen(runDir, pid, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.SetRoot(os.Getpid()) // the test is the CLI
	return s
}

// ask sends raw on a new connection and returns the reply line, or "" if
// the server closed the connection without one.
func ask(t *testing.T, s *Server, raw string) string {
	t.Helper()
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	// Write while reading: the server may answer and close before it has
	// read everything a too-long request sends.
	go func() {
		_, _ = c.Write([]byte(raw))
		_ = c.(*net.UnixConn).CloseWrite()
	}()
	line, _ := bufio.NewReader(c).ReadString('\n')
	return line
}

func TestSocketPermissions(t *testing.T) {
	tests := []struct {
		name string
		pre  func(t *testing.T, run string)
	}{
		{"new run dir", func(*testing.T, string) {}},
		{"loose run dir is tightened", func(t *testing.T, run string) {
			if err := os.Mkdir(run, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := filepath.Join(shortDir(t), "run")
			tt.pre(t, run)
			s := listen(t, run)
			di, err := os.Lstat(run)
			if err != nil {
				t.Fatal(err)
			}
			if di.Mode().Perm() != 0o700 {
				t.Errorf("run dir mode %v, want 0700", di.Mode().Perm())
			}
			fi, err := os.Lstat(s.Path())
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Type() != fs.ModeSocket || fi.Mode().Perm() != 0o600 {
				t.Errorf("socket mode %v, want a 0600 socket", fi.Mode())
			}
			if s.Path() != filepath.Join(run, pid+".sock") {
				t.Errorf("path %s", s.Path())
			}
		})
	}
}

func TestSocketRefusesBadSetup(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		pre     func(t *testing.T, run string)
		runDir  func(base string) string
		wantErr string
	}{
		{"parent id not a UUID", "../x", nil, nil, "not a UUID"},
		{"run dir is a symlink", pid, func(t *testing.T, run string) {
			if err := os.Symlink(t.TempDir(), run); err != nil {
				t.Fatal(err)
			}
		}, nil, ""},
		{"a file at the socket path", pid, func(t *testing.T, run string) {
			if err := os.MkdirAll(run, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(run, pid+".sock"), []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, nil, "not a socket"},
		{"path too long", pid, nil, func(base string) string { return filepath.Join(base, strings.Repeat("d", 80)) }, "longer than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := shortDir(t)
			run := filepath.Join(base, "run")
			if tt.runDir != nil {
				run = tt.runDir(base)
			}
			if tt.pre != nil {
				tt.pre(t, run)
			}
			s, err := Listen(run, tt.id, nil, nil)
			if err == nil {
				_ = s.Close()
				t.Fatal("Listen succeeded")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
			if tt.name == "a file at the socket path" {
				if b, _ := os.ReadFile(filepath.Join(run, pid+".sock")); string(b) != "keep" {
					t.Error("Listen touched a file that is not a socket")
				}
			}
		})
	}
}

func TestStaleSocketCleanup(t *testing.T) {
	run := filepath.Join(shortDir(t), "run")
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	// A socket a dead supervisor left behind: nobody holds its lock.
	path := filepath.Join(run, pid+".sock")
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	_ = old.Close()

	s := listen(t, run)
	if got := ask(t, s, `{"type":"ping"}`+"\n"); !strings.Contains(got, `"pong"`) {
		t.Errorf("ping after cleanup = %q", got)
	}

	// A live supervisor holds the lock: a second one is refused and the
	// live socket stays.
	if s2, err := Listen(run, pid, nil, nil); err == nil {
		_ = s2.Close()
		t.Fatal("a second supervisor took a live session's socket")
	} else if !strings.Contains(err.Error(), "another aggy") {
		t.Errorf("err = %v", err)
	}
	if got := ask(t, s, `{"type":"ping"}`+"\n"); !strings.Contains(got, `"pong"`) {
		t.Errorf("the live socket stopped answering: %q", got)
	}

	// Close removes the socket and the lock file.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, filepath.Join(run, pid+".lock")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", p, err)
		}
	}
}

func TestSocketProtocol(t *testing.T) {
	run := filepath.Join(shortDir(t), "run")
	s := listen(t, run)
	pad := func(n int) string { // a valid ping n bytes long
		p := `{"type":"ping"}`
		return p + strings.Repeat(" ", n-len(p))
	}
	tests := []struct {
		name string
		in   string
		want string // substring of the reply; "" means no reply
	}{
		{"ping", `{"type":"ping"}` + "\n", `{"ok":true,"type":"pong"}`},
		{"two pings, one connection", `{"type":"ping"}` + "\n" + `{"type":"ping"}` + "\n", `"pong"`},
		{"hook event", `{"type":"hook-event","vendor":"claude","event":"Stop","payload":{"x":1}}` + "\n", `{"ok":true}`},
		{"switch request stub", `{"type":"switch-request","vendor":"grok"}` + "\n", `not implemented`},
		{"exactly the cap", pad(MaxMessage) + "\n", `"pong"`},
		{"one byte over the cap", pad(MaxMessage+1) + "\n", `longer than 65536 bytes`},
		{"far over the cap, no newline", strings.Repeat("a", 3*MaxMessage), `longer than`},
		{"malformed JSON", `{"type":` + "\n", `bad request`},
		{"not an object", `[1,2]` + "\n", `bad request`},
		{"unknown field", `{"type":"ping","x":1}` + "\n", `unknown field`},
		{"two values on a line", `{"type":"ping"}{"type":"ping"}` + "\n", `more than one`},
		{"unknown type", `{"type":"exec"}` + "\n", `unknown type`},
		{"ping with fields", `{"type":"ping","vendor":"claude"}` + "\n", `no fields`},
		{"hook from unknown vendor", `{"type":"hook-event","vendor":"gpt","event":"Stop","payload":{}}` + "\n", `unknown vendor`},
		{"hook event name with a space", `{"type":"hook-event","vendor":"codex","event":"Stop now","payload":{}}` + "\n", `event name`},
		{"hook payload not an object", `{"type":"hook-event","vendor":"grok","event":"Stop","payload":"rm -rf"}` + "\n", `JSON object`},
		{"hook without payload", `{"type":"hook-event","vendor":"grok","event":"Stop"}` + "\n", `JSON object`},
		{"switch to unknown vendor", `{"type":"switch-request","vendor":"x"}` + "\n", `unknown vendor`},
		{"no newline, then close", `{"type":"ping"}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ask(t, s, tt.in)
			if tt.want == "" {
				if got != "" {
					t.Errorf("reply %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("reply %q, want %q", got, tt.want)
			}
			var r Response
			if err := json.Unmarshal([]byte(got), &r); err != nil {
				t.Errorf("reply is not JSON: %v", err)
			}
		})
	}
}

func TestSocketRefusesOtherUID(t *testing.T) {
	tests := []struct {
		name string
		uid  func(*net.UnixConn) (int, int, error)
		want bool // answered
	}{
		{"same uid", func(*net.UnixConn) (int, int, error) { return os.Getuid(), 1, nil }, true},
		{"other uid", func(*net.UnixConn) (int, int, error) { return os.Getuid() + 1, 1, nil }, false},
		{"lookup fails", func(*net.UnixConn) (int, int, error) { return -1, -1, os.ErrPermission }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := peerCred
			peerCred = tt.uid
			t.Cleanup(func() { peerCred = old })
			s := listen(t, filepath.Join(shortDir(t), "run"))
			got := ask(t, s, `{"type":"ping"}`+"\n")
			if (got != "") != tt.want {
				t.Errorf("reply %q, answered want %v", got, tt.want)
			}
		})
	}
}

func TestPeerCredOfRealConnection(t *testing.T) {
	got := make(chan [2]int, 1)
	old := peerCred
	peerCred = func(c *net.UnixConn) (int, int, error) {
		uid, pid, err := peerCredOf(c)
		if err != nil {
			t.Error(err)
		}
		got <- [2]int{uid, pid}
		return uid, pid, err
	}
	t.Cleanup(func() { peerCred = old })
	s := listen(t, filepath.Join(shortDir(t), "run"))
	if r := ask(t, s, `{"type":"ping"}`+"\n"); !strings.Contains(r, "pong") {
		t.Errorf("reply %q", r)
	}
	if g := <-got; g != [2]int{os.Getuid(), os.Getpid()} {
		t.Errorf("peer uid, pid %v, want %d, %d", g, os.Getuid(), os.Getpid())
	}
}

// TestCloseDoesNotWaitForClients (B13-1): a connected client, idle or
// chatty, must not keep Close (and so aggy's exit) waiting.
func TestCloseDoesNotWaitForClients(t *testing.T) {
	tests := []struct {
		name  string
		every time.Duration // ping interval; 0 means idle after one ping
	}{
		{"idle client", 0},
		{"client pinging every 50ms", 50 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Listen(filepath.Join(shortDir(t), "run"), pid, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			s.SetRoot(os.Getpid()) // the test is the CLI, so its pings keep the connection
			c, err := net.Dial("unix", s.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			r := bufio.NewReader(c)
			ping := func() bool {
				if _, err := c.Write([]byte(`{"type":"ping"}` + "\n")); err != nil {
					return false
				}
				_, err := r.ReadString('\n')
				return err == nil
			}
			if !ping() {
				t.Fatal("first ping failed")
			}
			stop := make(chan struct{})
			defer close(stop)
			if tt.every > 0 {
				go func() {
					for {
						select {
						case <-stop:
							return
						case <-time.After(tt.every):
							if !ping() {
								return
							}
						}
					}
				}()
			}
			start := time.Now()
			closed := make(chan error, 1)
			go func() { closed <- s.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Close waited on a connected client")
			}
			if d := time.Since(start); d > time.Second {
				t.Errorf("Close took %v", d)
			}
		})
	}
}

// TestTakeLockAfterUnlink (B13-7): a lock taken on a file that Close
// unlinked in the meantime, while another supervisor locked a new file
// at the path, must not count.
func TestTakeLockAfterUnlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.lock")
	old, err := takeLock(path)
	if err != nil {
		t.Fatal(err)
	}
	var other *os.File
	lockOpened = func() {
		lockOpened = nil
		// The old holder exits as Close does (unlink, then unlock), and
		// a new supervisor locks a fresh file at the path.
		if err := os.Remove(path); err != nil {
			t.Error(err)
		}
		_ = old.Close()
		if other, err = takeLock(path); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { lockOpened = nil })
	if f, err := takeLock(path); err == nil {
		_ = f.Close()
		t.Fatal("took a lock on an unlinked file while another supervisor holds the path's")
	}
	if other != nil {
		_ = other.Close()
	}
}

func TestParseStat(t *testing.T) {
	// Fields 5 to 21 are filler; field 22 is the start time.
	rest := " 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 98765 0 0"
	tests := []struct {
		line  string
		ppid  int
		start uint64
		ok    bool
	}{
		{"123 (bash) S 45" + rest, 45, 98765, true},
		{"123 (a b) c)) R 7" + rest, 7, 98765, true},
		{"123 (x)", -1, 0, false},
		{"garbage", -1, 0, false},
		{"1 (x) S notanumber" + rest, -1, 0, false},
		{"1 (x) S 4 1 1", -1, 0, false}, // too short
	}
	for _, tt := range tests {
		ppid, start, err := parseStat([]byte(tt.line))
		if (err == nil) != tt.ok || tt.ok && (ppid != tt.ppid || start != tt.start) {
			t.Errorf("%q: %d, %d, %v; want %d, %d", tt.line, ppid, start, err, tt.ppid, tt.start)
		}
	}
}

func TestPeerInstanceOfRealConnection(t *testing.T) {
	got := make(chan bool, 1)
	old := peerInstance
	peerInstance = func(c *net.UnixConn, pid int) (func() bool, func(), error) {
		same, release, err := peerInstanceOf(c, pid)
		if err != nil {
			t.Error(err)
			got <- false
			return nil, nil, err
		}
		got <- same()
		return same, release, nil
	}
	t.Cleanup(func() { peerInstance = old })
	s := listen(t, filepath.Join(shortDir(t), "run"))
	_ = ask(t, s, `{"type":"ping"}`+"\n")
	if !<-got {
		t.Error("the connecting process (this test) is not the same process as itself")
	}
}

// TestPidReuseAfterAccept (G38-1): a peer whose pid comes to name another
// process (one in the CLI's tree) after it connected is not from the
// session, and a connection not from the session gets one answer only,
// so it cannot wait with pings for a pid to be reused.
func TestPidReuseAfterAccept(t *testing.T) {
	old := peerInstance
	peerInstance = func(*net.UnixConn, int) (func() bool, func(), error) {
		return func() bool { return false }, func() {}, nil // reused since accept
	}
	t.Cleanup(func() { peerInstance = old })
	got := make(chan Request, 1)
	s, err := Listen(filepath.Join(shortDir(t), "run"), pid, nil, func(r Request) { got <- r })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.SetRoot(os.Getpid()) // the peer's pid is in the tree
	if r := ask(t, s, `{"type":"hook-event","vendor":"codex","event":"Stop","payload":{}}`+"\n"); !strings.Contains(r, `"ok":true`) {
		t.Fatalf("reply %q", r)
	}
	if r := <-got; r.FromSession {
		t.Error("a reused pid made the event FromSession")
	}

	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	for i, want := range []bool{true, false} {
		_, _ = c.Write([]byte(`{"type":"ping"}` + "\n"))
		_, err := br.ReadString('\n')
		if (err == nil) != want {
			t.Errorf("ping %d answered = %v, want %v", i+1, err == nil, want)
		}
	}
}

func TestParentPIDOfRealProcess(t *testing.T) {
	got, err := parentPIDOf(os.Getpid())
	if err != nil || got != os.Getppid() {
		t.Errorf("parentPIDOf(self) = %d, %v; want %d", got, err, os.Getppid())
	}
	if _, err := parentPIDOf(1 << 30); err == nil {
		t.Error("parentPIDOf of a pid that does not exist did not fail")
	}
}

func TestDescends(t *testing.T) {
	// 10 <- 11 <- 12; 20 is elsewhere; 30 loops; 40 fails.
	parents := map[int]int{11: 10, 12: 11, 10: 1, 20: 1, 30: 31, 31: 30}
	old := parentPID
	parentPID = func(p int) (int, error) {
		if pp, ok := parents[p]; ok {
			return pp, nil
		}
		return -1, os.ErrNotExist
	}
	t.Cleanup(func() { parentPID = old })
	tests := []struct {
		name      string
		pid, root int
		want      bool
	}{
		{"root itself", 10, 10, true},
		{"grandchild", 12, 10, true},
		{"elsewhere", 20, 10, false},
		{"ancestor of root", 10, 12, false},
		{"loop is bounded", 30, 10, false},
		{"lookup fails", 40, 10, false},
		{"no pid", 0, 10, false},
		{"no root", 12, 0, false},
	}
	for _, tt := range tests {
		if got := descends(tt.pid, tt.root); got != tt.want {
			t.Errorf("%s: descends(%d, %d) = %v", tt.name, tt.pid, tt.root, got)
		}
	}
}

// TestLineageMarksRequests (D20-7): a hook event is FromSession only when
// the sender descends from the CLI (SetRoot); a switch-request from
// outside is refused. Without SetRoot, both fail closed.
func TestLineageMarksRequests(t *testing.T) {
	old := rootWait
	rootWait = 50 * time.Millisecond
	t.Cleanup(func() { rootWait = old })
	// A real process that is not this test's ancestor.
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	tests := []struct {
		name string
		root int // 0: SetRoot is never called
		want bool
	}{
		{"sender is the CLI", os.Getpid(), true},
		{"sender descends from the CLI", os.Getppid(), true},
		{"sender outside the CLI's tree", other.Process.Pid, false},
		{"CLI not started", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := make(chan Request, 1)
			s, err := Listen(filepath.Join(shortDir(t), "run"), pid, nil, func(r Request) { got <- r })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if tt.root != 0 {
				s.SetRoot(tt.root)
			}
			if r := ask(t, s, `{"type":"hook-event","vendor":"codex","event":"Stop","payload":{}}`+"\n"); !strings.Contains(r, `"ok":true`) {
				t.Fatalf("hook reply %q", r)
			}
			if r := <-got; r.FromSession != tt.want {
				t.Errorf("FromSession = %v, want %v", r.FromSession, tt.want)
			}
			r := ask(t, s, `{"type":"switch-request","vendor":"grok"}`+"\n")
			if refused := strings.Contains(r, "outside this session"); refused == tt.want {
				t.Errorf("switch-request reply %q", r)
			}
		})
	}
}

// TestFromSessionNotOnTheWire: a client cannot claim lineage itself.
func TestFromSessionNotOnTheWire(t *testing.T) {
	s := listen(t, filepath.Join(shortDir(t), "run"))
	r := ask(t, s, `{"type":"hook-event","vendor":"codex","event":"Stop","payload":{},"FromSession":true}`+"\n")
	if !strings.Contains(r, "unknown field") {
		t.Errorf("reply %q, want an unknown-field error", r)
	}
}

func TestPidfdFallback(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{syscall.ENOPROTOOPT, true},
		{fmt.Errorf("getsockopt: %w", syscall.ENOPROTOOPT), true},
		{syscall.EINVAL, false},
		{syscall.EBADF, false},
		{errors.New("other"), false},
	}
	for _, tt := range tests {
		if got := pidfdFallback(tt.err); got != tt.want {
			t.Errorf("pidfdFallback(%v) = %v", tt.err, got)
		}
	}
}
