//go:build unix

package supervisor

import (
	"bytes"
	"errors"
	"strconv"
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
// Any failure (no pid, a process gone mid-walk, an OS without the calls,
// no root yet) is "not in the lineage": id claims and switch-triggering
// requests then fail closed, while informational hook events are still
// passed on, unmarked.

// maxLineageDepth bounds the parent walk. A hook runs a few levels below
// the CLI (CLI, shell, aggy hook; a node launcher adds one).
const maxLineageDepth = 64

// parentPID returns a process's parent pid; tests replace it.
var parentPID = parentPIDOf

// descends reports whether pid is root or descends from it.
//
// shortcut: a pid read at connect time could be reused if the peer exits
// before the walk; the reuser is a process of the same uid (the uid check
// came first) that must also descend from the CLI. Upgrade to start-time
// checks if T6664 needs more.
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

// parseStatPPID reads the parent pid from a Linux /proc/<pid>/stat line:
// the fourth field, after the command name in parentheses, which may
// itself hold spaces and parentheses, so the last ")" ends it.
func parseStatPPID(b []byte) (int, error) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return -1, errors.New("supervisor: bad /proc stat line")
	}
	f := bytes.Fields(b[i+1:]) // state, ppid, ...
	if len(f) < 2 {
		return -1, errors.New("supervisor: bad /proc stat line")
	}
	return strconv.Atoi(string(f[1]))
}
