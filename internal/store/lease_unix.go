//go:build unix

package store

import (
	"errors"
	"os"
	"syscall"

	"github.com/congdoe/aggy/internal/safefs"
)

// takeLock creates path and holds an exclusive flock on it until unlock.
// Where locks are unsupported it returns a no-op unlock, and liveness
// falls back to the pid.
func takeLock(path string) (func(), error) {
	f, err := safefs.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil && !lockUnsupported(err) {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// lockHeld reports whether some process holds the flock on path. It
// returns an error when path does not exist or locks are unsupported, so
// the caller can fall back.
func lockHeld(path string) (bool, error) {
	f, err := safefs.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	for {
		err = flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	switch {
	case err == nil:
		return false, nil // closing f drops the lock again
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
		return true, nil
	}
	return false, err
}

func lockUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOLCK)
}

// pidAlive reports whether a process with pid exists. EPERM means it
// exists under another user.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
