//go:build unix

package supervisor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestSendHermesPinProof (T6954): the hook takes a hermes-pin answer only
// from the process that holds the supervisor's lock for the socket's parent
// id. A socket someone else bound at the path the index row names, answering
// --no-policy, is refused: with no open file, with its own open of the lock
// file, with another file, or for a parent id with no lock file. The real
// supervisor is accepted. The stand-ins run in the test's process: the proof
// is the open file the lock is on, not a pid, so that is no easier case.
func TestSendHermesPinProof(t *testing.T) {
	const tok = "0123456789abcdef0123456789abcdef"
	const sid = "20261110_101500_a1b2c3"
	const other = "1b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e"
	run := filepath.Join(shortDir(t), "run")
	s, err := Listen(run, pid, nil, func(Request) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.SetRoot(1 << 30)
	s.SetToken(tok)
	s.SetHermesPin(NewHermesClaim(HermesPin{Session: sid, NoPolicy: true}).Answer)
	payload, _ := json.Marshal(map[string]string{"session_id": sid})
	req := Request{Type: ReqHermesPin, Vendor: "hermes", Payload: payload, Token: tok}
	sock := func(id string) string { return filepath.Join(run, id+".sock") }

	// The real supervisor.
	resp, err := SendHermesPin(run, sock(pid), req, 5*time.Second)
	if err != nil || !resp.OK || !resp.NoPolicy {
		t.Fatalf("the supervisor: %+v %v", resp, err)
	}
	// A refusal needs no proof: it loosens nothing.
	bad := req
	bad.Token = tok[1:] + "0"
	if resp, err := SendHermesPin(run, sock(pid), bad, 5*time.Second); err != nil || resp.OK {
		t.Fatalf("a refusal: %+v %v", resp, err)
	}

	// standIn binds a socket at path, in place of whatever is there, and
	// answers one request with --no-policy, sending fd along when it is >= 0.
	standIn := func(t *testing.T, path string, fd int) {
		t.Helper()
		_ = os.Remove(path)
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			c, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
				return
			}
			var oob []byte
			if fd >= 0 {
				oob = syscall.UnixRights(fd)
			}
			_, _, _ = c.WriteMsgUnix([]byte(`{"ok":true,"no_policy":true}`+"\n"), oob, nil)
		}()
	}
	openLock := func(t *testing.T, id string) int {
		t.Helper()
		f, err := os.OpenFile(filepath.Join(run, id+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return int(f.Fd())
	}
	for _, tt := range []struct {
		name string
		id   string
		fd   func(t *testing.T) int
	}{
		{"no open file, in the supervisor's place", pid, func(*testing.T) int { return -1 }},
		{"its own open of the held lock file", pid, func(t *testing.T) int { return openLock(t, pid) }},
		{"a file that is not the lock", pid, func(t *testing.T) int {
			f, err := os.CreateTemp(t.TempDir(), "x")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.Close() })
			return int(f.Fd())
		}},
		{"a parent id with no lock file", other, func(*testing.T) int { return -1 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			standIn(t, sock(tt.id), tt.fd(t))
			resp, err := SendHermesPin(run, sock(tt.id), req, 5*time.Second)
			if !errors.Is(err, ErrUnproven) {
				t.Fatalf("got %+v %v, want ErrUnproven", resp, err)
			}
		})
	}
	// #136 review: a reply carrying six open files (the lock's among them)
	// is refused, and every open file that arrived is closed. Cut by the
	// kernel (MSG_CTRUNC, a buffer lowered to four) it is refused too; Linux
	// drops what does not fit, but darwin still puts every file in the
	// receiver's table, so there the two past the buffer stay open (the real
	// buffer holds more than either kernel sends in one message).
	for i, tt := range []struct {
		name   string
		rights int
		leak   int
	}{
		{"six open files", maxRights, 0},
		{"cut by the kernel", 4, map[bool]int{true: 2, false: 0}[runtime.GOOS == "darwin"]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			own := fmt.Sprintf("3b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1%d", i)
			lockFD := openLock(t, own)
			if err := syscall.Flock(lockFD, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			fds := []int{lockFD}
			for range 5 {
				f, err := os.CreateTemp(t.TempDir(), "x")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = f.Close() })
				fds = append(fds, int(f.Fd()))
			}
			path := sock(own)
			ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			go func() {
				c, err := ln.AcceptUnix()
				if err != nil {
					return
				}
				defer func() { _ = c.Close() }()
				if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
					return
				}
				_, _, _ = c.WriteMsgUnix([]byte(`{"ok":true,"no_policy":true}`+"\n"), syscall.UnixRights(fds...), nil)
			}()
			old := maxRights
			maxRights = tt.rights
			t.Cleanup(func() { maxRights = old })
			before := openFDs(t)
			resp, err := SendHermesPin(run, path, req, 5*time.Second)
			if !errors.Is(err, ErrUnproven) {
				t.Fatalf("got %+v %v, want ErrUnproven", resp, err)
			}
			if after := openFDs(t); after != before+tt.leak {
				t.Errorf("open files %d before, %d after, want %d more", before, after, tt.leak)
			}
		})
	}
	// rightsIn reads a message the kernel cut as far as it goes.
	whole := syscall.UnixRights(7, 8, 9)
	if got := rightsIn(whole[:syscall.CmsgLen(8)]); len(got) != 2 || got[0] != 7 || got[1] != 8 {
		t.Errorf("rightsIn(cut) = %v, want [7 8]", got)
	}
	// The probes left the real supervisor's lock alone.
	if live, err := Live(run, pid); err != nil || !live {
		t.Fatalf("the supervisor's lock: %v %v", live, err)
	}
	// What this does not stop: a stand-in that holds a lock of its own, for a
	// parent id it made up, passes (docs/design.md, "Consent and the pin").
	t.Run("a stand-in holding its own lock is not stopped", func(t *testing.T) {
		const own = "2b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e"
		fd := openLock(t, own)
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		standIn(t, sock(own), fd)
		if resp, err := SendHermesPin(run, sock(own), req, 5*time.Second); err != nil || !resp.OK {
			t.Fatalf("got %+v %v", resp, err)
		}
	})
}

func TestSendDeliversHookEvent(t *testing.T) {
	run := filepath.Join(shortDir(t), "run")
	var mu sync.Mutex
	var got []Request
	s, err := Listen(run, pid, nil, func(r Request) { mu.Lock(); got = append(got, r); mu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	req := Request{Type: ReqHookEvent, Vendor: "claude", Event: "Stop", Payload: json.RawMessage(`{"session_id":"x"}`)}
	resp, err := Send(run, s.Path(), req, 5*time.Second)
	if err != nil || !resp.OK {
		t.Fatalf("Send: %+v %v", resp, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Event != "Stop" || string(got[0].Payload) != `{"session_id":"x"}` {
		t.Errorf("handler got %+v", got)
	}
}

func TestSendRefusesForeignSockets(t *testing.T) {
	base := shortDir(t)
	run := filepath.Join(base, "run")
	s := listen(t, run)
	ok := Request{Type: ReqPing}

	// A socket outside the run directory, with a valid name.
	other := filepath.Join(base, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(other, pid+".sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: stray, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	// A regular file with a socket's name in the run directory.
	file := filepath.Join(run, "00000000-0000-4000-8000-000000000000.sock")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, runDir, path, want string }{
		{"outside run dir", run, stray, "not in aggy's run directory"},
		{"run dir from env", other, stray, "not a private directory"},
		{"relative", run, "run/" + pid + ".sock", "not in aggy's run directory"},
		{"bad name", run, filepath.Join(run, "x.sock"), "not a supervisor socket name"},
		{"not a socket", run, file, "not a socket"},
		{"missing", run, filepath.Join(run, "00000000-0000-4000-8000-000000000001.sock"), "no such file"},
	}
	if err := os.Chmod(other, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Send(tt.runDir, tt.path, ok, time.Second); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Send: %v, want %q", err, tt.want)
			}
		})
	}
	// The real one works.
	if resp, err := Send(run, s.Path(), ok, time.Second); err != nil || resp.Type != "pong" {
		t.Errorf("ping: %+v %v", resp, err)
	}
	// A loosened run directory is refused.
	if err := os.Chmod(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Send(run, s.Path(), ok, time.Second); err == nil {
		t.Error("Send to a 0755 run directory worked")
	}
}

func TestSendChecksRequestFirst(t *testing.T) {
	run := filepath.Join(shortDir(t), "run")
	s := listen(t, run)
	big := `{"k":"` + strings.Repeat("x", MaxMessage) + `"}`
	for _, req := range []Request{
		{Type: ReqHookEvent, Vendor: "claude", Event: "Stop", Payload: json.RawMessage(big)},
		{Type: ReqHookEvent, Vendor: "nobody", Event: "Stop", Payload: json.RawMessage(`{}`)},
		{Type: ReqHookEvent, Vendor: "claude", Event: "bad event", Payload: json.RawMessage(`{}`)},
	} {
		if _, err := Send(run, s.Path(), req, time.Second); err == nil {
			t.Errorf("Send(%s/%s) worked", req.Vendor, req.Event)
		}
	}
}

// openFDs counts the test process's open files.
func openFDs(t *testing.T) int {
	t.Helper()
	es, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}
