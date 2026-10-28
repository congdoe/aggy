//go:build unix

package execrun

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group and makes the
// timeout kill the whole group, so a CLI's own children die with it.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
