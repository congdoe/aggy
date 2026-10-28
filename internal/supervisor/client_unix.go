//go:build unix

package supervisor

// This is the socket's client, used by `aggy hook <event>`. Besides the
// server's files, it is the only aggy code allowed to import net, and it
// may call only net.DialUnix (see .golangci.yml), on a path it has
// checked is aggy's own socket.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
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
	out, err := bufio.NewReaderSize(c, MaxMessage+1).ReadSlice('\n')
	if err != nil {
		return Response{}, fmt.Errorf("supervisor: no reply: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(out, &resp); err != nil {
		return Response{}, fmt.Errorf("supervisor: bad reply: %w", err)
	}
	return resp, nil
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
