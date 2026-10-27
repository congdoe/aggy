//go:build unix

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/congdoe/aggy/internal/safefs"
)

// flock is syscall.Flock; tests replace it to simulate file systems.
var flock = syscall.Flock

// lockFile takes an exclusive flock on path, creating it 0600, and returns
// the unlock function. flock locks belong to the open file description, so
// two opens in one process exclude each other too.
//
// It never blocks in the kernel: it polls with LOCK_NB and a backoff, and
// gives up when ctx ends (returning ctx's error) or after wait (returning
// ErrLockTimeout). On a file system without flock support (ENOTSUP,
// EOPNOTSUPP, ENOLCK) it returns an error matching ErrLockUnsupported and
// a no-op unlock, so the caller can choose to go on unlocked.
func lockFile(ctx context.Context, path string, wait time.Duration) (func(), error) {
	f, err := safefs.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())
	deadline := time.Now().Add(wait)
	delay := time.Millisecond
	for {
		err := flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return func() {
				_ = flock(fd, syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
			// Held by another process or open; wait below.
		case errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOLCK):
			_ = f.Close()
			return func() {}, fmt.Errorf("%w: %s: %w", ErrLockUnsupported, path, err)
		default:
			_ = f.Close()
			return nil, fmt.Errorf("store: lock %s: %w", path, err)
		}
		left := time.Until(deadline)
		if left <= 0 {
			_ = f.Close()
			return nil, fmt.Errorf("%w: %s after %s", ErrLockTimeout, path, wait)
		}
		timer := time.NewTimer(min(delay, left))
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 20*time.Millisecond)
	}
}
