//go:build unix && !linux && !darwin

package execrun

// leaderExited cannot wait without reaping here, so reap kills the group
// after cmd.Wait (see reap).
func leaderExited(int) bool { return false }
