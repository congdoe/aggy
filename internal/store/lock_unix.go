//go:build unix

package store

import (
	"os"
	"syscall"

	"github.com/congdoe/aggy/internal/safefs"
)

// lockFile takes an exclusive flock on path, creating it 0600, and returns
// the unlock function. flock locks belong to the open file description, so
// two opens in one process exclude each other too.
func lockFile(path string) (func(), error) {
	f, err := safefs.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
