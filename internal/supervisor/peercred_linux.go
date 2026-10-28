//go:build linux

package supervisor

import (
	"bytes"
	"errors"
	"io"
	"math"
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

// readStat reads /proc/<pid>/stat through safefs.
func readStat(pid string) ([]byte, error) {
	f, err := safefs.Open("/proc/" + pid + "/stat")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, 4096))
}

// procInfoOf reads pid's parent and start time (clock ticks since boot)
// from /proc/<pid>/stat.
func procInfoOf(pid int) (ppid int, start int64, err error) {
	b, err := readStat(strconv.Itoa(pid))
	if err != nil {
		return -1, 0, err
	}
	ppid, st, err := parseStat(b)
	if st > math.MaxInt64 {
		return -1, 0, errors.New("supervisor: bad start time")
	}
	return ppid, int64(st), err
}

// peerInstanceOf pins the process that connected. With SO_PEERPIDFD the
// kernel hands over a pidfd for that very process, taken at connect, and
// same reports whether it still has pid (a pidfd's fdinfo shows -1 once
// it has exited), so no reuse of the pid counts. Only a kernel without
// the option (ENOPROTOOPT, before 6.5) falls back to pinning the process
// at pid by its start time, read now; any other error, such as EINVAL
// for a peer that has exited (6.5 to 6.14), is no identity (G38-7).
//
// shortcut: on that fallback, a pid freed and reused between the peer's
// connect and this read is not caught (G38-8): normally microseconds, but
// unbounded if a process of the user's SIGSTOPs aggy. Kernels from 6.5
// take the pidfd path, which has no such gap.
func peerInstanceOf(c *net.UnixConn, pid int) (same func() bool, release func(), err error) {
	pfd, err := peerPidfd(c)
	switch {
	case err == nil:
		fdinfo := "/proc/self/fdinfo/" + strconv.Itoa(pfd)
		return func() bool { return fdinfoPid(fdinfo) == pid }, func() { _ = unix.Close(pfd) }, nil
	case !pidfdFallback(err):
		return nil, nil, err
	}
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

// getPeerPidfd is getsockopt SO_PEERPIDFD on a socket; tests replace it.
var getPeerPidfd = func(fd int) (int, error) {
	return unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PEERPIDFD)
}

// peerPidfd asks for the peer's pidfd (SO_PEERPIDFD, Linux 6.5).
func peerPidfd(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	pfd, gerr := -1, error(nil)
	if err := raw.Control(func(fd uintptr) { pfd, gerr = getPeerPidfd(int(fd)) }); err != nil {
		return -1, err
	}
	if gerr == nil && pfd < 0 {
		gerr = errors.New("supervisor: no pidfd")
	}
	return pfd, gerr
}

// fdinfoPid reads the Pid: line of a pidfd's fdinfo, or -1.
func fdinfoPid(path string) int {
	b, err := safefs.ReadFile(path)
	if err != nil {
		return -1
	}
	for _, ln := range bytes.Split(b, []byte("\n")) {
		if v, ok := bytes.CutPrefix(ln, []byte("Pid:")); ok {
			if n, err := strconv.Atoi(string(bytes.TrimSpace(v))); err == nil {
				return n
			}
		}
	}
	return -1
}
