//go:build unix

package supervisor

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

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
