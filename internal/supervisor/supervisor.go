package supervisor

import (
	"io"
	"os"
	"syscall"

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
}
