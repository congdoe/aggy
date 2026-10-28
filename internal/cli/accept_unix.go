//go:build unix

package cli

// acceptOS is nil: accept works on Unix.
func acceptOS() error { return nil }
