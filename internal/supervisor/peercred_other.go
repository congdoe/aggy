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

// parentPIDOf fails closed: no peer is in the lineage.
func parentPIDOf(int) (int, error) {
	return -1, errors.New("supervisor: process parents are not supported on this OS")
}
