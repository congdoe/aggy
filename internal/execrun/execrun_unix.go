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

// killGroup kills what is left of the child's process group. Called
// while the leader has exited but is not yet reaped (leaderExited), the
// group id is still held by the zombie, so the kill reaches only the
// group's members, never a process that reused the pid.
func killGroup(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// reap waits for the leader, kills what is left of its group, and reaps
// it: the kill comes before the reap where leaderExited can wait without
// reaping (Linux, macOS). Elsewhere it comes after, and a group left
// empty whose pid the kernel handed out again at once would be killed
// too; a non-empty group keeps its id, so the window is a group that
// emptied itself and a pid reused in between (negligible, documented).
func reap(cmd *exec.Cmd) error {
	if leaderExited(cmd.Process.Pid) {
		killGroup(cmd)
		return cmd.Wait()
	}
	err := cmd.Wait()
	killGroup(cmd)
	return err
}
