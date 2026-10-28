//go:build !unix

package execrun

import "os/exec"

// setProcessGroup is a no-op where process groups do not exist; the
// timeout kills the process itself.
func setProcessGroup(*exec.Cmd) {}

// reap waits for the child: there is no process group to kill.
func reap(cmd *exec.Cmd) error { return cmd.Wait() }
