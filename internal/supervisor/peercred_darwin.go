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

// procInfoOf reads pid's parent and start time (p_starttime, in ns).
func procInfoOf(pid int) (ppid int, start int64, err error) {
	k, err := kinfo(pid)
	if err != nil {
		return -1, 0, err
	}
	return int(k.Eproc.Ppid), k.Proc.P_starttime.Nano(), nil
}

// peerInstanceOf pins the process at pid by its start time, read now (at
// accept): same reports whether pid still names that process, so a pid
// reused later, by any process, does not count.
//
// shortcut: a pid freed and reused between the peer's connect and this
// read is not caught (G38-8). The gap is normally microseconds, but has
// no bound: a process of the user's can SIGSTOP aggy, which --direct does
// not show. darwin has no pidfd. Its audit token (LOCAL_PEERTOKEN) holds
// the peer's pidversion and can be read without unsafe (a 32-byte
// getsockopt), but nothing aggy may call reads a live pid's pidversion
// to compare it with (proc_pidinfo is libproc, cgo). Upgrade when x/sys
// gains one. The lineage is a hint either way (T6664).
func peerInstanceOf(_ *net.UnixConn, pid int) (same func() bool, release func(), err error) {
	start := func() (int64, error) {
		_, st, err := procInfoOf(pid)
		return st, err
	}
	t0, err := start()
	if err != nil {
		return nil, nil, err
	}
	return func() bool { t, err := start(); return err == nil && t == t0 }, func() {}, nil
}
