//go:build unix

package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/store"
)

// TestBorrowHelperProcess is the aggy borrow that TestBorrowSignalKillsChild
// signals. It runs only as that test's helper.
func TestBorrowHelperProcess(t *testing.T) {
	if os.Getenv("AGGY_TEST_BORROW_HELPER") != "1" {
		t.Skip("helper process only")
	}
	f := newBorrowFixture(t, adapter.Grok)
	f.scenario["grok"] = `{"sleep_ms":60000,"json":{"result":"late"}}`
	code, out, errOut := f.run(t, "news", "x")
	t.Logf("code %d\n%s\n%s", code, out, errOut)
}

// TestBorrowSignalKillsChild (#10-2): SIGTERM, SIGINT, or SIGHUP to aggy
// borrow kills the child, records the borrow as killed (interrupted),
// and removes the child's temp dir. Two signals close together (R-5,
// C17-7) still kill the child before aggy can end.
func TestBorrowSignalKillsChild(t *testing.T) {
	if os.Getenv("AGGY_TEST_BORROW_HELPER") != "" {
		t.Skip()
	}
	tests := []struct {
		name string
		sigs []syscall.Signal
	}{
		{"terminated", []syscall.Signal{syscall.SIGTERM}},
		{"interrupt", []syscall.Signal{syscall.SIGINT}},
		{"hangup", []syscall.Signal{syscall.SIGHUP}},
		{"hangup then terminated", []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM}},
		{"two terminated", []syscall.Signal{syscall.SIGTERM, syscall.SIGTERM}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "aggy")
			record := filepath.Join(t.TempDir(), "rec.jsonl")
			cmd := exec.Command(os.Args[0], "-test.run=^TestBorrowHelperProcess$", "-test.v")
			cmd.Env = append(os.Environ(), "AGGY_TEST_BORROW_HELPER=1", "AGGY_TEST_STATE="+state,
				"AGGY_TEST_RECORD="+record, "HOME="+t.TempDir())
			var out lockedBuffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill() }()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			// stop kills aggy borrow and waits for it, so out is read only
			// once nothing writes to it.
			stop := func() string {
				_ = cmd.Process.Kill()
				t.Logf("aggy borrow killed: %v", <-done)
				return out.String()
			}
			var child fakecli.Record
			// The child's record is the readiness marker. Under load the
			// start can take long, so the wait ends early only when aggy
			// borrow exits first; the deadline is a backstop.
			deadline := time.Now().Add(3 * time.Minute)
			for child.PID == 0 {
				select {
				case werr := <-done:
					t.Fatalf("aggy borrow exited (%v) before the child started:\n%s", werr, out.String())
				case <-time.After(50 * time.Millisecond):
				}
				if time.Now().After(deadline) {
					t.Fatalf("the child never started:\n%s", stop())
				}
				for _, r := range fakecli.Records(t, record) {
					if r.Mode == "headless" {
						child = r
					}
				}
			}
			for _, sig := range tt.sigs {
				// A second signal may find aggy already gone.
				_ = cmd.Process.Signal(sig)
			}
			select {
			case werr := <-done:
				t.Logf("aggy borrow exited: %v", werr)
			case <-time.After(15 * time.Second):
				t.Fatalf("aggy borrow did not exit after %v:\n%s", tt.sigs, stop())
			}
			gone := time.Now().Add(5 * time.Second)
			for syscall.Kill(child.PID, 0) == nil {
				if time.Now().After(gone) {
					_ = syscall.Kill(child.PID, syscall.SIGKILL)
					t.Fatalf("child %d outlived aggy borrow", child.PID)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if len(tt.sigs) > 1 {
				return // the second signal may end aggy before its cleanup
			}
			if _, err := os.Stat(child.Cwd); !os.IsNotExist(err) {
				t.Errorf("temp dir %s left behind: %v", child.Cwd, err)
			}
			t.Setenv("AGGY_HOME", state)
			s, err := store.OpenDefault(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			var status, outcome string
			if err := s.DB().QueryRow(`SELECT status, outcome FROM borrows`).Scan(&status, &outcome); err != nil {
				t.Fatal(err)
			}
			if status != "killed" || outcome != "interrupted" {
				t.Errorf("borrow %s/%s, want killed/interrupted:\n%s", status, outcome, out.String())
			}
		})
	}
}

// lockedBuffer is a bytes.Buffer that the exec copy goroutines and the
// test can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
