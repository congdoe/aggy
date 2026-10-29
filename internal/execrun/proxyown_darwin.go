package execrun

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// proxyAttr starts the proxy as the leader of a new session, and so of
// its own process group. macOS has no parent-death signal.
func proxyAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// lsof is macOS's own lsof, part of the base system.
const lsof = "/usr/sbin/lsof"

// listenerOwned checks that every process listening on addr is in
// process group pgid (HIGH-1). macOS has no /proc, and its socket
// tables need sysctl structures or proc_pidfdinfo, which aggy cannot
// call without unsafe or cgo, so it asks the system's lsof, through
// ExecRunner (no stdin, a scrubbed env, a time limit): the PID and PGID
// of each process with a TCP LISTEN socket on exactly addr. There must
// be one, and every one must be in the group.
//
// Limit: without root, lsof sees only the user's own processes. Another
// user's process could share the port only through SO_REUSEPORT, which
// both sockets must set; then it goes unseen here.
func listenerOwned(ctx context.Context, addr netip.AddrPort, pgid int) error {
	host := addr.Addr().String()
	if addr.Addr().Is6() {
		host = "[" + host + "]"
	}
	out, err := ExecRunner(10*time.Second)(ctx, adapter.Command{Path: lsof,
		Args: []string{"-nP", "-a", "-iTCP@" + host + ":" + strconv.Itoa(int(addr.Port())), "-sTCP:LISTEN", "-Fpg"}})
	if err != nil {
		return fmt.Errorf("lsof: %w", err)
	}
	if out.ExitCode != 0 && len(out.Stdout) > 0 {
		return fmt.Errorf("lsof exited %d", out.ExitCode)
	}
	pgids := lsofPgids(string(out.Stdout))
	if len(pgids) == 0 {
		return errNoListener
	}
	for _, g := range pgids {
		if g != pgid {
			return fmt.Errorf("a process outside the proxy's group listens on %s", addr)
		}
	}
	return nil
}

// lsofPgids are the g (process group) fields of lsof -F output, one per
// process; a process without one counts as group -1.
func lsofPgids(out string) []int {
	var gs []int
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "p"):
			gs = append(gs, -1)
		case strings.HasPrefix(l, "g") && len(gs) > 0:
			if n, err := strconv.Atoi(l[1:]); err == nil {
				gs[len(gs)-1] = n
			}
		}
	}
	return gs
}
