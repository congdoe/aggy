package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/congdoe/aggy/internal/patch"
)

// Vendor names a vendor CLI.
type Vendor string

// The vendors aggy supports.
const (
	Claude Vendor = "claude"
	Codex  Vendor = "codex"
	Grok   Vendor = "grok"
)

// Vendors returns every supported vendor in a stable order.
func Vendors() []Vendor { return []Vendor{Claude, Codex, Grok} }

// ParseVendor returns the vendor named s, or an error naming the valid ones.
func ParseVendor(s string) (Vendor, error) {
	for _, v := range Vendors() {
		if string(v) == s {
			return v, nil
		}
	}
	return "", fmt.Errorf("unknown vendor %q (want claude, codex, or grok)", s)
}

// Kind is what a borrow is for. The on-screen agent picks the kind; aggy
// picks the vendor.
type Kind string

// The borrow kinds.
const (
	Image    Kind = "image"
	Video    Kind = "video"
	Research Kind = "research"
	News     Kind = "news"
	Review   Kind = "review"
	Edit     Kind = "edit"
)

// Kinds returns every borrow kind in a stable order.
func Kinds() []Kind { return []Kind{Image, Video, Research, News, Review, Edit} }

// ParseKind returns the kind named s, or an error naming the valid ones.
func ParseKind(s string) (Kind, error) {
	for _, k := range Kinds() {
		if string(k) == s {
			return k, nil
		}
	}
	return "", fmt.Errorf("unknown kind %q (want image, video, research, news, review, or edit)", s)
}

// ReadOnly reports whether a borrow of this kind must never write the repo.
// Only edit writes, and only in its own worktree.
func (k Kind) ReadOnly() bool { return k != Edit }

// Command is a process for the supervisor or borrow runner to start. It is a
// description only; adapters never start it themselves.
type Command struct {
	// Path is the program, usually the bare vendor binary name ("codex"),
	// which the runner resolves on PATH.
	Path string
	// Args excludes the program name.
	Args []string
	// Env holds KEY=value entries added to aggy's environment. Adapters
	// never add *_BASE_URL or API-key variables.
	Env []string
	// Dir is the working directory.
	Dir string
	// Stdin says what the process reads. Codex exec waits on a non-terminal
	// stdin until EOF, so headless Codex never gets an open pipe.
	Stdin Stdin
	// StdinPath is the file read as stdin when Stdin is StdinFile: the
	// headless prompt file, so the prompt never goes in argv.
	//
	// TODO(T6621): the borrow runner opens StdinPath through safefs with
	// O_NOFOLLOW and O_NONBLOCK, refuses anything but a regular file (a
	// FIFO or device could block or feed the CLI forever), and only then
	// clears O_NONBLOCK and hands the descriptor to the child.
	StdinPath string
}

// Stdin is the standard input of a Command.
type Stdin int

// Standard input modes.
const (
	StdinNull     Stdin = iota // /dev/null (the zero value)
	StdinTerminal              // the pseudo-terminal, for interactive sessions
	StdinFile                  // the regular file at Command.StdinPath, read to EOF
)

// Output is what a Runner returns for a finished Command.
type Output struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner runs a Command and returns its output. The caller supplies it, so
// adapters stay free of process handling and tests can fake it.
type Runner func(ctx context.Context, cmd Command) (Output, error)

// Probe is what aggy records about an installed CLI: connection status
// only. It never holds tokens or credentials.
type Probe struct {
	Vendor       Vendor
	Path         string    // resolved binary path, empty when not installed
	Version      string    // as the CLI reports it
	Connected    bool      // the CLI reports itself signed in
	Capabilities []string  // feature names this version supports, e.g. "headless-json"
	ProbedAt     time.Time // when the probe ran
}

// SessionRef identifies one vendor session.
type SessionRef struct {
	ID  string // the vendor's conversation id
	Cwd string // the directory the session ran in
}

// InteractiveRequest asks for an on-screen session in a pseudo-terminal.
type InteractiveRequest struct {
	Cwd string
	// Model is passed through unchanged. Empty means the vendor's default.
	Model string
	// SessionID pre-assigns a new session's id where the CLI supports it
	// (Claude and Grok). Empty lets the CLI choose.
	SessionID string
	// Resume reopens this vendor session id instead of starting one.
	Resume string
	// Fork branches the resumed session instead of reusing its id.
	Fork bool
	// BriefFile is a file the new session is told to read. Briefs are never
	// passed inline.
	BriefFile string
	// Env holds extra KEY=value entries, such as AGGY_VENDOR and AGGY_MODEL.
	Env []string
	// Memory is the text the session gets at start, framed and cleaned
	// by internal/parity: Codex takes it with -c developer_instructions
	// and Grok with --rules. Empty means none.
	Memory string
	// MemoryFile is an absolute path holding Memory, for Claude's
	// --append-system-prompt-file. Empty means none.
	MemoryFile string
}

// HeadlessRequest asks for one bounded borrow run.
type HeadlessRequest struct {
	Kind Kind
	// PromptFile holds the task. It is a path, never inline text: macOS
	// ARG_MAX is 1MB including the environment.
	PromptFile string
	// Model is passed through unchanged. Empty means the vendor's default.
	Model string
	// Cwd is the working directory: an empty temp dir for research and news,
	// a worktree for edit.
	Cwd string
	// MaxTurns caps agent turns where the CLI supports it. Zero means no cap.
	MaxTurns int
	// AllowedTools lists the vendor tool names the child may use. Nil means
	// none beyond what the kind requires: tools are off by default.
	AllowedTools []string
	// WebSearch turns the vendor's web search on.
	WebSearch bool
	// Resume continues this vendor session id, as for image --edit.
	Resume string
	// SessionID pre-assigns a new session's id where the CLI supports it
	// (Claude and Grok). Empty lets the CLI choose.
	SessionID string
	// RefImages are reference images passed to the CLI where it takes them.
	RefImages []string
	// FileTools allows the media tools that read a local file at any path
	// the model names and upload it to the vendor (Grok's image_edit,
	// image_to_video, and reference_to_video). Off unless the user opts in
	// (user config [media] grok_file_tools); without it a request that
	// needs one fails with ErrFileTools.
	FileTools bool
	// Env holds extra KEY=value entries, such as AGGY_DELEGATED=1.
	Env []string
	// IsolatedCwd says Cwd is a fresh, empty temp dir made for this run.
	// Adapters cannot check that without touching the disk, so the caller
	// vouches for it: the borrow runner creates one for every kind but
	// review and sets this. Research and news refuse to build without it.
	IsolatedCwd bool
	// RunDir is a private dir the borrow runner made for this run, outside
	// Cwd. It holds the files an adapter asks for through ChildFiler (such
	// as Claude's empty MCP config), which the runner writes before
	// starting the command. Adapters that need such files refuse to build
	// without it.
	RunDir string
	// GitRepo says Cwd is inside a git work tree. Codex is then not given
	// --skip-git-repo-check.
	GitRepo bool
	// GuardCommand is the PreToolUse guard to wire into the child where
	// the CLI takes a per-run hook: an absolute program path and its
	// arguments (aggy hook pretooluse). Empty means none.
	GuardCommand []string
}

// ChildFile is a per-run file an adapter needs in HeadlessRequest.RunDir.
// It is not vendor config: it lives only in the run's temp dir, which the
// borrow runner removes afterwards, so it does not go through the patch
// manifest.
type ChildFile struct {
	Name string // a base name, no separators
	Data []byte
}

// ChildFiler is implemented by adapters whose headless commands read
// per-run files. The borrow runner writes every returned file into
// RunDir, through safefs, before it runs the command Headless built.
type ChildFiler interface {
	ChildFiles(req HeadlessRequest) ([]ChildFile, error)
}

// ErrorClass is aggy's classification of a failed run. Adapters map each
// vendor's own error values onto these.
type ErrorClass string

// Error classes. The empty class means success.
const (
	ErrNone           ErrorClass = ""
	ErrRateLimit      ErrorClass = "rate_limit" // plan limit or rate limit: switch or reroute
	ErrAuth           ErrorClass = "auth"       // not signed in; the user runs the vendor's login
	ErrBilling        ErrorClass = "billing"
	ErrServer         ErrorClass = "server" // vendor outage or overload
	ErrInvalidRequest ErrorClass = "invalid_request"
	ErrMaxTurns       ErrorClass = "max_turns"
	ErrTimeout        ErrorClass = "timeout" // set by the runner when it kills a run
	// ErrLockdown: the CLI rejected or ignored a lockdown flag (tool
	// allowlist, MCP lockout, sandbox). The run is a failure, never
	// retried without the flag.
	ErrLockdown ErrorClass = "lockdown"
	ErrUnknown  ErrorClass = "unknown"
)

// Artifact is a file a run produced, such as a generated image.
type Artifact struct {
	Path string // absolute path in the vendor's own store
	Kind string // "image" or "video"
}

// HeadlessResult is a parsed headless run. Text and ErrorDetail are raw
// vendor output, which a model or a web page may have shaped: callers must
// pass them through termsafe before printing them to a terminal (M1).
type HeadlessResult struct {
	SessionID   string // the vendor's conversation id, for resume
	Text        string // the final reply
	Artifacts   []Artifact
	Error       ErrorClass
	ErrorDetail string // vendor's error message, for logs
	Tokens      int64  // total tokens where the CLI reports them, else 0
}

// Turn is one message from a vendor transcript.
type Turn struct {
	Role string // "user" or "assistant"
	Text string
	Time time.Time
}

// Transcript is the tail of a vendor session's transcript.
type Transcript struct {
	Path  string // the full transcript, which a brief points to
	Turns []Turn // oldest first
}

// ErrNoTranscript means the adapter cannot read this vendor's transcript,
// for example after a format change. Callers degrade: the brief then uses
// the task file alone.
var ErrNoTranscript = errors.New("transcript not available")

// ErrUnsupported matches a request this vendor's CLI cannot serve with
// the flags recorded in docs/adapters.md, such as a pre-assigned session
// id on Codex or video on Codex. Callers route elsewhere.
var ErrUnsupported = errors.New("not supported by this CLI")

// ErrFileTools matches a request that needs a media tool reading local
// files at any path (FileTools) when it is off. It matches ErrUnsupported
// too, so the borrow passes the vendor over.
var ErrFileTools = fmt.Errorf("needs a media tool that uploads any local image a path names, which is off (%w)", ErrUnsupported)

// ErrUnparsed matches a headless run whose output the adapter could not
// read. Its HeadlessResult carries ErrUnknown.
var ErrUnparsed = errors.New("unrecognized headless output")

// ConfigContext tells ConfigPatches where to write.
type ConfigContext struct {
	Home string // the user's home directory
	Repo string // the repo root, empty for user-level patches only
}

// Adapter is one vendor CLI. Implementations live in adapter/<vendor>.
type Adapter interface {
	// Vendor names this adapter's vendor.
	Vendor() Vendor

	// TestedVersions is the range of CLI versions this adapter was tested
	// on. VersionWarning compares a probed version with it.
	TestedVersions() VersionRange

	// Probe checks the installed CLI through run: path, version, and
	// capabilities. It never reads credential files. Sign-in status is
	// checked separately (SignIn, CheckSignIn), so Connected stays false.
	Probe(ctx context.Context, run Runner) (Probe, error)

	// SignIn returns the vendor's own sign-in step and, when one is
	// verified, a sign-in status check.
	SignIn() SignIn

	// Interactive builds the command for an on-screen session.
	Interactive(req InteractiveRequest) (Command, error)

	// Headless builds the command for one borrow run.
	Headless(req HeadlessRequest) (Command, error)

	// ParseHeadless reads the output of a finished Headless command.
	ParseHeadless(req HeadlessRequest, out Output) (HeadlessResult, error)

	// Artifacts lists the files a headless run produced, from the vendor's
	// own store. It returns every file for the session, not only the one the
	// reply names.
	Artifacts(req HeadlessRequest, res HeadlessResult) ([]Artifact, error)

	// TranscriptTail returns up to n of the latest turns of a session, or
	// ErrNoTranscript.
	TranscriptTail(ref SessionRef, n int) (Transcript, error)

	// ConfigPatches returns the parity changes for this vendor's config.
	// Each patch is guarded by the file's current hash.
	ConfigPatches(cc ConfigContext) ([]patch.Patch, error)
}
