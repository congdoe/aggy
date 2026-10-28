//go:build unix && !darwin && !linux

package supervisor

import (
	"errors"
	"net"
)

// peerUIDOf fails closed where aggy has no peer-credential call: every
// connection is refused.
func peerUIDOf(*net.UnixConn) (int, error) {
	return -1, errors.New("supervisor: peer credentials are not supported on this OS")
}
