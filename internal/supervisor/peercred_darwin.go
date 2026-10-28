//go:build darwin

package supervisor

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUIDOf reads the peer's uid with LOCAL_PEERCRED (getpeereid). The
// stdlib syscall package has no call for it on darwin.
func peerUIDOf(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	uid := -1
	var cerr error
	err = raw.Control(func(fd uintptr) {
		x, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			cerr = err
			return
		}
		uid = int(x.Uid)
	})
	if err != nil {
		return -1, err
	}
	return uid, cerr
}
