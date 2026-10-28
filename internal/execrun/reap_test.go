//go:build linux || darwin

package execrun

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestLeaderExitedKeepsZombie (T6748): leaderExited returns once the
// leader exits but leaves it unreaped, so its pid, and the group id
// killGroup uses, cannot be handed out again before the kill.
func TestLeaderExitedKeepsZombie(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 3")
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if !leaderExited(cmd.Process.Pid) {
		t.Fatal("leaderExited = false")
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Errorf("the leader was reaped before Wait: %v", err)
	}
	killGroup(cmd)
	if err := cmd.Wait(); cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 3 {
		t.Errorf("Wait: %v, state %v", err, cmd.ProcessState)
	}
}
