//go:build unix

package supervisor

// This is the socket's client, used by `aggy hook <event>`. Besides the
// server's files, it is the only aggy code allowed to import net, and it
// may call only net.DialUnix (see .golangci.yml), on a path it has
// checked is aggy's own socket.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// Send delivers req to the supervisor socket at path and returns the
// reply, within timeout (the connect itself is not bounded; a caller
// that must not hang runs Send under its own deadline).
//
// path must be runDir/<uuid>.sock, a socket owned by this uid, in a 0700
// directory owned by this uid: a hook never hands its payload to a
// socket another user could have planted, whatever AGGY_SOCKET says. req
// must be a request the server would accept.
func Send(runDir, path string, req Request, timeout time.Duration) (Response, error) {
	return send(runDir, path, req, timeout, false)
}

// ErrUnproven is a hermes-pin answer that came without the proof that its
// sender holds the supervisor's lock for the socket's parent id.
var ErrUnproven = errors.New("supervisor: the socket's peer did not show it holds the supervisor's lock")

// SendHermesPin is Send for a hermes-pin (T6954). The index row names the
// socket, and anyone of this uid can bind a socket in the run directory, so
// an answer that is OK counts only when it carries the proof replyWithLock
// attaches: one open file, the lock file runDir/<parent-id>.lock by
// identity, on which an exclusive flock succeeds at once. While the real
// supervisor holds that lock, only its own open file passes; any other open
// of the same file fails the flock. Without the proof the answer is
// ErrUnproven, which the hook treats as a supervisor it cannot reach: it
// asks. A refusal needs no proof, since it loosens nothing.
//
// What it does not stop: a process of this uid that takes the lock itself
// (for a parent id of its own, or after removing the lock file) and points
// the row at it, or one that relays a real supervisor's answer and its open
// file. It closes the plain path of binding a socket and editing a row.
func SendHermesPin(runDir, path string, req Request, timeout time.Duration) (Response, error) {
	return send(runDir, path, req, timeout, true)
}

func send(runDir, path string, req Request, timeout time.Duration, proof bool) (Response, error) {
	if err := checkSocket(runDir, path); err != nil {
		return Response{}, err
	}
	line, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	if len(line) > MaxMessage {
		return Response{}, fmt.Errorf("supervisor: request of %d bytes is over the %d-byte limit", len(line), MaxMessage)
	}
	if _, err := decodeRequest(line); err != nil {
		return Response{}, err
	}
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
		return Response{}, err
	}
	if _, err := c.Write(append(line, '\n')); err != nil {
		return Response{}, err
	}
	var out []byte
	var fds []int
	if proof {
		out, fds, err = readWithRights(c)
		defer closeFDs(fds)
	} else {
		out, err = bufio.NewReaderSize(c, MaxMessage+1).ReadSlice('\n')
	}
	if err != nil {
		return Response{}, fmt.Errorf("supervisor: no reply: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(out, &resp); err != nil {
		return Response{}, fmt.Errorf("supervisor: bad reply: %w", err)
	}
	if proof && resp.OK {
		id := strings.TrimSuffix(filepath.Base(path), ".sock")
		if err := holdsLock(filepath.Join(runDir, id+".lock"), fds); err != nil {
			return Response{}, err
		}
	}
	return resp, nil
}

// maxRights is how many open files a reply may carry before the rest are
// cut off; a proof is exactly one.
const maxRights = 4

// readWithRights reads one reply line from c with the open files sent along
// with it (SCM_RIGHTS), which the caller closes.
func readWithRights(c *net.UnixConn) (line []byte, fds []int, err error) {
	buf := make([]byte, MaxMessage+1)
	oob := make([]byte, syscall.CmsgSpace(4*maxRights))
	n := 0
	for {
		k, oobn, _, _, rerr := c.ReadMsgUnix(buf[n:], oob)
		if oobn > 0 {
			msgs, perr := syscall.ParseSocketControlMessage(oob[:oobn])
			if perr == nil {
				for i := range msgs {
					if r, err := syscall.ParseUnixRights(&msgs[i]); err == nil {
						fds = append(fds, r...)
					}
				}
			}
		}
		n += k
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			return buf[:i+1], fds, nil
		}
		switch {
		case rerr != nil:
			return nil, fds, rerr
		case n == len(buf):
			return nil, fds, fmt.Errorf("reply longer than %d bytes", MaxMessage)
		case k == 0:
			return nil, fds, io.ErrUnexpectedEOF
		}
	}
}

func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
}

// holdsLock reports whether fds is the proof that its sender holds the
// lock at lockPath: exactly one open file, which is that lock file (by
// device and inode) and on which an exclusive flock succeeds without
// waiting. flock locks belong to an open file, not a process, so while the
// supervisor holds its lock no other open of the file can take it, and on
// the supervisor's own open file the flock is a no-op. It only reads the
// file's metadata.
func holdsLock(lockPath string, fds []int) error {
	if len(fds) != 1 {
		return ErrUnproven
	}
	fi, err := safefs.Lstat(lockPath)
	if err != nil || !fi.Mode().IsRegular() {
		return ErrUnproven
	}
	want, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ErrUnproven
	}
	var got syscall.Stat_t
	if err := syscall.Fstat(fds[0], &got); err != nil || got.Dev != want.Dev || got.Ino != want.Ino {
		return ErrUnproven
	}
	for {
		err = syscall.Flock(fds[0], syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return ErrUnproven
	}
	return nil
}

// checkSocket refuses a path that is not runDir/<uuid>.sock, a socket of
// this uid in a 0700 directory of this uid.
func checkSocket(runDir, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) != filepath.Clean(runDir) {
		return fmt.Errorf("supervisor: %s is not in aggy's run directory", path)
	}
	if id, ok := strings.CutSuffix(filepath.Base(path), ".sock"); !ok || !adapter.IsUUID(id) {
		return fmt.Errorf("supervisor: %s is not a supervisor socket name", path)
	}
	mine := func(fi fs.FileInfo) bool {
		st, ok := fi.Sys().(*syscall.Stat_t)
		return ok && int(st.Uid) == os.Getuid()
	}
	dir, err := safefs.Lstat(runDir)
	if err != nil {
		return err
	}
	if !dir.IsDir() || dir.Mode().Perm() != 0o700 || !mine(dir) {
		return fmt.Errorf("supervisor: %s is not a private directory of this user", runDir)
	}
	fi, err := safefs.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Type() != fs.ModeSocket || !mine(fi) {
		return errors.New("supervisor: the socket is not a socket of this user")
	}
	return nil
}
