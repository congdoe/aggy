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

// killGroup kills what is left of the child's process group once the
// leader has exited (and been reaped). The group id is the leader's
// pid; the kernel does not hand that pid out again while the group
// still has members, so the kill reaches only them.
func killGroup(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
