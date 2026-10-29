//go:build unix && !linux && !darwin

package execrun

import (
	"context"
	"errors"
	"net/netip"
	"syscall"
)

func proxyAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// listenerOwned cannot check who listens here, so it fails closed: no
// proxy runs on this system (T6780).
// TODO(T6780): a listener-owner check for the BSDs.
func listenerOwned(context.Context, netip.AddrPort, int) error {
	return errors.New("aggy cannot check which process listens on this system")
}
