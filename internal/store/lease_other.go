//go:build !unix

package store

import "errors"

var errNoLocks = errors.New("store: file locks are not supported here")

func takeLock(string) (func(), error) { return func() {}, nil }

func lockHeld(string) (bool, error) { return false, errNoLocks }

// pidAlive cannot check a pid here, so it assumes the holder is alive.
// Windows support is a later milestone.
func pidAlive(int) bool { return true }
