//go:build !darwin

package safefs

// IsAPFS is false off darwin.
func IsAPFS(string) bool { return false }
