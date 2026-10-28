//go:build linux

package supervisor

import (
	"io"
	"net"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/congdoe/aggy/internal/safefs"
)

// peerCredOf reads the peer's uid and pid with SO_PEERCRED, both taken
// when it connected.
func peerCredOf(c *net.UnixConn) (uid, pid int, err error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, -1, err
	}
	uid, pid = -1, -1
	var cerr error
	err = raw.Control(func(fd uintptr) {
		u, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			cerr = err
			return
		}
		uid, pid = int(u.Uid), int(u.Pid)
	})
	if err != nil {
		return -1, -1, err
	}
	return uid, pid, cerr
}

// parentPIDOf reads pid's parent from /proc/<pid>/stat, through safefs.
func parentPIDOf(pid int) (int, error) {
	f, err := safefs.Open("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return -1, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return -1, err
	}
	return parseStatPPID(b)
}
