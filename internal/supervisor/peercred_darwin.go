//go:build darwin

package supervisor

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerCredOf reads the peer's uid with LOCAL_PEERCRED (getpeereid) and
// its pid with LOCAL_PEERPID, both taken when it connected. The stdlib
// syscall package has no call for either on darwin.
func peerCredOf(c *net.UnixConn) (uid, pid int, err error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, -1, err
	}
	uid, pid = -1, -1
	var cerr error
	err = raw.Control(func(fd uintptr) {
		x, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			cerr = err
			return
		}
		p, err := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if err != nil {
			cerr = err
			return
		}
		uid, pid = int(x.Uid), p
	})
	if err != nil {
		return -1, -1, err
	}
	return uid, pid, cerr
}

// parentPIDOf reads pid's parent with sysctl kern.proc.pid, without cgo.
func parentPIDOf(pid int) (int, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return -1, err
	}
	if int(k.Proc.P_pid) != pid {
		return -1, fmt.Errorf("supervisor: sysctl answered for pid %d, not %d", k.Proc.P_pid, pid)
	}
	return int(k.Eproc.Ppid), nil
}
