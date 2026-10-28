package supervisor

import (
	"io"
	"os"
	"syscall"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// Child is a vendor CLI running in a pseudo-terminal. Read and Write
// reach the terminal's master side. internal/execrun's PTY implements it;
// the supervisor never starts processes itself, the caller's Start does.
type Child interface {
	io.ReadWriteCloser
	// Resize sets the terminal size, which sends the CLI SIGWINCH.
	Resize(rows, cols int) error
	// Wait blocks until the CLI stops (stopped true) or exits (its code,
	// or 128 plus the signal number).
	Wait() (stopped bool, code int, err error)
	// Continue resumes a stopped CLI.
	Continue() error
	// Signal sends sig to the CLI's process group.
	Signal(sig syscall.Signal) error
	// Pid is the CLI's process id, the top of the socket's lineage check.
	Pid() int
}

// Options configure one supervised session.
type Options struct {
	// In and Out are the user's terminal, normally os.Stdin and os.Stdout.
	// In must be a terminal.
	In, Out *os.File
	// Command is the interactive CLI to run (adapter.Adapter.Interactive),
	// with an absolute Path.
	Command adapter.Command
	// Start starts Command in a new pseudo-terminal of rows by cols.
	Start func(c adapter.Command, rows, cols int) (Child, error)
	// SwitchKey is the control byte that opens the switch picker
	// (config.ParseKey); zero means Ctrl+\ (0x1c).
	SwitchKey byte
	// Vendors are the picker's choices, in switch.order order, without
	// the vendor on screen.
	Vendors []adapter.Vendor
	// DoublePress is how soon a second switch key must follow the first
	// to be sent to the CLI instead of opening the picker. Zero means
	// DefaultDoublePress.
	DoublePress time.Duration
	// RunDir is the state directory's run/ folder, where the supervisor
	// socket lives. Empty means no socket (tests of the pass-through).
	RunDir string
	// ParentID is the parent session's id, a UUID: it names the socket
	// and reaches the CLI as AGGY_PARENT.
	ParentID string
	// OnHook gets each hook event the socket accepts (see Listen).
	OnHook func(Request)
}

// SocketEnv is env plus the variables that lead hooks and skills inside
// the CLI to the socket: AGGY_SOCKET and AGGY_PARENT.
func SocketEnv(env []string, socket, parentID string) []string {
	return append(append([]string(nil), env...), "AGGY_SOCKET="+socket, "AGGY_PARENT="+parentID)
}

// DefaultDoublePress is the default Options.DoublePress.
const DefaultDoublePress = 400 * time.Millisecond
