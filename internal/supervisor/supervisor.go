package supervisor

import (
	"errors"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
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
	// Vendor is the vendor on screen: its turn-end hooks gate a switch,
	// and its exit command ends it (adapter.ExitCommand).
	Vendor adapter.Vendor
	// CanSwitch says why the session may not switch to v now (the repo's
	// allowed_vendors, the CLI not on PATH), or nil. It is asked when a
	// switch is picked or requested and again before the exit command is
	// typed. Nil means the session cannot switch.
	CanSwitch func(v adapter.Vendor) error
	// StartsTurn is set when the CLI starts with a turn running (a brief
	// prompt), so the session is not at its prompt until a turn end.
	StartsTurn bool
	// SessionID returns the on-screen child's vendor session id, or ""
	// while it is not known (a new Codex child before the index confirms
	// it). Only a turn-end hook whose payload names this id counts (S-9):
	// a nested CLI in the session's tree has its own. Nil counts none.
	SessionID func() string
	// Children lists the open parent's interactive children of v, latest
	// first, for the picker (T6665): with more than one, picking v asks
	// which. Nil lists none (the latest is resumed).
	Children func(v adapter.Vendor) []ChildChoice
	// HasChild says whether the child id picked for v is still recorded,
	// read fresh: Children may be stale. It is called off the main loop
	// once a switch to a picked child is confirmed, before the exit
	// command is typed (T6770). Nil skips the check.
	HasChild func(v adapter.Vendor, id string) bool
	// Limits gets the vendor on screen each time it reports its plan
	// limit (T6667); HasQuota says whether a vendor has none recorded. On
	// a limit, the first vendor of Vendors with quota is preselected and,
	// where the vendor on screen has a hold signal, offered at the turn
	// end with a brief. Either may be nil.
	Limits   <-chan LimitHit
	HasQuota func(v adapter.Vendor) bool
	// Used returns how much of v's limit is used, where v's CLI writes
	// what it has left on this machine; ok is false elsewhere (T6699). It
	// is called off the main loop, at each turn end of the on-screen
	// child. WarnAt is the share at which that turn end suggests a
	// handoff, once per crossing. A nil Used or a zero WarnAt never warns.
	Used   func(v adapter.Vendor) (s Share, ok bool)
	WarnAt float64
	// Diag gets the session's diagnostic records (docs/logging.md):
	// switches, the picker, limits, and the socket's hook events. It only
	// queues, so the terminal loop never waits on the disk; nil records
	// nothing. ChildID is the on-screen child's id in the index, for them.
	Diag    *diag.Queue
	ChildID string
}

// Share is how much of a vendor's limit is used: Used, 0 to 1, of the
// window named Window, which resets at Reset (zero: unknown).
type Share struct {
	Used   float64
	Window string
	Reset  time.Time
}

// LimitHit is a plan limit the session recorded for Vendor. Unverified is
// set for one only a hook reported (O56-2), which any process in the
// CLI's tree can send.
type LimitHit struct {
	Vendor     adapter.Vendor
	Unverified bool
}

// ChildChoice is one child the picker offers.
type ChildChoice struct {
	ID    string // the child's id in the index
	Label string // what the picker shows: safe for a terminal
}

// Choice is a switch as picked or requested (T6665): the vendor, and
// optionally which of its children (empty: the latest), a new child
// instead, a model for this one launch (never written to config), and a
// brief for the child.
type Choice struct {
	Vendor adapter.Vendor
	Child  string
	New    bool
	Model  string
	Brief  bool
	// Tools are turned on for the next launch only, never written to
	// the settings (switchboard.Board.Session).
	Tools []string
}

// ErrChecking is what CanSwitch returns while a check that takes time
// (the safety policy's probe) is still running: the switch may wait, but
// it is not offered yet.
var ErrChecking = errors.New("still checking that vendor")

// Exit is how a supervised session ended.
type Exit struct {
	// Code is the CLI's exit code.
	Code int
	// SwitchTo is set when the CLI exited for a switch: the vendor the
	// caller launches next; Next is the whole choice, Next.Vendor equal to
	// SwitchTo.
	SwitchTo adapter.Vendor
	Next     Choice
}

// SocketEnv is env plus the variables that lead hooks and skills inside
// the CLI to the socket: AGGY_SOCKET and AGGY_PARENT.
func SocketEnv(env []string, socket, parentID string) []string {
	return append(append([]string(nil), env...), "AGGY_SOCKET="+socket, "AGGY_PARENT="+parentID)
}

// DefaultDoublePress is the default Options.DoublePress.
const DefaultDoublePress = 400 * time.Millisecond
