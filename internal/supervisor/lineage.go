//go:build unix

package supervisor

import (
	"bytes"
	"errors"
	"strconv"
	"syscall"
)

// Peer-pid lineage (D20-7, D23-8). Every process the CLI starts inherits
// AGGY_SOCKET, and so does any process of the user's that copies it, so
// the socket's uid check alone cannot tell the on-screen CLI's hooks
// from anything else of the user's. The server also takes the peer's pid
// (LOCAL_PEERPID on darwin, SO_PEERCRED on Linux, both read when it
// connected) and walks its parents, at most maxLineageDepth steps, to the
// CLI's own pid (SetRoot). A request whose sender gets there is marked
// FromSession.
//
// Lineage proves "came from this session's process tree", not "came
// from the CLI itself": the CLI's own tool subprocesses (a nested
// claude -p or codex exec run by the agent's shell tool, or the agent
// piping JSON into aggy hook) descend from it too. A process that the
// CLI started and that outlived it, or that daemonized, is reparented and
// falls out of the tree. So a FromSession event is still a hint for
// switching (T6664), and Codex id claims are further checked against the
// rollout itself (internal/cli, sessionIndex).
//
// The answer is worked out once per connection, at accept, before any
// request is read, and bound to the process that connected, not just its
// pid (G38-1): peerInstance pins that process (a pidfd on Linux, its
// start time otherwise) before the walk and checks it again after. A pid
// is otherwise easy to have reused: a process outside the tree connects,
// hands the connection on, and exits, and pids are handed out again soon
// (pid_max 99999 on darwin, often 32768 on Linux) to whatever the CLI
// starts next. A connection that is not from the session is closed after
// its first request, so it cannot be kept open waiting for that.
//
// Any failure (no pid, no identity, a process gone mid-walk, an OS
// without the calls, no root within rootWait) is "not in the lineage":
// id claims and switch-triggering requests then fail closed, while
// informational hook events are still passed on, unmarked.

// maxLineageDepth bounds the parent walk. A hook runs a few levels below
// the CLI (CLI, shell, aggy hook; a node launcher adds one).
const maxLineageDepth = 64

// parentPID returns a process's parent pid, and peerInstance pins the
// process that connected (see peerInstanceOf); tests replace them.
var (
	parentPID    = parentPIDOf
	peerInstance = peerInstanceOf
)

// descends reports whether pid is root or descends from it. The caller
// pins pid's process around it (peerInstance).
func descends(pid, root int) bool {
	if pid <= 0 || root <= 0 {
		return false
	}
	for range maxLineageDepth {
		if pid == root {
			return true
		}
		if pid <= 1 {
			return false
		}
		p, err := parentPID(pid)
		if err != nil || p == pid {
			return false
		}
		pid = p
	}
	return false
}

// pidfdFallback reports whether a failed SO_PEERPIDFD may fall back to
// the start-time check: only when the kernel lacks the option.
func pidfdFallback(err error) bool { return errors.Is(err, syscall.ENOPROTOOPT) }

// parseStat reads the parent pid (field 4) and start time (field 22, in
// clock ticks since boot) from a Linux /proc/<pid>/stat line (proc(5)).
// They follow the command name in parentheses, which may itself hold
// spaces and parentheses, so the last ")" ends it.
func parseStat(b []byte) (ppid int, start uint64, err error) {
	bad := errors.New("supervisor: bad /proc stat line")
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return -1, 0, bad
	}
	f := bytes.Fields(b[i+1:]) // field 3 (state) on
	if len(f) < 20 {
		return -1, 0, bad
	}
	if ppid, err = strconv.Atoi(string(f[1])); err != nil {
		return -1, 0, bad
	}
	if start, err = strconv.ParseUint(string(f[19]), 10, 64); err != nil {
		return -1, 0, bad
	}
	return ppid, start, nil
}
