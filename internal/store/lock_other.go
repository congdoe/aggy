//go:build !unix

package store

// lockFile is a no-op where flock does not exist. Windows support is a
// later milestone and will need LockFileEx here.
func lockFile(string) (func(), error) { return func() {}, nil }
