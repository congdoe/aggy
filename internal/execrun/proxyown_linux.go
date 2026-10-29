package execrun

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/congdoe/aggy/internal/safefs"
)

// proxyAttr starts the proxy as the leader of a new session (and so of
// its own process group), and asks the kernel to send it SIGTERM if aggy
// dies first. Pdeathsig follows the thread that started it, which the Go
// runtime keeps for the process's life unless a locked goroutine exits.
func proxyAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Pdeathsig: syscall.SIGTERM}
}

// maxProc caps a read under /proc.
const maxProc = 8 << 20

func readProc(name string) ([]byte, error) {
	f, err := safefs.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxProc))
}

// listenerOwned checks that every socket listening on addr is an open
// descriptor of a process in process group pgid (HIGH-1): the LISTEN
// rows (state 0A) for addr in this network namespace's /proc/net/tcp or
// tcp6 give their inodes, and each must be a socket:[inode] link under
// /proc/<pid>/fd of a process whose pgrp is pgid. A uid match is not
// enough: another process of the same user could hold the port. Any
// read that fails fails the check.
func listenerOwned(_ context.Context, addr netip.AddrPort, pgid int) error {
	table := "tcp"
	if addr.Addr().Is6() {
		table = "tcp6"
	}
	// Not /proc/net, which is a symlink to self/net.
	data, err := readProc("/proc/" + strconv.Itoa(os.Getpid()) + "/net/" + table)
	if err != nil {
		return err
	}
	inodes := listenInodes(data, procNetAddr(addr))
	if len(inodes) == 0 {
		return errNoListener
	}
	socks, err := groupSockets(pgid)
	if err != nil {
		return err
	}
	for _, ino := range inodes {
		if !socks["socket:["+ino+"]"] {
			return fmt.Errorf("a process outside the proxy's group listens on %s", addr)
		}
	}
	return nil
}

// procNetAddr is addr as /proc/net/tcp and tcp6 print it: each 32-bit
// word of the address in host byte order, in hex, then the port.
func procNetAddr(addr netip.AddrPort) string {
	b := addr.Addr().AsSlice()
	var sb strings.Builder
	for i := 0; i+4 <= len(b); i += 4 {
		fmt.Fprintf(&sb, "%08X", binary.NativeEndian.Uint32(b[i:i+4]))
	}
	fmt.Fprintf(&sb, ":%04X", addr.Port())
	return sb.String()
}

// listenInodes are the inodes of the LISTEN rows for local in a
// /proc/net/tcp table.
func listenInodes(data []byte, local string) []string {
	var out []string
	for _, line := range strings.Split(string(data), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 9 && strings.EqualFold(f[1], local) && f[3] == "0A" && f[9] != "0" {
			out = append(out, f[9])
		}
	}
	return out
}

// groupSockets are the socket:[inode] links open in the processes of
// process group pgid.
func groupSockets(pgid int) (map[string]bool, error) {
	ents, err := safefs.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	socks := map[string]bool{}
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		stat, err := readProc("/proc/" + e.Name() + "/stat")
		if err != nil || statPgrp(stat) != pgid {
			continue // gone, or another user's
		}
		dir := "/proc/" + e.Name() + "/fd"
		fds, err := safefs.ReadDir(dir)
		if err != nil {
			return nil, err // a member aggy cannot read: fail closed
		}
		for _, fd := range fds {
			if l, err := safefs.Readlink(dir + "/" + fd.Name()); err == nil && strings.HasPrefix(l, "socket:[") {
				socks[l] = true
			}
		}
	}
	return socks, nil
}

// statPgrp is the process group in a /proc/<pid>/stat line, -1 when it
// cannot be read: the fields after the last ')' are state, ppid, pgrp.
func statPgrp(b []byte) int {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return -1
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 3 {
		return -1
	}
	n, err := strconv.Atoi(f[2])
	if err != nil {
		return -1
	}
	return n
}
