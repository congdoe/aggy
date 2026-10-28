//go:build unix

package supervisor

// This file and the peercred files are the only aggy code allowed to
// import net (see .golangci.yml): a Unix socket needs it. It listens on a
// path in a 0700 directory and never dials or opens anything but that
// socket.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// maxSockPath is the longest socket path every supported OS takes
// (darwin's sun_path holds 104 bytes with the NUL).
const maxSockPath = 103

// connIdle is how long a connection may sit without a full request.
const connIdle = 10 * time.Second

// peerUID returns the uid of the process at the other end of c. Tests
// replace it before Listen, which keeps the value it finds.
var peerUID = peerUIDOf

// Server is the supervisor socket, run/<parent-id>.sock in the state
// directory. Only processes of aggy's own uid get an answer.
type Server struct {
	ln   *net.UnixListener
	path string
	lock *os.File // flocked while the server runs; marks the socket live
	peer func(*net.UnixConn) (int, error)
	wg   sync.WaitGroup
	once sync.Once
}

// Path returns the socket's path, for AGGY_SOCKET.
func (s *Server) Path() string { return s.path }

// Listen creates runDir (0700) if needed and listens on
// runDir/<parentID>.sock (0600). parentID must be a UUID.
//
// A socket left by a supervisor that died is removed first. Liveness is
// an flock on runDir/<parentID>.lock, held while the server runs: the
// kernel drops it when its holder dies, so unlike a pid it cannot be
// fooled by pid reuse. If another live supervisor holds it, Listen fails.
// Anything at the socket path that is not a socket is left alone and
// Listen fails.
func Listen(runDir, parentID string) (*Server, error) {
	if !adapter.IsUUID(parentID) {
		return nil, fmt.Errorf("supervisor: parent id %q is not a UUID", parentID)
	}
	if err := privateDir(runDir); err != nil {
		return nil, err
	}
	path := filepath.Join(runDir, parentID+".sock")
	if len(path) > maxSockPath {
		return nil, fmt.Errorf("supervisor: socket path %s is longer than %d bytes", path, maxSockPath)
	}
	lock, err := takeLock(filepath.Join(runDir, parentID+".lock"))
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Server, error) {
		_ = safefs.Remove(lock.Name())
		_ = lock.Close()
		return nil, err
	}
	if fi, err := safefs.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return fail(fmt.Errorf("supervisor: %s exists and is not a socket", path))
		}
		// We hold the lock, so the socket's supervisor is gone.
		if err := safefs.Remove(path); err != nil {
			return fail(err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fail(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return fail(err)
	}
	ln.SetUnlinkOnClose(false) // Close removes it through safefs
	// The directory is 0700, so no other uid could reach the socket in
	// the moment before this; safefs.Chmod opens the file, which a socket
	// cannot be.
	if err := os.Chmod(path, 0o600); err != nil { //nolint:forbidigo // a socket cannot be opened, so safefs.Chmod cannot set its mode; the path is in our 0700 dir
		_ = ln.Close()
		_ = safefs.Remove(path)
		return fail(err)
	}
	s := &Server{ln: ln, path: path, lock: lock, peer: peerUID}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// privateDir makes dir a 0700 directory owned by aggy's uid: it creates
// it, or tightens an existing one's mode, and refuses a symlink or a
// directory owned by anyone else.
func privateDir(dir string) error {
	if err := safefs.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := safefs.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("supervisor: %s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("supervisor: %s is not owned by this user", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		return safefs.Chmod(dir, 0o700)
	}
	return nil
}

// takeLock opens path and takes an exclusive flock on it without waiting.
func takeLock(path string) (*os.File, error) {
	f, err := safefs.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("supervisor: another aggy is running this session (%s is locked)", path)
		}
		return nil, fmt.Errorf("supervisor: lock %s: %w", path, err)
	}
	return f, nil
}

// Close stops the server, waits for its connections, and removes the
// socket and the lock file.
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		err = s.ln.Close()
		s.wg.Wait()
		if rerr := safefs.Remove(s.path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) && err == nil {
			err = rerr
		}
		// Remove the lock file while still holding the lock, so a new
		// supervisor never locks a file that is about to vanish.
		_ = safefs.Remove(s.lock.Name())
		_ = s.lock.Close()
	})
	return err
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(c)
		}()
	}
}

// serve answers c's requests until it closes, idles, or errs. A peer of
// another uid is closed at once, unanswered.
func (s *Server) serve(c *net.UnixConn) {
	defer func() { _ = c.Close() }()
	if uid, err := s.peer(c); err != nil || uid != os.Getuid() {
		return
	}
	r := bufio.NewReaderSize(c, MaxMessage+1)
	for {
		_ = c.SetDeadline(time.Now().Add(connIdle))
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			reply(c, Response{Error: fmt.Sprintf("request longer than %d bytes", MaxMessage)})
			return
		}
		if err != nil {
			return // closed, idle, or a last line without a newline
		}
		resp := answer(line[:len(line)-1])
		reply(c, resp)
		if !resp.OK {
			return
		}
	}
}

func reply(c *net.UnixConn, r Response) {
	b, _ := json.Marshal(r)
	_, _ = c.Write(append(b, '\n'))
}
