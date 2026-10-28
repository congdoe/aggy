//go:build darwin || linux

package main

import (
	"syscall"
	"unsafe"
)

// The fake sets raw mode with ioctls of its own, so it stays stdlib-only:
// tests build it with a temp HOME and so an empty module cache.

type termState struct{ t syscall.Termios }

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// makeRaw sets fd to raw mode, as cfmakeraw does, and returns the old state.
func makeRaw(fd int) (*termState, error) {
	var t syscall.Termios
	if err := ioctl(fd, ioctlGet, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	old := &termState{t}
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	return old, ioctl(fd, ioctlSet, unsafe.Pointer(&t))
}

func restoreTerm(fd int, s *termState) error {
	return ioctl(fd, ioctlSet, unsafe.Pointer(&s.t))
}

// termSize returns fd's rows and columns.
func termSize(fd int) (rows, cols int) {
	var ws struct{ Row, Col, X, Y uint16 }
	_ = ioctl(fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws))
	return int(ws.Row), int(ws.Col)
}
