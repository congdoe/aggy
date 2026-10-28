package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// SandboxEdit is the sandbox of an edit borrow, which writes its own
// worktree. Unconfirmed: only "read-only" is recorded.
const SandboxEdit = "workspace-write"

var modelRE = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)

// modelArgs passes a model as the config override -c model="<m>". The -c
// mechanism is recorded; the model key is unconfirmed. An empty model adds
// nothing.
func modelArgs(model string) ([]string, error) {
	if model == "" {
		return nil, nil
	}
	if !modelRE.MatchString(model) {
		return nil, fmt.Errorf("codex: model %q has characters a config value cannot hold", model)
	}
	return []string{"-c", `model="` + model + `"`}, nil
}

// Interactive builds an on-screen Codex session with --no-alt-screen, so
// its output stays in the terminal's scrollback: codex for a new session,
// codex resume <id> to reopen one. Codex cannot be given a session id in
// advance and fork is not recorded, so both are ErrUnsupported. A brief is
// passed as a short first message pointing to the file; that Codex takes
// a prompt argument at session start is unconfirmed (T6619).
func (Adapter) Interactive(req adapter.InteractiveRequest) (adapter.Command, error) {
	if req.SessionID != "" {
		return adapter.Command{}, fmt.Errorf("codex: pre-assigned session id: %w", adapter.ErrUnsupported)
	}
	if req.Fork {
		return adapter.Command{}, fmt.Errorf("codex: fork: %w", adapter.ErrUnsupported)
	}
	var args []string
	if req.Resume != "" {
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

// Headless builds codex exec --json (or codex exec resume <id> --json).
// The prompt argument is "-" and stdin is the prompt file: codex exec
// reads a non-terminal stdin to EOF, and a regular file reaches EOF, so it
// never waits as it would on an open pipe. That "-" reads the prompt from
// stdin is unconfirmed.
//
// Every read-only kind runs with --sandbox read-only; edit uses
// SandboxEdit. Research and news add Isolation and live web search.
// WebSearch adds live web search to the other kinds, and RefImages go in
// with -i. Video, AllowedTools (no recorded tool allowlist), and a
// pre-assigned SessionID are ErrUnsupported. MaxTurns is ignored.
func (Adapter) Headless(req adapter.HeadlessRequest) (adapter.Command, error) {
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
	args := []string{"exec"}
	if req.Resume != "" {
		args = append(args, "resume", req.Resume)
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
	case adapter.Image, adapter.Review:
		args = append(args, "--sandbox", "read-only")
		if req.WebSearch {
			args = append(args, WebSearchLive...)
		}
	case adapter.Edit:
		args = append(args, "--sandbox", SandboxEdit)
		if req.WebSearch {
			args = append(args, WebSearchLive...)
		}
	default:
		return adapter.Command{}, fmt.Errorf("codex: unknown kind %q", req.Kind)
	}
	for _, img := range req.RefImages {
		if err := adapter.CheckAbs("reference image", img); err != nil {
			return adapter.Command{}, err
		}
		args = append(args, "-i", img)
	}
	args = append(args, "-")
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

// Limit signals. The code and text are recorded in the binary; whether
// headless Codex reports them in --json is unconfirmed (T6617), so stderr
// is read for them too.
const (
	LimitCode = "usage_limit_reached"
	LimitText = "You've hit your usage limit"
)

// ParseHeadless reads the JSON event stream: the session id from the
// first thread.started event, the final text from the last completed
// agent message, tokens from turn.completed usage, and errors from error
// or turn.failed events and stderr. A usage limit maps to rate_limit.
func (Adapter) ParseHeadless(req adapter.HeadlessRequest, out adapter.Output) (adapter.HeadlessResult, error) {
	res := adapter.HeadlessResult{}
	var errText []string
	sawThread := false
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
				res.SessionID, sawThread = e.ThreadID, true
			}
		case "item.completed":
			if (e.Item.Type == "agent_message" || e.Item.Type == "") && e.Item.Text != "" {
				res.Text = e.Item.Text
			}
		case "turn.completed":
			res.Tokens += e.Usage.Input + e.Usage.Output
		case "error", "turn.failed":
			errText = append(errText, strings.TrimSpace(e.Code+" "+e.Message+" "+string(e.Error)))
		}
	}
	if res.SessionID == "" {
		res.SessionID = req.Resume
	}
	all := strings.Join(errText, "\n") + "\n" + string(out.Stderr)
	switch {
	case strings.Contains(all, LimitCode) || strings.Contains(all, LimitText):
		res.Error = adapter.ErrRateLimit
	case out.ExitCode != 0 || len(errText) > 0:
		res.Error = adapter.ErrUnknown
	}
	if res.Error != adapter.ErrNone {
		res.ErrorDetail = strings.TrimSpace(firstNonEmpty(strings.Join(errText, "\n"), string(out.Stderr)))
	}
	if !sawThread && out.ExitCode == 0 {
		res.Error = adapter.ErrUnknown
		return res, fmt.Errorf("codex: no thread.started event: %w", adapter.ErrUnparsed)
	}
	return res, nil
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
// not in the JSON stream. It returns paths only and copies nothing.
func (a Adapter) Artifacts(_ adapter.HeadlessRequest, res adapter.HeadlessResult) ([]adapter.Artifact, error) {
	id := res.SessionID
	if id == "" || id != filepath.Base(id) || id == "." || id == ".." {
		return nil, nil
	}
	home := a.Home()
	if !filepath.IsAbs(home) {
		return nil, fmt.Errorf("codex: cannot locate the Codex home")
	}
	return adapter.ListFiles(filepath.Join(home, "generated_images", id), "image", ".png")
}

// TranscriptTail returns ErrNoTranscript: the format of Codex's session
// files is not recorded in docs/adapters.md.
func (Adapter) TranscriptTail(adapter.SessionRef, int) (adapter.Transcript, error) {
	return adapter.Transcript{}, adapter.ErrNoTranscript
}

// ConfigPatches returns no patches here. Codex's CLAUDE.md fallback (the
// project_doc_fallback_filenames patch) is built in internal/parity.
func (Adapter) ConfigPatches(adapter.ConfigContext) ([]patch.Patch, error) { return nil, nil }
