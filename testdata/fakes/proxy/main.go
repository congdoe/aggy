// Command fake-proxy stands in for a tool's local proxy (T6780) in
// tests: fake-proxy <mode> <addr> <dir>. It writes its pid and its
// environment into dir, starts a grandchild (/bin/sleep 60, its pid in
// dir/child) that must die with its process group, and serves HTTP on
// addr: GET /health answers 200, or 500 in mode "unhealthy". Mode
// "exit" prints a line and exits 1 at once. Mode "escape" (T6780
// HIGH-2), 1.5 seconds after its first request, hands its listener to a
// process in its own session (mode "holdfd", its pid in dir/escaped),
// which keeps the port when the proxy dies and accepts nothing; it
// exits after 20 seconds. It never forwards anything.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "freeport" {
		// A loopback port nothing listens on, for a test's definition.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(1)
		}
		fmt.Println(ln.Addr().(*net.TCPAddr).Port)
		_ = ln.Close()
		return
	}
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: fake-proxy <mode> <addr> <dir>")
		os.Exit(2)
	}
	mode, addr, dir := os.Args[1], os.Args[2], os.Args[3]
	if mode == "holdfd" {
		time.Sleep(20 * time.Second)
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "env"), []byte(strings.Join(os.Environ(), "\n")), 0o600)
	if mode == "exit" {
		fmt.Fprintln(os.Stderr, "fake-proxy: cannot bind")
		os.Exit(1)
	}
	sleep := exec.Command("/bin/sleep", "60")
	if err := sleep.Start(); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "child"), []byte(strconv.Itoa(sleep.Process.Pid)), 0o600)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		os.Exit(3)
	}
	var escape sync.Once
	_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "escape" {
			escape.Do(func() { go handOff(ln, addr, dir) })
		}
		if mode == "unhealthy" || r.URL.Path != "/health" {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

// handOff gives the listener to a process in its own session.
func handOff(ln net.Listener, addr, dir string) {
	time.Sleep(1500 * time.Millisecond)
	f, err := ln.(*net.TCPListener).File()
	if err != nil {
		return
	}
	c := exec.Command(os.Args[0], "holdfd", addr, dir)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.ExtraFiles = []*os.File{f}
	if c.Start() == nil {
		_ = os.WriteFile(filepath.Join(dir, "escaped"), []byte(strconv.Itoa(c.Process.Pid)), 0o600)
	}
}
