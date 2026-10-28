package execrun

import "golang.org/x/sys/unix"

// leaderExited blocks until process pid has exited, without reaping it
// (waitid with WNOWAIT), so its pid and group id stay held until
// cmd.Wait. It reports false when it cannot tell, and the caller then
// reaps first. The context's kill (exec.Cmd.Cancel) still ends the
// leader meanwhile: os/exec watches the context from Start.
func leaderExited(pid int) bool {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return err == nil
		}
	}
}
