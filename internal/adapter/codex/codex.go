package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/probe"
)

// Bin is the program name of the Codex CLI.
const Bin = "codex"

// Adapter is the Codex adapter. The zero value is ready to use.
type Adapter struct {
	// Path is the CLI to probe. Empty means Bin on PATH.
	Path string
	// Getenv reads HOME and CODEX_HOME for Artifacts. Nil means os.Getenv.
	Getenv func(string) string
	// Caps are the capabilities a probe of this CLI found. Headless adds
	// --ignore-user-config to research and news only when they include
	// probe.CapIgnoreUserConfig.
	//
	// TODO(T6621): the borrow runner must build the adapter with the
	// probe's Caps; without them --ignore-user-config is never passed.
	Caps []string
}

var _ adapter.Adapter = Adapter{}

// New returns a Codex adapter.
func New() Adapter { return Adapter{} }

// Vendor returns adapter.Codex.
func (Adapter) Vendor() adapter.Vendor { return adapter.Codex }

// Tested is the Codex CLI version range this adapter was tested on: the
// version the facts in docs/adapters.md were verified with.
var Tested = adapter.VersionRange{Min: "0.164.0", Max: "0.164.0"}

// TestedVersions returns Tested.
func (Adapter) TestedVersions() adapter.VersionRange { return Tested }

// SignIn returns Codex's own sign-in step, from docs/design.md section 2.
// No sign-in status command is verified in docs/adapters.md, so there is
// no Check and the status is unknown, and the step is only printed:
// aggy never runs it.
func (Adapter) SignIn() adapter.SignIn {
	return adapter.SignIn{Steps: "run `codex login`"}
}

// Probe reports the CLI's path, version, and capabilities through run,
// using probe.Specs[adapter.Codex].
func (a Adapter) Probe(ctx context.Context, run adapter.Runner) (adapter.Probe, error) {
	path := a.Path
	if path == "" {
		p, err := probe.LookPath(Bin)
		if err != nil {
			return adapter.Probe{Vendor: adapter.Codex}, err
		}
		path = p
	}
	return probe.Detect(ctx, run, adapter.Codex, path)
}

// Recorded flags (docs/adapters.md).
var (
	// WebSearchLive turns on live web search: exec has no --search.
	WebSearchLive = []string{"-c", `web_search="live"`}
	// Isolation removes Codex's shell, exec, and image-view tools for
	// research and news, which also run with --sandbox read-only in an
	// empty temp dir. Unconfirmed (T6618): whether this fully stops file
	// reads is still to be checked live.
	Isolation = []string{"--disable", "shell_tool", "--disable", "unified_exec", "--disable", "view_image"}
)

// IgnoreUserConfig keeps the user's config, and so their MCP servers, out
// of a research child. Not recorded: used only when a probe found it.
const IgnoreUserConfig = "--ignore-user-config"

// SandboxEdit is the sandbox of an edit borrow, which writes its own
// worktree. Unconfirmed: only "read-only" is recorded.
const SandboxEdit = "workspace-write"

// modelArgs passes a model as the config override -c model="<m>". The -c
// mechanism is recorded; the model key is unconfirmed. An empty model adds
// nothing.
func modelArgs(model string) ([]string, error) {
	if model == "" {
		return nil, nil
	}
	if err := adapter.ValidModel(model); err != nil || strings.ContainsAny(model, `"\`) {
		return nil, fmt.Errorf("codex: model %q has characters a config value cannot hold", model)
	}
	return []string{"-c", `model="` + model + `"`}, nil
}

// Interactive builds an on-screen Codex session with --no-alt-screen, so
// its output stays in the terminal's scrollback: codex for a new session,
// codex resume <id> to reopen one. The id passes
// adapter.ValidSessionID, so it cannot be read as a flag. Codex cannot be
// given a session id in advance and fork is not recorded, so both are
// ErrUnsupported. A brief is passed as a short first message pointing to
// the file; that Codex takes a prompt argument at session start is
// unconfirmed (T6619).
func (Adapter) Interactive(req adapter.InteractiveRequest) (adapter.Command, error) {
	if req.SessionID != "" {
		return adapter.Command{}, fmt.Errorf("codex: pre-assigned session id: %w", adapter.ErrUnsupported)
	}
	if req.Fork {
		return adapter.Command{}, fmt.Errorf("codex: fork: %w", adapter.ErrUnsupported)
	}
	var args []string
	if req.Resume != "" {
		if err := adapter.ValidSessionID(adapter.Codex, req.Resume); err != nil {
			return adapter.Command{}, err
		}
		args = append(args, "resume")
	}
	args = append(args, "--no-alt-screen")
	m, err := modelArgs(req.Model)
	if err != nil {
		return adapter.Command{}, err
	}
	args = append(args, m...)
	if req.Resume != "" {
		args = append(args, req.Resume)
	}
	if req.BriefFile != "" {
		if err := adapter.CheckAbs("brief file", req.BriefFile); err != nil {
			return adapter.Command{}, err
		}
		args = append(args, adapter.BriefPrompt(req.BriefFile))
	}
	return adapter.Command{Path: Bin, Args: args, Env: req.Env, Dir: req.Cwd, Stdin: adapter.StdinTerminal}, nil
}

// Headless builds
//
//	codex exec [-i <ref>]... --json [-c model=...] --sandbox <mode> [isolation] [resume <id>] -- -
//
// The exec flags all go before the resume subcommand, which takes only its
// own flags. -i takes one or more values, so the references come first and
// --json ends them. The prompt argument is "-" after "--", and stdin is
// the prompt file: codex exec reads a non-terminal stdin to EOF, and a
// regular file reaches EOF, so it never waits as it would on an open pipe.
// The order and "-" are unconfirmed on 0.164.
//
// Read-only kinds run with --sandbox read-only, edit with SandboxEdit.
// Research and news add Isolation, live web search, and, when the probe
// found it, --ignore-user-config; they need IsolatedCwd. Web search
// anywhere else, video, AllowedTools (no recorded tool allowlist), and a
// pre-assigned SessionID are ErrUnsupported. MaxTurns is ignored.
func (a Adapter) Headless(req adapter.HeadlessRequest) (adapter.Command, error) {
	if err := adapter.CheckAbs("prompt file", req.PromptFile); err != nil {
		return adapter.Command{}, err
	}
	if err := adapter.CheckAbs("cwd", req.Cwd); err != nil {
		return adapter.Command{}, err
	}
	switch {
	case req.Kind == adapter.Video:
		return adapter.Command{}, fmt.Errorf("codex: video: %w", adapter.ErrUnsupported)
	case req.AllowedTools != nil:
		return adapter.Command{}, fmt.Errorf("codex: allowed tools: %w (no recorded tool allowlist)", adapter.ErrUnsupported)
	case req.SessionID != "":
		return adapter.Command{}, fmt.Errorf("codex: pre-assigned session id: %w", adapter.ErrUnsupported)
	}
	if err := adapter.CheckWebSearch(adapter.Codex, req); err != nil {
		return adapter.Command{}, err
	}
	if err := adapter.CheckIsolated(adapter.Codex, req); err != nil {
		return adapter.Command{}, err
	}
	if req.Resume != "" {
		if err := adapter.ValidSessionID(adapter.Codex, req.Resume); err != nil {
			return adapter.Command{}, err
		}
	}
	args := []string{"exec"}
	for _, img := range req.RefImages {
		if err := adapter.CheckAbs("reference image", img); err != nil {
			return adapter.Command{}, err
		}
		// -i splits its values on commas, so "/b,c.png" would be two
		// images, the second relative to the cwd.
		if strings.ContainsRune(img, ',') {
			return adapter.Command{}, fmt.Errorf("codex: reference image %q holds a comma, which -i splits on", img)
		}
		args = append(args, "-i", img)
	}
	args = append(args, "--json")
	m, err := modelArgs(req.Model)
	if err != nil {
		return adapter.Command{}, err
	}
	args = append(args, m...)
	switch req.Kind {
	case adapter.Research, adapter.News:
		args = append(args, "--sandbox", "read-only")
		args = append(args, Isolation...)
		args = append(args, WebSearchLive...)
		if slices.Contains(a.Caps, probe.CapIgnoreUserConfig) {
			args = append(args, IgnoreUserConfig)
		}
	case adapter.Image, adapter.Review:
		args = append(args, "--sandbox", "read-only")
	case adapter.Edit:
		args = append(args, "--sandbox", SandboxEdit)
	default:
		return adapter.Command{}, fmt.Errorf("codex: unknown kind %q", req.Kind)
	}
	if req.Resume != "" {
		args = append(args, "resume", req.Resume)
	}
	args = append(args, "--", "-")
	return adapter.Command{
		Path: Bin, Args: args, Env: req.Env, Dir: req.Cwd,
		Stdin: adapter.StdinFile, StdinPath: req.PromptFile,
	}, nil
}

// event is one codex exec --json line. Only thread.started with thread_id
// is recorded; the item, usage, and error shapes are unconfirmed.
type event struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Usage struct {
		Input  int64 `json:"input_tokens"`
		Output int64 `json:"output_tokens"`
	} `json:"usage"`
	Message string          `json:"message"`
	Code    string          `json:"code"`
	Error   json.RawMessage `json:"error"`
}

// fields returns the event's error code and message, from the top level
// or a nested "error" object or string.
func (e event) fields() (code, msg string) {
	code, msg = e.Code, e.Message
	var nested struct {
		Code    string `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	var s string
	switch {
	case len(e.Error) == 0:
	case json.Unmarshal(e.Error, &nested) == nil:
		code = firstNonEmpty(code, nested.Code, nested.Type)
		msg = firstNonEmpty(msg, nested.Message)
	case json.Unmarshal(e.Error, &s) == nil:
		msg = firstNonEmpty(msg, s)
	}
	return code, msg
}

// Limit signals. The code and text are recorded in the binary; whether
// headless Codex reports them in --json is unconfirmed (T6617).
const (
	LimitCode = "usage_limit_reached"
	LimitText = "You've hit your usage limit"
)

// ParseHeadless reads the JSON event stream: the session id from the
// first thread.started event (checked with adapter.ValidSessionID), the
// final text from the last completed agent message, and tokens from
// turn.completed usage.
//
// The error class comes from error and turn.failed events only: their
// code, or the CLI's own error message, names a usage limit. An error
// event that a turn.completed follows is a success with a warning in
// ErrorDetail. Stderr is read for the limit only when the run exited
// non-zero with no error event, since the --json limit report is
// unconfirmed.
func (Adapter) ParseHeadless(req adapter.HeadlessRequest, out adapter.Output) (adapter.HeadlessResult, error) {
	res := adapter.HeadlessResult{}
	var errText []string
	sawThread, pending, failed, limit := false, false, false, false
	var idErr error
	for _, line := range bytes.Split(out.Stdout, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e event
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Type {
		case "thread.started":
			if !sawThread {
				sawThread = true
				// On a resume the thread keeps the resumed id.
				res.SessionID, idErr = adapter.ParsedSessionID(adapter.Codex, req.Resume, e.ThreadID)
			}
		case "item.completed":
			if (e.Item.Type == "agent_message" || e.Item.Type == "") && e.Item.Text != "" {
				res.Text = e.Item.Text
			}
		case "turn.completed":
			res.Tokens += e.Usage.Input + e.Usage.Output
			pending = false
		case "error", "turn.failed":
			code, msg := e.fields()
			errText = append(errText, strings.TrimSpace(code+" "+msg))
			if code == LimitCode || strings.Contains(msg, LimitText) {
				limit = true
			}
			pending = true
			if e.Type == "turn.failed" {
				failed = true
			}
		}
	}
	if res.SessionID == "" && idErr == nil && req.Resume != "" {
		res.SessionID = req.Resume
	}
	stderr := string(out.Stderr)
	switch {
	case limit && (failed || pending || out.ExitCode != 0):
		res.Error = adapter.ErrRateLimit
	case failed || pending:
		res.Error = adapter.ErrUnknown
	case out.ExitCode != 0 && len(errText) == 0 && (strings.Contains(stderr, LimitCode) || strings.Contains(stderr, LimitText)):
		res.Error = adapter.ErrRateLimit
	case out.ExitCode != 0:
		res.Error = adapter.ErrUnknown
	}
	if res.Error != adapter.ErrNone || len(errText) > 0 {
		res.ErrorDetail = strings.TrimSpace(firstNonEmpty(strings.Join(errText, "\n"), stderr))
	}
	if !sawThread && out.ExitCode == 0 {
		res.Error = adapter.ErrUnknown
		return res, fmt.Errorf("codex: no thread.started event: %w", adapter.ErrUnparsed)
	}
	return res, idErr
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// Home is Codex's config directory: $CODEX_HOME, else ~/.codex.
func (a Adapter) Home() string {
	getenv := a.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if h := getenv("CODEX_HOME"); h != "" {
		return filepath.Clean(h)
	}
	if h := getenv("HOME"); h != "" {
		return filepath.Join(h, ".codex")
	}
	return ""
}

// Artifacts lists every image in generated_images/<thread-id>/ under the
// Codex home: one request can produce several, and the image tool call is
// not in the JSON stream. It returns paths only and copies nothing. An
// id that fails adapter.ValidSessionID is an error.
func (a Adapter) Artifacts(_ adapter.HeadlessRequest, res adapter.HeadlessResult) ([]adapter.Artifact, error) {
	if res.SessionID == "" {
		return nil, nil
	}
	if err := adapter.ValidSessionID(adapter.Codex, res.SessionID); err != nil {
		return nil, err
	}
	home := a.Home()
	if !filepath.IsAbs(home) {
		return nil, fmt.Errorf("codex: cannot locate the Codex home")
	}
	return adapter.ListFiles(filepath.Join(home, "generated_images", res.SessionID), "image", ".png")
}

// TranscriptTail returns ErrNoTranscript: the format of Codex's session
// files is not recorded in docs/adapters.md.
func (Adapter) TranscriptTail(adapter.SessionRef, int) (adapter.Transcript, error) {
	return adapter.Transcript{}, adapter.ErrNoTranscript
}

// ConfigPatches returns no patches here. Codex's CLAUDE.md fallback (the
// project_doc_fallback_filenames patch) is built in internal/parity.
func (Adapter) ConfigPatches(adapter.ConfigContext) ([]patch.Patch, error) { return nil, nil }
