//go:build linux

package supervisor

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUIDOf reads the peer's uid with SO_PEERCRED.
func peerUIDOf(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	uid := -1
	var cerr error
	err = raw.Control(func(fd uintptr) {
		u, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			cerr = err
			return
		}
		uid = int(u.Uid)
	})
	if err != nil {
		return -1, err
	}
	return uid, cerr
}
