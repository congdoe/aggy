//go:build unix

package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func seedSession(t *testing.T, s *Store, id, vendor, session string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	_ = s.InsertParent(ctx, Parent{ID: "p", Dir: "/x", CreatedAt: now, LastActive: now})
	if err := s.InsertChild(ctx, Child{ID: id, ParentID: "p", Vendor: vendor, VendorSessionID: session, Cwd: "/x", CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseOneHolder(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	seedSession(t, s, "c1", "claude", "sess-1")

	l, err := s.AcquireLease(ctx, "claude", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AcquireLease(ctx, "claude", "sess-1")
	var be *BusyError
	if !errors.As(err, &be) || be.PID != os.Getpid() || !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire = %v, want busy by this pid", err)
	}
	if pid, alive, err := s.LeaseHolder(ctx, "claude", "sess-1"); err != nil || !alive || pid != os.Getpid() {
		t.Errorf("LeaseHolder = %d, %v, %v", pid, alive, err)
	}
	if err := l.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(ctx); err != nil {
		t.Errorf("second release = %v", err)
	}
	l2, err := s.AcquireLease(ctx, "claude", "sess-1")
	if err != nil {
		t.Fatalf("acquire after release = %v", err)
	}
	_ = l2.Release(ctx)
	if _, err := s.AcquireLease(ctx, "claude", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session = %v", err)
	}
	entries, _ := os.ReadDir(s.leaseDir())
	if len(entries) != 0 {
		t.Errorf("lock files left: %d", len(entries))
	}
}

func TestLeaseConcurrentAcquire(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	seedSession(t, s, "c1", "grok", "sess-1")
	var wg sync.WaitGroup
	var mu sync.Mutex
	var won []*Lease
	for range 8 {
		wg.Go(func() {
			l, err := s.AcquireLease(ctx, "grok", "sess-1")
			if err == nil {
				mu.Lock()
				won = append(won, l)
				mu.Unlock()
			} else if !errors.Is(err, ErrBusy) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(won) != 1 {
		t.Fatalf("%d holders, want 1", len(won))
	}
	_ = won[0].Release(ctx)
}

// plantLease writes a lease row as another process would have left it.
func plantLease(t *testing.T, s *Store, child string, pid int, nonce string, lockFile bool) {
	t.Helper()
	if lockFile {
		if err := os.MkdirAll(s.leaseDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.lockPath(nonce), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec(`UPDATE children SET lease_pid = ?, lease_nonce = ?, lease_at = ? WHERE id = ?`,
		pid, nonce, time.Now().UnixMilli(), child); err != nil {
		t.Fatal(err)
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestLeaseStale(t *testing.T) {
	tests := []struct {
		name     string
		pid      func(t *testing.T) int
		lockFile bool
		wantBusy bool
	}{
		// The holder died: the kernel dropped its lock.
		{"dead pid, lock free", deadPID, true, false},
		// PID reuse: the recorded pid now belongs to a live, unrelated
		// process, but the nonce lock is free, so the lease is stale.
		{"reused pid, lock free", func(*testing.T) int { return os.Getpid() }, true, false},
		// No lock file: the holder is gone, even when its pid is in use
		// again (#10-11).
		{"dead pid, no lock file", deadPID, false, false},
		{"reused pid, no lock file", func(*testing.T) int { return os.Getpid() }, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := openTemp(t)
			ctx := context.Background()
			seedSession(t, s, "c1", "codex", "thread-1")
			plantLease(t, s, "c1", tt.pid(t), "deadbeef", tt.lockFile)
			l, err := s.AcquireLease(ctx, "codex", "thread-1")
			if tt.wantBusy {
				if !errors.Is(err, ErrBusy) {
					t.Fatalf("acquire = %v, want busy", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("stale lease not reclaimed: %v", err)
			}
			if _, err := os.Stat(s.lockPath("deadbeef")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("stale lock file kept: %v", err)
			}
			_ = l.Release(ctx)
		})
	}
}

// TestLeaseHolderProcessDies: a lease held by another process is busy
// while it runs and reclaimable once it is killed, through the kernel
// dropping its flock.
func TestLeaseHolderProcessDies(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	seedSession(t, s, "c1", "claude", "sess-x")

	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseHelperProcess$")
	cmd.Env = append(os.Environ(), "AGGY_LEASE_HELPER_DB="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "held\n" {
		t.Fatalf("helper said %q, %v", line, err)
	}
	_, err = s.AcquireLease(ctx, "claude", "sess-x")
	var be *BusyError
	if !errors.As(err, &be) || be.PID != cmd.Process.Pid {
		t.Fatalf("acquire while helper lives = %v, want busy by pid %d", err, cmd.Process.Pid)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	l, err := s.AcquireLease(ctx, "claude", "sess-x")
	if err != nil {
		t.Fatalf("acquire after the holder died = %v", err)
	}
	_ = l.Release(ctx)
}

// TestLeaseHelperProcess is the lease holder for TestLeaseHolderProcessDies.
func TestLeaseHelperProcess(t *testing.T) {
	path := os.Getenv("AGGY_LEASE_HELPER_DB")
	if path == "" {
		t.Skip("helper process only")
	}
	s, err := Open(context.Background(), path)
	if err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	if _, err := s.AcquireLease(context.Background(), "claude", "sess-x"); err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	fmt.Println("held")
	time.Sleep(time.Minute)
	os.Exit(0)
}

func TestLeaseDirNextToDB(t *testing.T) {
	s, path := openTemp(t)
	if got, want := s.leaseDir(), filepath.Join(filepath.Dir(path), "run", "leases"); got != want {
		t.Errorf("leaseDir = %s, want %s", got, want)
	}
}
