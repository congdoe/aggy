package adapter

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	// OpenCode is an open harness (docs/design.md, section 15): on screen
	// only. It is not in Vendors, so it has no borrows, hooks, tools, or
	// default route, and its adapter refuses Headless.
	OpenCode Vendor = "opencode"
	// Hermes is Nous Research's Hermes Agent, an open harness like OpenCode
	// (docs/design.md, section 16): on screen only, in an aggy-owned profile.
	// It is not in Vendors either, and its adapter refuses Headless.
	Hermes Vendor = "hermes"
	// Antigravity is Google's Antigravity CLI, whose binary is agy
	// (docs/design.md, section 15, "Antigravity"): on screen only, on the
	// person's own Google sign-in, with no borrows and no print mode. Its
	// vendor name is never the binary's, one letter from aggy's own.
	Antigravity Vendor = "antigravity"
)

// Vendors returns the subscription vendors in a stable order: those with
// borrows, installed hooks, and managed tools.
func Vendors() []Vendor { return []Vendor{Claude, Codex, Grok} }

// Known returns every vendor aggy can name: Vendors, then the open
// harnesses, which are only ever on screen, and only when the person asks.
func Known() []Vendor { return append(Vendors(), OpenCode, Hermes, Antigravity) }

// OpenHarness reports a harness on screen only, opened only when the person
// names it: the open harnesses OpenCode and Hermes, and Antigravity, whose
// FAQ does not support it wrapped headless (docs/design.md, section 15).
func OpenHarness(v Vendor) bool { return v == OpenCode || v == Hermes || v == Antigravity }

// ParseVendor returns the vendor named s, or an error naming the valid ones.
func ParseVendor(s string) (Vendor, error) {
	for _, v := range Known() {
		if string(v) == s {
			return v, nil
		}
	}
	return "", fmt.Errorf("unknown vendor %q (want claude, codex, grok, opencode, hermes, or antigravity)", s)
}

// ExitCommand is the line the supervisor types into vendor v's session,
// at a turn end, to end it for a switch, and whether that is verified.
// It is "/exit" for every vendor, unverified except on OpenCode, where
// it exits 0 with nothing open (docs/verification.md, T6930), and Hermes,
// where it exits 0 at the prompt (T6933, 2026-11-11), and Antigravity, where
// /exit exits 0 and prints the resume line (agy 1.5.5, 2026-11-11); Claude's
// check is tied to the versions it ran on (ExitCommandAt). The others have no
// live check of it yet (docs/adapters.md). The supervisor asks the user
// before it types anything, verified or not, unless the person typed the
// switch in full where Ctrl+U is checked (ClearLineChecked).
func ExitCommand(v Vendor) (cmd string, verified bool) {
	return "/exit", v == OpenCode || v == Hermes || v == Antigravity
}

// ExitCommandAt is ExitCommand for the CLI's version, as the launch probed it:
// a check recorded for some versions only (liveChecks) holds at those.
func ExitCommandAt(v Vendor, version string) (cmd string, verified bool) {
	cmd, verified = ExitCommand(v)
	if c, ok := checkAt(v, version); ok && c.exit {
		verified = true
	}
	return cmd, verified
}

// localCommands are, per vendor, the commands that start no model turn. Each
// comes from the vendor's docs; a vendor with none sourced has none, which is the
// safe side: its command's Enter counts as maybe starting a turn.
//
// Claude: code.claude.com/docs/en/commands.md, "All commands", read 2026-11-07,
// which says most entries are "built-in commands whose behavior is coded into the
// CLI" and marks those that are "a prompt handed to Claude" as Skill. None of these
// is so marked, and each row describes a dialog or a display, not a model request;
// not seen live (TODO(T6895)). /compact is out: it summarizes the conversation, so
// it may run a turn. So are custom commands and skills, and any command with
// arguments.
//
// TODO(T6895): a live check settles whether a user's own skill can take one of
// these names. If it can, its Enter counts as starting no turn, atPrompt stays
// true during that turn, and a doubt note could hold its output (docs/design.md).
//
// Codex is empty on purpose as well: its SessionStart comes inside a turn, so a
// move there says nothing of a turn (session.followSession).
var localCommands = map[Vendor][]string{
	Claude: {
		"/clear",       // "Start a new conversation with empty context"
		"/model",       // "With no argument, opens a picker"
		"/usage",       // "Show session cost, plan usage limits, and activity stats"
		"/cost",        // "Alias for /usage"
		"/status",      // "Open the Settings interface on the Status tab"
		"/help",        // "Show help and available commands"
		"/config",      // "Open the Settings interface"
		"/permissions", // "Opens an interactive dialog"
		"/memory",      // "Edit CLAUDE.md files, ... and view auto memory entries"
		"/context",     // "Visualize current context usage as a colored grid"
	},
}

// LocalCommand reports whether line, whole, is a command of v's that starts no
// model turn.
func LocalCommand(v Vendor, line string) bool {
	return slices.Contains(localCommands[v], line)
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
	Vendor  Vendor
	Path    string // resolved binary path, empty when not installed
	Version string // as the CLI reports it
	// VersionLine is the first line --version printed, trimmed and capped,
	// when Version could not be parsed from it; "" otherwise.
	VersionLine  string
	Connected    bool      // the CLI reports itself signed in
	Capabilities []string  // feature names this version supports, e.g. "headless-json"
	ProbedAt     time.Time // when the probe ran
	// ValueFlags are the options the CLI's top-level help shows with a
	// required value (probe.ValueFlags), for sorting a bare word after a
	// flag aggy's table does not know (PassFlags.ParseWith).
	ValueFlags []string
	// Switches are the options the same help shows with no value
	// (probe.Switches): the only flags a "--" may follow (ParseOpts).
	Switches []string
}

// SessionRef identifies one vendor session.
type SessionRef struct {
	ID  string // the vendor's conversation id
	Cwd string // the directory the session ran in
}

// SharedRules are rules in Claude's syntax for one session.
type SharedRules struct {
	Allow, Ask, Deny []string
	// Mode is a permission mode the person set in aggy, or "".
	Mode string
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
	// ResumeName reopens the session the CLI itself finds by this name (or an
	// id it searches for), as --resume <name> (T6965): Claude and Grok only,
	// with no SessionID, Resume, or Fork. It must pass ValidResumeName.
	ResumeName string
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
	// Deny holds the safety policy's deny rules (parity.ClaudeDeny or
	// parity.GrokDeny), in the vendor's rule syntax: Claude takes them as
	// permissions.deny in an inline --settings layer, Grok as --deny
	// flags. Codex ignores them; its policy is a rules file.
	Deny []string
	// Ask holds the safety policy's rules that ask on screen
	// (parity.ClaudePolicy): Claude takes them as permissions.ask in the
	// same layer. Grok and Codex never get any. With PolicyHook set, the
	// launch has moved them there instead.
	Ask []string
	// PolicyHook, when set, is aggy's policy decision hook for Claude
	// (T6976): a PreToolUse entry that asks, allows an exact pre-approval, or
	// denies, in place of the static asks, which it keeps as Fallback. The
	// adapter puts the static asks back whenever the final layer would not
	// run the hook (claude.SettingsJSON).
	PolicyHook *PolicyHook
	// Rules are the person's shared rules for this session only
	// (internal/permissions), on top of what the CLI reads itself. Claude
	// takes them in the same inline --settings layer as Deny, Grok as --allow
	// and --deny; Codex and OpenCode do not read this. A borrow never has any.
	Rules SharedRules
	// MCP are the stdio MCP servers of the managed tools that are on,
	// added to the user's own servers for this session only. Codex takes
	// them as -c mcp_servers.<name>.* overrides. Claude and Grok take a
	// file instead: MCPConfigFile and AgentFile, which the caller renders
	// from the same list (internal/toolwire).
	MCP []MCPServer
	// MCPConfigFile is an absolute path holding the MCP servers for
	// Claude's --mcp-config (never with --strict-mcp-config: the user's
	// own servers stay). Empty means none.
	MCPConfigFile string
	// AgentFile is an absolute path holding a Grok agent definition whose
	// frontmatter lists the MCP servers, for --agent. Empty means none.
	AgentFile string
	// Hooks are the managed tools' hooks, by event, for this session
	// only: Claude takes them in its inline --settings layer, next to
	// Deny. Each command runs aggy hook-tool, never a tool directly.
	Hooks map[string][]HookGroup
	// Extra are the user's own flags for the CLI (aggy <vendor> ...), which
	// PassFlags.Parse let through. Each adapter puts them after its own
	// arguments and before a brief pointer.
	Extra []string
	// ClaudeSandbox turns Claude Code's own sandbox on for its shell
	// commands (T6980): Claude's adapter puts claude.SandboxLayer under
	// "sandbox" in the same inline --settings layer. Only an on-screen launch
	// in a folder whose trust list ticks it sets it; no other adapter reads it.
	ClaudeSandbox bool
	// UserSettings is the person's own --settings layers for Claude, read and
	// checked by the launch (T6962), which Claude merges into the inline layer
	// it passes, under aggy's: neither is dropped, and aggy's deny rules stay.
	UserSettings map[string]any
	// SettingsPath, when set, is an absolute 0600 file in aggy's run
	// directory that holds the settings layer Claude's adapter would pass
	// inline (claude.SettingsJSON), written by the launch so a person's
	// --settings file is not copied onto argv (PR #124 review).
	SettingsPath string
	// Print builds a run aggy neither supervises nor records (print mode,
	// a stdin that is no terminal): no session id, no brief, and no flag
	// that only shapes the screen (Codex's and Grok's --no-alt-screen).
	// Extra holds the CLI's own words for it, a print subcommand included.
	Print bool
	// NoSessionPersistence keeps a print run's session off the disk, so it is
	// never in the CLI's resume list or aggy's (PR #127 review). Claude only,
	// and only with Print: its --no-session-persistence ("Disable session
	// persistence so sessions are not saved to disk and cannot be resumed.
	// Print mode only", code.claude.com/docs/en/cli-reference, read
	// 2026-11-10). The launch sets it only when the probe found the flag.
	NoSessionPersistence bool
	// OpenCode is how an OpenCode session is launched; only that adapter
	// reads it, and it refuses to build a command without it.
	OpenCode *OpenCodeLaunch
	// Hermes is how a Hermes session is launched; only that adapter reads it,
	// and it refuses to build a command without it.
	Hermes *HermesLaunch
	// Antigravity is how an Antigravity session is launched; only that adapter
	// reads it, and it refuses to build a command without it.
	Antigravity *AntigravityLaunch
}

// AntigravityLaunch is what the launch made for one Antigravity session
// (docs/design.md, section 15, "Antigravity"): the per-run settings file aggy
// passes as --settings, and the decision plan its hook reads.
type AntigravityLaunch struct {
	// SettingsFile is an absolute path in the launch's private folder holding
	// aggy's permissions.deny and permissions.ask.
	SettingsFile string
	// Plan and PlanSum are the decision plan aggy's PreToolUse hook judges by
	// and its SHA-256. They reach the hook in the CLI's environment, which the
	// hook inherits (live, agy 1.5.5), as AGGY_DECIDE_PLAN and
	// AGGY_DECIDE_SHA256. Both empty is a launch with no decisions
	// (--no-policy).
	Plan, PlanSum string
}

// HermesLaunch is what the launch made for one Hermes session
// (docs/design.md, section 16): the aggy-owned profile it runs in, whose
// config.yaml carries aggy's approvals and hooks.
type HermesLaunch struct {
	// Profile is the profile's name, passed as -p.
	Profile string
}

// OpenCodeLaunch is what the launch made for one OpenCode session
// (docs/design.md, section 15). Everything else OpenCode needs from aggy
// (rules, instructions, the guard) is in ConfigFile.
type OpenCodeLaunch struct {
	// Mini runs `opencode mini`: any repo not trusted for OpenCode. A
	// repo's screen plugin runs on the full screen, with nothing to stop
	// it, and not on mini.
	Mini bool
	// Auto answers every ask with once (--auto). Only on the full screen:
	// mini has no such flag, and the launch allows it only in a trusted
	// repo.
	Auto bool
	// ConfigFile and ConfigDir are absolute paths in the folder aggy made
	// for this launch: OPENCODE_CONFIG and OPENCODE_CONFIG_DIR.
	ConfigFile, ConfigDir string
}

// HookGroup is one matcher group of a Claude-format hooks config.
type HookGroup struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []HookCommand `json:"hooks"`
}

// HookCommand is one command hook of a HookGroup.
type HookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
	// OnFailure "block" blocks the call when the hook fails (times out,
	// exits with an error other than 2, or prints bad JSON); without it
	// Claude goes on through its normal permission flow (code.claude.com/docs
	// hooks.md, read 2026-11-10; live 2026-11-10, docs/verification.md).
	OnFailure string `json:"onFailure,omitempty"`
}

// PolicyHook is aggy's policy decision hook on a Claude launch.
type PolicyHook struct {
	// Group is the PreToolUse matcher group that runs it.
	Group HookGroup
	// Fallback are the static ask rules it replaces, put back when the hook
	// would not run.
	Fallback []string
}

// MCPServer is one stdio MCP server a session gets at launch.
type MCPServer struct {
	// Name is the managed tool's name, the server's name in the CLI.
	Name    string
	Command string
	Args    []string
	// EnvVars names variables passed from the user's environment.
	EnvVars []string
	// Cwd is the server's working directory, where the CLI takes one:
	// never the repo (S63-14).
	Cwd string
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
	// MCP are the managed tools' servers for this child only (aggy borrow
	// --tool and routing.edit_tools, T6709): edit only, since a review
	// child stays read-only, each started through aggy tool exec. Nil means none, and the
	// headless lockdown keeps every other server out.
	MCP []MCPServer
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

// FeatureChecker is implemented by an adapter whose lockdown lists what to
// turn off instead of what to keep, so a tool new in a later CLI would be
// on. It lets a borrow compare the CLI's own feature list with what aggy
// reviewed, without a model call.
type FeatureChecker interface {
	// FeatureCommand is the command that prints the feature list. The
	// caller sets its Path and Dir.
	FeatureCommand() Command
	// FeatureBaseline names what was reviewed, so a cached result is dropped
	// when an aggy release reviews more.
	FeatureBaseline() string
	// NewFeatures reads the command's output: the features on that aggy has
	// not reviewed as on. An error means the output says nothing either way.
	NewFeatures(out Output) ([]string, error)
}

// SelfTestEvidence is what a finished self-test's own record shows beyond the
// child's reply, which a model can get wrong.
type SelfTestEvidence struct {
	// Read says the planted token is in the record, so the child read it.
	Read bool
	// Tools names tools the record shows the child using that the self-test
	// kind's lockdown should have left it without.
	Tools []string
	// Unreadable says a record exists but could not be read whole, such as
	// one over its cap, so it shows nothing either way: not a pass.
	Unreadable bool
	// NoRecord says no record was found. Where the record's place is source
	// only, that may mean only that the CLI moved it, so the reply decides;
	// the diagnostic log says so.
	NoRecord bool
}

// SelfTestWitness is implemented by an adapter whose CLI keeps a record of a
// run's tool use that aggy can read. Claude has none: its JSON reply carries
// only the final result, so there the reply is all a self-test has.
type SelfTestWitness interface {
	SelfTestEvidence(req HeadlessRequest, res HeadlessResult, out Output, token string) SelfTestEvidence
}

// RunNoter is implemented by an adapter that can say something about a
// finished borrow its reply may not, such as tools its lockdown refused. The
// notes are the adapter's own words, never the CLI's, and go in the report.
type RunNoter interface {
	RunNotes(req HeadlessRequest, res HeadlessResult) []string
}

// SelfTestSettings is implemented by an adapter whose self-test loads the
// user's own settings, so the user can change its result.
type SelfTestSettings interface {
	// SelfTestInputs are the files whose content the result depends on: a
	// change to one tests again.
	SelfTestInputs() []string
	// SelfTestHint says what in them can make a child read the file, and
	// what to do about it.
	SelfTestHint() string
}

// ConfigWatcher is implemented by an adapter whose edit child could write
// the user's own CLI config: a borrow compares the file before and after.
type ConfigWatcher interface {
	UserConfigFile() string
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
	// ErrPermission: a tool the run needed stopped at a permission
	// prompt, which a headless run cannot answer, so it did not run.
	ErrPermission ErrorClass = "permission_prompt"
	ErrUnknown    ErrorClass = "unknown"
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

// ErrNoCapability matches a request this CLI version cannot run safely
// because its probe did not find a flag the kind needs (Grok's --sandbox
// for review, research, news, and image generation). It matches ErrUnsupported too, so the
// borrow passes the vendor over rather than run it without the flag.
var ErrNoCapability = fmt.Errorf("needs a flag this CLI's --help does not list (%w)", ErrUnsupported)

// ErrNoSandbox matches a kind that runs only under the CLI's own sandbox,
// which this CLI or host cannot apply. It matches ErrNoCapability too.
// The diagnostic log names it sandbox.missing.
var ErrNoSandbox = fmt.Errorf("its sandbox is missing: %w", ErrNoCapability)

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

	// LockdownCaps lists the probe capabilities (flags in the CLI's help)
	// a borrow of kind k cannot be locked down without. A CLI above the
	// tested range must show them all before it runs (design section 7).
	// A flag whose presence in the real help is not recorded still counts:
	// the check then fails closed, with the flag named.
	LockdownCaps(k Kind) []string

	// SelfTestKind is the kind whose lockdown runs the new-version
	// self-test: one that leaves the child no tool reading outside its
	// working dir, so a planted canary must not come back.
	SelfTestKind() Kind

	// Probe checks the installed CLI through run: path, version, and
	// capabilities. It never reads credential files. Sign-in status is
	// checked separately (SignIn, CheckSignIn), so Connected stays false.
	Probe(ctx context.Context, run Runner) (Probe, error)

	// SignIn returns the vendor's own sign-in step and, when one is
	// verified, a sign-in status check.
	SignIn() SignIn

	// Interactive builds the command for an on-screen session.
	Interactive(req InteractiveRequest) (Command, error)

	// PassFlags says which of the CLI's own flags aggy takes over or
	// refuses at aggy <vendor>; every other flag goes to the CLI.
	PassFlags() PassFlags

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
