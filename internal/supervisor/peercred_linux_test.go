//go:build linux

package supervisor

import (
	"net"
	"path/filepath"
	"syscall"
	"testing"
)

// TestPeerInstancePidfdErrors (G38-7): only a kernel without
// SO_PEERPIDFD falls back to the start-time check; EINVAL (a peer that
// exited, Linux 6.5 to 6.14) or any other error is no identity.
func TestPeerInstancePidfdErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool // an identity
	}{
		{"no such option", syscall.ENOPROTOOPT, true},
		{"peer exited", syscall.EINVAL, false},
		{"other", syscall.EBADF, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := getPeerPidfd
			getPeerPidfd = func(int) (int, error) { return -1, tt.err }
			t.Cleanup(func() { getPeerPidfd = old })
			got := make(chan bool, 1)
			oldI := peerInstance
			peerInstance = func(c *net.UnixConn, pid int) (func() bool, func(), error) {
				same, release, err := peerInstanceOf(c, pid)
				got <- err == nil
				return same, release, err
			}
			t.Cleanup(func() { peerInstance = oldI })
			s := listen(t, filepath.Join(shortDir(t), "run"))
			_ = ask(t, s, `{"type":"ping"}`+"\n")
			if g := <-got; g != tt.want {
				t.Errorf("identity = %v, want %v", g, tt.want)
			}
		})
	}
}
