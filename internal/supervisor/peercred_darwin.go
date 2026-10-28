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

// kinfo reads pid's process entry with sysctl kern.proc.pid, without cgo.
func kinfo(pid int) (*unix.KinfoProc, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return nil, err
	}
	if int(k.Proc.P_pid) != pid {
		return nil, fmt.Errorf("supervisor: sysctl answered for pid %d, not %d", k.Proc.P_pid, pid)
	}
	return k, nil
}

// parentPIDOf reads pid's parent.
func parentPIDOf(pid int) (int, error) {
	k, err := kinfo(pid)
	if err != nil {
		return -1, err
	}
	return int(k.Eproc.Ppid), nil
}

// peerInstanceOf pins the process at pid by its start time, read now (at
// accept): same reports whether pid still names that process, so a pid
// reused later, by any process, does not count.
//
// shortcut: darwin has no pidfd, and its audit token (LOCAL_PEERTOKEN)
// needs a raw getsockopt aggy has no call for; a pid freed and reused
// between the peer's connect and this read (microseconds) is not caught.
func peerInstanceOf(_ *net.UnixConn, pid int) (same func() bool, release func(), err error) {
	start := func() (int64, error) {
		k, err := kinfo(pid)
		if err != nil {
			return 0, err
		}
		return k.Proc.P_starttime.Nano(), nil
	}
	t0, err := start()
	if err != nil {
		return nil, nil, err
	}
	return func() bool { t, err := start(); return err == nil && t == t0 }, func() {}, nil
}
