//go:build unix

package store

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLockWaitHonorsContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := lockFile(context.Background(), p, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = lockFile(ctx, p, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lockFile = %v, want the context's error", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("lockFile returned after %s, long after the context ended", d)
	}
}

func TestLockWaitIsBounded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := lockFile(context.Background(), p, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lockFile(context.Background(), p, 100*time.Millisecond)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("lockFile = %v, want ErrLockTimeout", err)
	}
	// Once released, the lock is free again.
	unlock()
	unlock2, err := lockFile(context.Background(), p, time.Second)
	if err != nil {
		t.Fatalf("lockFile after release = %v", err)
	}
	unlock2()
}

func TestLockUnsupportedFallsBack(t *testing.T) {
	for _, e := range []syscall.Errno{syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOLCK} {
		t.Run(e.Error(), func(t *testing.T) {
			old := flock
			flock = func(int, int) error { return e }
			t.Cleanup(func() { flock = old })

			s, _ := openTemp(t)
			ws := s.Warnings()
			if len(ws) != 1 || !errors.Is(ws[0], ErrLockUnsupported) || !errors.Is(ws[0], e) {
				t.Fatalf("Warnings = %v, want one ErrLockUnsupported wrapping %v", ws, e)
			}
			if v, err := s.Version(context.Background()); err != nil || v < 1 {
				t.Errorf("store not migrated without a lock: %d, %v", v, err)
			}
		})
	}
}

func TestLockOtherErrorFails(t *testing.T) {
	old := flock
	flock = func(int, int) error { return syscall.EBADF }
	t.Cleanup(func() { flock = old })
	t.Setenv("HOME", t.TempDir())
	if _, err := Open(context.Background(), filepath.Join(t.TempDir(), FileName)); !errors.Is(err, syscall.EBADF) {
		t.Errorf("Open = %v, want EBADF", err)
	}
}
