package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/probe"
)

// Bin is the program name of the Claude Code CLI.
const Bin = "claude"

// Adapter is the Claude Code adapter. The zero value is ready to use.
type Adapter struct {
	// Path is the CLI to probe. Empty means Bin on PATH.
	Path string
}

var _ adapter.Adapter = Adapter{}

// New returns a Claude Code adapter.
func New() Adapter { return Adapter{} }

// Vendor returns adapter.Claude.
func (Adapter) Vendor() adapter.Vendor { return adapter.Claude }

// Tested is the Claude Code CLI version range this adapter was tested on: the
// version the facts in docs/adapters.md were verified with.
var Tested = adapter.VersionRange{Min: "2.1.311", Max: "2.1.311"}

// TestedVersions returns Tested.
func (Adapter) TestedVersions() adapter.VersionRange { return Tested }

// SignIn returns Claude's own sign-in step, from docs/design.md section 2.
// No sign-in status command is verified in docs/adapters.md, so there is
// no Check and the status is unknown, and the step is only printed:
// aggy never runs it.
func (Adapter) SignIn() adapter.SignIn {
	return adapter.SignIn{Steps: "run `claude`, then type `/login`"}
}

// Probe reports the CLI's path, version, and capabilities through run,
// using probe.Specs[adapter.Claude]. It never checks sign-in, so Connected
// is false.
func (a Adapter) Probe(ctx context.Context, run adapter.Runner) (adapter.Probe, error) {
	path := a.Path
	if path == "" {
		p, err := probe.LookPath(Bin)
		if err != nil {
			return adapter.Probe{Vendor: adapter.Claude}, err
		}
		path = p
	}
	return probe.Detect(ctx, run, adapter.Claude, path)
}

// modelArgs passes a model through unchanged, after adapter.ValidModel.
// --model is not recorded in docs/adapters.md (unconfirmed); an empty
// model adds nothing.
func modelArgs(model string) ([]string, error) {
	if err := adapter.ValidModel(model); err != nil || model == "" {
		return nil, err
	}
	return []string{"--model", model}, nil
}

// sessionArgs builds the session flags: --session-id <uuid> for a new
// session, --resume <id> to reopen one, and --fork-session to branch it,
// optionally with a pre-assigned id for the fork.
//
// Every id passes adapter.ValidSessionID, so none can be read as a flag.
// Claude's --resume takes an optional value; a valid id never starts with
// "-", so it is always taken as the value.
func sessionArgs(sessionID, resume string, fork bool) ([]string, error) {
	for _, id := range []string{sessionID, resume} {
		if id != "" {
			if err := adapter.ValidSessionID(adapter.Claude, id); err != nil {
				return nil, err
			}
		}
	}
	switch {
	case resume == "" && fork:
		return nil, fmt.Errorf("claude: fork needs a session to resume")
	case resume == "" && sessionID != "":
		return []string{"--session-id", sessionID}, nil
	case resume == "":
		return nil, nil
	case sessionID != "" && !fork:
		return nil, fmt.Errorf("claude: a session id with resume needs fork")
	}
	args := []string{"--resume", resume}
	if fork {
		args = append(args, "--fork-session")
		if sessionID != "" {
			args = append(args, "--session-id", sessionID)
		}
	}
	return args, nil
}

// Interactive builds an on-screen Claude Code session. Claude renders
// inline, so no alt-screen flag is needed. A brief is passed as a short
// first message that points to the file.
func (Adapter) Interactive(req adapter.InteractiveRequest) (adapter.Command, error) {
	args, err := modelArgs(req.Model)
	if err != nil {
		return adapter.Command{}, err
	}
	s, err := sessionArgs(req.SessionID, req.Resume, req.Fork)
	if err != nil {
		return adapter.Command{}, err
	}
	args = append(args, s...)
	if req.BriefFile != "" {
		if err := adapter.CheckAbs("brief file", req.BriefFile); err != nil {
			return adapter.Command{}, err
		}
		args = append(args, adapter.BriefPrompt(req.BriefFile))
	}
	return adapter.Command{Path: Bin, Args: args, Env: req.Env, Dir: req.Cwd, Stdin: adapter.StdinTerminal}, nil
}

// Headless builds claude -p --output-format json, with the prompt file as
// stdin (claude -p reads its prompt from stdin when none is given as an
// argument). Review runs with --permission-mode plan.
//
// Claude's tool, web search, turn-limit, and image-input flags are not
// recorded in docs/adapters.md, so image, video, research, and news are
// ErrUnsupported (research isolation could not be enforced), as are
// AllowedTools, WebSearch, and RefImages. MaxTurns is ignored. Edit runs
// with --permission-mode acceptEdits, a value that is unconfirmed.
func (Adapter) Headless(req adapter.HeadlessRequest) (adapter.Command, error) {
	if err := adapter.CheckAbs("prompt file", req.PromptFile); err != nil {
		return adapter.Command{}, err
	}
	if err := adapter.CheckAbs("cwd", req.Cwd); err != nil {
		return adapter.Command{}, err
	}
	switch {
	case req.Kind == adapter.Image || req.Kind == adapter.Video:
		return adapter.Command{}, fmt.Errorf("claude: %s: %w (no recorded media generation)", req.Kind, adapter.ErrUnsupported)
	case req.Kind == adapter.Research || req.Kind == adapter.News:
		return adapter.Command{}, fmt.Errorf("claude: %s: %w (no recorded flag removes file and shell tools)", req.Kind, adapter.ErrUnsupported)
	case req.AllowedTools != nil:
		return adapter.Command{}, fmt.Errorf("claude: allowed tools: %w (no recorded tool flag)", adapter.ErrUnsupported)
	case req.WebSearch:
		return adapter.Command{}, fmt.Errorf("claude: %s: %w: %s", req.Kind, adapter.ErrUnsupported, adapter.ErrWebSearchKind)
	case len(req.RefImages) > 0:
		return adapter.Command{}, fmt.Errorf("claude: reference images: %w", adapter.ErrUnsupported)
	}
	args := []string{"-p", "--output-format", "json"}
	m, err := modelArgs(req.Model)
	if err != nil {
		return adapter.Command{}, err
	}
	args = append(args, m...)
	switch req.Kind {
	case adapter.Review:
		args = append(args, "--permission-mode", "plan")
	case adapter.Edit:
		args = append(args, "--permission-mode", PermissionEdit)
	default:
		return adapter.Command{}, fmt.Errorf("claude: unknown kind %q", req.Kind)
	}
	s, err := sessionArgs(req.SessionID, req.Resume, false)
	if err != nil {
		return adapter.Command{}, err
	}
	args = append(args, s...)
	return adapter.Command{
		Path: Bin, Args: args, Env: req.Env, Dir: req.Cwd,
		Stdin: adapter.StdinFile, StdinPath: req.PromptFile,
	}, nil
}

// PermissionEdit is the permission mode of an edit borrow, which writes
// its own worktree without prompting. Unconfirmed: only "plan" is
// recorded in docs/adapters.md.
const PermissionEdit = "acceptEdits"

// result is the claude -p --output-format json reply. Only the fact that it
// is JSON is recorded; these field names are unconfirmed, so a reply
// without them degrades to ErrUnknown.
type result struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	Error     string `json:"error"`
	Usage     struct {
		Input  int64 `json:"input_tokens"`
		Output int64 `json:"output_tokens"`
	} `json:"usage"`
}

// limitText is the recorded fragment of Claude's plan-limit message.
const limitText = "hit your limit"

// ParseHeadless reads the JSON reply: final text, session id, tokens, and
// the error class. A session id aggy pre-assigned or resumed is kept, and
// a reply naming another one, or an invalid one, is an error.
//
// The class comes from structured fields only: an "error" class name,
// subtype error_max_turns, and is_error. The recorded limit text "hit
// your limit" is looked for only where the CLI, not the model, wrote it:
// in result when is_error is set, and in stderr when the exit is non-zero.
func (Adapter) ParseHeadless(req adapter.HeadlessRequest, out adapter.Output) (adapter.HeadlessResult, error) {
	assigned := req.SessionID
	if assigned == "" {
		assigned = req.Resume
	}
	res := adapter.HeadlessResult{SessionID: assigned}
	var r result
	// A JSON object with none of the expected fields is not a reply.
	parsed := lastJSON(out.Stdout, &r) && (r.Type != "" || r.SessionID != "" || r.Result != "")
	var idErr error
	if parsed {
		// A pre-assigned or resumed id must come back unchanged.
		res.SessionID, idErr = adapter.ParsedSessionID(adapter.Claude, assigned, r.SessionID)
		res.Text = r.Result
		res.Tokens = r.Usage.Input + r.Usage.Output
	}
	failed := out.ExitCode != 0 || r.IsError || strings.HasPrefix(r.Subtype, "error") || r.Error != ""
	if !failed && parsed {
		return res, idErr
	}
	switch {
	case r.Error != "":
		res.Error = adapter.ClassifyName(r.Error)
	case r.Subtype == "error_max_turns":
		res.Error = adapter.ErrMaxTurns
	case r.IsError && strings.Contains(r.Result, limitText),
		out.ExitCode != 0 && strings.Contains(string(out.Stderr), limitText):
		res.Error = adapter.ErrRateLimit
	default:
		res.Error = adapter.ErrUnknown
	}
	res.ErrorDetail = strings.TrimSpace(firstNonEmpty(r.Error, r.Result, string(out.Stderr)))
	if !parsed && out.ExitCode == 0 {
		return res, fmt.Errorf("claude: %w", adapter.ErrUnparsed)
	}
	return res, idErr
}

// lastJSON decodes the last line of out that holds a JSON object into v.
func lastJSON(out []byte, v any) bool {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		l := bytes.TrimSpace(lines[i])
		if len(l) > 0 && l[0] == '{' && json.Unmarshal(l, v) == nil {
			return true
		}
	}
	return false
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// Artifacts returns nothing: Claude's headless kinds produce no media, and
// no Claude artifact path is recorded.
func (Adapter) Artifacts(adapter.HeadlessRequest, adapter.HeadlessResult) ([]adapter.Artifact, error) {
	return nil, nil
}

// TranscriptTail returns ErrNoTranscript: Claude's transcript path and
// format are not recorded in docs/adapters.md.
func (Adapter) TranscriptTail(adapter.SessionRef, int) (adapter.Transcript, error) {
	return adapter.Transcript{}, adapter.ErrNoTranscript
}

// ConfigPatches returns no patches yet.
func (Adapter) ConfigPatches(adapter.ConfigContext) ([]patch.Patch, error) { return nil, nil }
