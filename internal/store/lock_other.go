//go:build !unix

package store

import (
	"context"
	"time"
)

// lockFile reports ErrLockUnsupported where flock does not exist. Windows
// support is a later milestone and will need LockFileEx here.
func lockFile(context.Context, string, time.Duration) (func(), error) {
	return func() {}, ErrLockUnsupported
}
