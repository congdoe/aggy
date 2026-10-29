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
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/safefs"
)

// maxSockPath is the longest socket path every supported OS takes
// (darwin's sun_path holds 104 bytes with the NUL).
const maxSockPath = 103

// connIdle is how long a connection may sit without a full request.
const connIdle = 10 * time.Second

// rootWait bounds how long a request waits for SetRoot, so a hook that
// races the CLI's start is still checked (aggy hook gives up at 3 s).
// Tests shorten it.
var rootWait = 2 * time.Second

// peerCred returns the uid and pid of the process at the other end of c.
// Tests replace it before Listen, which keeps the value it finds.
var peerCred = peerCredOf

// Server is the supervisor socket, run/<parent-id>.sock in the state
// directory. Only processes of aggy's own uid get an answer.
type Server struct {
	ln   *net.UnixListener
	path string
	lock *os.File // flocked while the server runs; marks the socket live
	peer func(*net.UnixConn) (uid, pid int, err error)
	// root is the on-screen CLI's pid (SetRoot), the top of the lineage;
	// rootSet closes once it is known, done once Close runs.
	root     int
	rootSet  chan struct{}
	rootOnce sync.Once
	done     chan struct{}
	// onPanic restores the terminal before a panic in a server goroutine
	// ends the process.
	onPanic func()
	onHook  func(Request) // gets each valid hook-event; may be nil
	// onSwitch takes a switch-request (SetSwitch); nil refuses them.
	// Guarded by mu.
	onSwitch func(Request) error
	// onTools takes a tools-changed notice (SetTools); nil refuses them.
	// Guarded by mu.
	onTools func()
	// log gets a record of each message refused or not acted on
	// (SetLog); it must not block. Guarded by mu.
	log    func(diag.Rec)
	wg     sync.WaitGroup
	once   sync.Once
	mu     sync.Mutex
	conns  map[*net.UnixConn]struct{} // open connections, closed by Close
	closed bool
}

// Path returns the socket's path, for AGGY_SOCKET.
func (s *Server) Path() string { return s.path }

// Listen creates runDir (0700) if needed and listens on
// runDir/<parentID>.sock (0600). parentID must be a UUID. onPanic, if
// set, runs before a panic in a server goroutine ends the process.
// onHook, if set, gets each valid hook-event, on the connection's
// goroutine: it must be quick and safe for concurrent use.
//
// A socket left by a supervisor that died is removed first. Liveness is
// an flock on runDir/<parentID>.lock, held while the server runs: the
// kernel drops it when its holder dies, so unlike a pid it cannot be
// fooled by pid reuse. If another live supervisor holds it, Listen fails.
// Anything at the socket path that is not a socket is left alone and
// Listen fails.
func Listen(runDir, parentID string, onPanic func(), onHook func(Request)) (*Server, error) {
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
	s := &Server{ln: ln, path: path, lock: lock, peer: peerCred, onPanic: onPanic, onHook: onHook,
		conns: map[*net.UnixConn]struct{}{}, rootSet: make(chan struct{}), done: make(chan struct{})}
	s.goSafe(s.accept)
	return s, nil
}

// SetRoot records the on-screen CLI's pid once it has started: a request
// is FromSession when its sender descends from it (see lineage.go). Only
// the first call counts.
func (s *Server) SetRoot(pid int) {
	s.rootOnce.Do(func() {
		s.root = pid
		close(s.rootSet)
	})
}

// SetTools sets what takes a tools-changed notice, from any peer of this
// uid. It runs on the connection's goroutine and must not block.
func (s *Server) SetTools(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onTools = f
}

// SetSwitch sets what takes a switch-request from the session: it
// returns nil when the switch is queued, else why not. It runs on the
// connection's goroutine and must be quick and safe for concurrent use.
func (s *Server) SetSwitch(f func(Request) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onSwitch = f
}

// SetLog sets what gets a record of each message the server refuses or
// does not act on (docs/logging.md). It must never block: a hook waits
// for its answer, and the CLI for its hook.
func (s *Server) SetLog(f func(diag.Rec)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = f
}

// logger is the server's log, or one that drops records.
func (s *Server) logger() func(diag.Rec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		return func(diag.Rec) {}
	}
	return s.log
}

// fromSession reports whether the process that opened c, at pid, is in
// the CLI's tree (lineage.go): it pins that process, waits up to
// rootWait for SetRoot, walks pid's parents, and checks the pinned
// process still has pid.
func (s *Server) fromSession(c *net.UnixConn, pid int) bool {
	same, release, err := peerInstance(c, pid)
	if err != nil {
		return false
	}
	defer release()
	select {
	case <-s.rootSet:
	case <-s.done:
		return false
	case <-time.After(rootWait):
		return false
	}
	return descends(pid, s.root) && same()
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

// PrivateDir makes dir (0700) if needed and checks that it is a real
// directory this user owns, as Listen does for the run dir.
func PrivateDir(dir string) error { return privateDir(dir) }

// Live reports whether the supervisor of session parentID is running:
// whether something holds the flock on runDir/<parentID>.lock. A missing
// lock file is not live. The check takes a shared lock for an instant;
// call it only for a session whose supervisor could not be starting.
func Live(runDir, parentID string) (bool, error) {
	f, err := safefs.OpenFile(filepath.Join(runDir, parentID+".lock"), os.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	switch {
	case errors.Is(err, syscall.EWOULDBLOCK):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// takeLock opens path and takes an exclusive flock on it without waiting.
//
// Close unlinks the lock file while holding it, so a file opened just
// before that unlink can be locked once its holder lets go, while another
// process creates and locks a new file at path: two holders of "the"
// lock (B13-7). So after locking, path must still name the locked file;
// if it does not, the lock is on an unlinked file and is taken again.
func takeLock(path string) (*os.File, error) {
	for range 3 {
		f, err := lockOnce(path)
		if err != nil {
			return nil, err
		}
		if sameFile(f, path) {
			return f, nil
		}
		_ = f.Close()
	}
	return nil, fmt.Errorf("supervisor: lock %s keeps changing", path)
}

// lockOpened, when set by a test, runs between the open and the flock.
var lockOpened func()

func lockOnce(path string) (*os.File, error) {
	f, err := safefs.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if lockOpened != nil {
		lockOpened()
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

// sameFile reports whether path still names the open file f.
func sameFile(f *os.File, path string) bool {
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := safefs.Lstat(path)
	return err == nil && os.SameFile(a, b)
}

// goSafe runs f in a goroutine Close waits for, which runs onPanic
// before a panic ends the process.
func (s *Server) goSafe(f func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				if s.onPanic != nil {
					s.onPanic()
				}
				panic(r)
			}
		}()
		f()
	}()
}

// track records an open connection; it reports false once Close has run.
func (s *Server) track(c *net.UnixConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c *net.UnixConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}

// Close stops the server, closes every open connection (a client that
// stays connected must not keep aggy from exiting), waits for their
// goroutines, and removes the socket and the lock file.
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		err = s.ln.Close()
		close(s.done)
		s.mu.Lock()
		s.closed = true
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
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
	for {
		c, err := s.ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if !s.track(c) {
			_ = c.Close()
			return
		}
		s.goSafe(func() {
			defer s.untrack(c)
			s.serve(c)
		})
	}
}

// serve answers c's requests until it closes, idles, or errs. A peer of
// another uid is closed at once, unanswered. Whether the peer is in the
// CLI's lineage is worked out once, now, before any request is read, and
// a peer that is not is closed after its first request (lineage.go).
func (s *Server) serve(c *net.UnixConn) {
	defer func() { _ = c.Close() }()
	uid, peerPID, err := s.peer(c)
	switch {
	case err != nil:
		// Not a security refusal: a peer that connected and left at once
		// cannot be told either (G67-8).
		s.logger()(diag.Rec{Event: diag.EvHookRejected, Code: diag.CodePeerUnknown, Msg: "a connection whose peer could not be told"})
		return
	case uid != os.Getuid():
		s.logger()(diag.Rec{Event: diag.EvHookRejected, Code: diag.CodePeerUID, Msg: "a connection from another user", Attrs: map[string]any{"uid": uid}})
		return
	}
	from := s.fromSession(c, peerPID)
	r := bufio.NewReaderSize(c, MaxMessage+1)
	for {
		_ = c.SetDeadline(time.Now().Add(connIdle))
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			s.logger()(diag.Rec{Event: diag.EvHookRejected, Code: diag.CodeBadRequest, Msg: "a message over the size limit", Attrs: map[string]any{"from_session": from}})
			reply(c, Response{Error: fmt.Sprintf("request longer than %d bytes", MaxMessage), Code: diag.CodeBadRequest.String()})
			return
		}
		if err != nil {
			return // closed, idle, or a last line without a newline
		}
		s.mu.Lock()
		onSwitch, onTools, log := s.onSwitch, s.onTools, s.log
		s.mu.Unlock()
		resp := answer(line[:len(line)-1], from, s.onHook, onSwitch, onTools, log)
		reply(c, resp)
		if !resp.OK || !from {
			return
		}
	}
}

func reply(c *net.UnixConn, r Response) {
	b, _ := json.Marshal(r)
	_, _ = c.Write(append(b, '\n'))
}
