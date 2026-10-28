package execrun

import "syscall"

// leaderExited blocks until process pid has exited, without reaping it,
// so its pid and group id stay held until cmd.Wait. macOS has no waitid
// in the stdlib or golang.org/x/sys/unix, so it waits for the kqueue
// NOTE_EXIT event instead; a process already exited (a zombie, since
// only cmd.Wait reaps it) gives ESRCH, which counts as exited. It
// reports false when it cannot tell, and the caller then reaps first.
func leaderExited(pid int) bool {
	kq, err := syscall.Kqueue()
	if err != nil {
		return false
	}
	defer func() { _ = syscall.Close(kq) }()
	var ev syscall.Kevent_t
	syscall.SetKevent(&ev, pid, syscall.EVFILT_PROC, syscall.EV_ADD|syscall.EV_ONESHOT)
	ev.Fflags = syscall.NOTE_EXIT
	got := make([]syscall.Kevent_t, 1)
	for {
		n, err := syscall.Kevent(kq, []syscall.Kevent_t{ev}, got, nil)
		switch {
		case err == syscall.EINTR:
			continue
		case err == syscall.ESRCH:
			return true
		case err != nil || n != 1:
			return false
		case got[0].Flags&syscall.EV_ERROR != 0:
			return syscall.Errno(got[0].Data) == syscall.ESRCH
		}
		return got[0].Fflags&syscall.NOTE_EXIT != 0
	}
}
