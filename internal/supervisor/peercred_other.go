//go:build unix && !darwin && !linux

package supervisor

import (
	"errors"
	"net"
)

// peerCredOf fails closed where aggy has no peer-credential call: every
// connection is refused.
func peerCredOf(*net.UnixConn) (uid, pid int, err error) {
	return -1, -1, errors.New("supervisor: peer credentials are not supported on this OS")
}

// procInfoOf fails closed: no peer is in the lineage.
func procInfoOf(int) (ppid int, start int64, err error) {
	return -1, 0, errors.New("supervisor: process parents are not supported on this OS")
}

// peerInstanceOf fails closed: no peer is in the lineage.
func peerInstanceOf(*net.UnixConn, int) (same func() bool, release func(), err error) {
	return nil, nil, errors.New("supervisor: process identity is not supported on this OS")
}
