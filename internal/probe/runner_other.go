//go:build !unix

package probe

import "os/exec"

// setProcessGroup is a no-op where process groups do not exist; the
// timeout kills the process itself.
func setProcessGroup(*exec.Cmd) {}
