package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/probe"
)

// Bin is the program name of the Grok Build CLI.
const Bin = "grok"

// Adapter is the Grok Build adapter. The zero value is ready to use.
type Adapter struct {
	// Path is the CLI to probe. Empty means Bin on PATH.
	Path string
	// Getenv reads HOME and GROK_HOME for Artifacts. Nil means os.Getenv.
	Getenv func(string) string
}

var _ adapter.Adapter = Adapter{}

// New returns a Grok Build adapter.
func New() Adapter { return Adapter{} }

// Vendor returns adapter.Grok.
func (Adapter) Vendor() adapter.Vendor { return adapter.Grok }

// Tested is the Grok Build CLI version range this adapter was tested on:
// the version the facts in docs/adapters.md were verified with.
var Tested = adapter.VersionRange{Min: "1.0.70", Max: "1.0.70"}

// TestedVersions returns Tested.
func (Adapter) TestedVersions() adapter.VersionRange { return Tested }

// SignIn returns Grok's own sign-in step, from docs/design.md section 2.
// No sign-in status command is verified in docs/adapters.md, so there is
// no Check and the status is unknown, and the step is only printed:
// aggy never runs it.
func (Adapter) SignIn() adapter.SignIn {
	return adapter.SignIn{Steps: "run `grok login`"}
}

// Probe reports the CLI's path, version, and capabilities through run,
// using probe.Specs[adapter.Grok].
func (a Adapter) Probe(ctx context.Context, run adapter.Runner) (adapter.Probe, error) {
	path := a.Path
	if path == "" {
		p, err := probe.LookPath(Bin)
		if err != nil {
			return adapter.Probe{Vendor: adapter.Grok}, err
		}
		path = p
	}
	return probe.Detect(ctx, run, adapter.Grok, path)
}

// Tool names, live-verified on grok 1.0.70 (2026-10-28). Grok ignores
// the whole --tools allowlist when any name in it is unknown, and only
// warns on stderr (UnknownToolWarning), so every name here must be exact.
// x_keyword_search and x_semantic_search do not exist; web_search brings
// open_page and open_page_with_find with it.
var (
	// SearchTools is research and news, and the only tools they may have.
	SearchTools = []string{"web_search"}
	// ImageTools is image generation. image_edit, added on resume, is
	// unverified headless.
	ImageTools = []string{"image_gen"}
	// VideoTools is video generation: Grok has no text-to-video tool, so
	// the child makes a still with image_gen and animates it with
	// image_to_video. Unverified headless.
	VideoTools = []string{"image_gen", "image_to_video"}
	// ReadTools is review, which also runs with --permission-mode plan.
	ReadTools = []string{"read_file", "grep", "list_dir"}
	// EditTools is edit, in its own worktree. No shell: aggy check runs
	// the tests.
	EditTools = []string{"read_file", "grep", "list_dir", "write", "search_replace"}
	// Lockdown is every built-in tool a borrow may be denied. Every run
	// passes it as --disallowed-tools, minus the tools its kind uses, so
	// the child keeps only those even if --tools were ignored. search_tool
	// and use_tool, the MCP meta-tools, are always denied, and web_search
	// is denied to every kind but research and news.
	//
	// open_page and open_page_with_find, the browse helpers that come
	// with web_search, are not in it: they cannot be removed by name, and
	// naming them makes Grok warn "name no tool ... so they remove
	// nothing" (live check C2, 2026-10-28), which fails a borrow closed.
	// Denying web_search takes them along: the review argv, run live
	// (C3, 2026-10-28), left the model reporting only read_file, grep,
	// and list_dir, with no warning. --disable-web-search (not in a
	// recorded help) is unverified and no longer needed: TODO(T6737).
	Lockdown = []string{
		"read_file", "grep", "list_dir", "write", "search_replace", "run_terminal_cmd", "web_fetch",
		"Agent", "search_tool", "use_tool", "image_gen", "image_edit", "image_to_video",
		"reference_to_video", "send_feedback", "monitor", "workflow", "todo_write",
		"ask_user_question", "enter_plan_mode", "exit_plan_mode", "web_search",
	}
	// NoSubagents keeps the child from starting its own agents.
	NoSubagents = "--no-subagents"
)

// UnknownToolWarning is what Grok writes to stderr when a --tools name is
// unknown, after which it ignores the whole allowlist. ParseHeadless takes
// it as a failed borrow.
const UnknownToolWarning = "name no tool"

// alwaysDenied are never allowed, whatever AllowedTools says.
var alwaysDenied = []string{"search_tool", "use_tool", "Agent"}

// permitted lists the tools each kind may have: its own and, through
// AllowedTools, nothing else.
func permitted(k adapter.Kind) []string {
	switch k {
	case adapter.Research, adapter.News:
		return SearchTools
	case adapter.Image:
		return append(slices.Clone(ImageTools), "image_edit")
	case adapter.Video:
		return append(slices.Clone(VideoTools), "reference_to_video")
	case adapter.Review:
		return ReadTools
	case adapter.Edit:
		return EditTools
	}
	return nil
}

// canonical returns the permitted tool named t, compared
// case-insensitively, or false.
func canonical(k adapter.Kind, t string) (string, bool) {
	for _, p := range permitted(k) {
		if strings.EqualFold(p, t) {
			return p, true
		}
	}
	return "", false
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

// sessionArgs is as on Claude: --session-id <uuid>, --resume <id>, and
// --fork-session. Every id passes adapter.ValidSessionID, so none can be
// read as a flag.
func sessionArgs(sessionID, resume string, fork bool) ([]string, error) {
	for _, id := range []string{sessionID, resume} {
		if id != "" {
			if err := adapter.ValidSessionID(adapter.Grok, id); err != nil {
				return nil, err
			}
		}
	}
	switch {
	case resume == "" && fork:
		return nil, fmt.Errorf("grok: fork needs a session to resume")
	case resume == "" && sessionID != "":
		return []string{"--session-id", sessionID}, nil
	case resume == "":
		return nil, nil
	case sessionID != "" && !fork:
		return nil, fmt.Errorf("grok: a session id with resume needs fork")
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

// Interactive builds an on-screen Grok session with --no-alt-screen, so
// its output stays in the scrollback. A brief is passed as a short first
// message pointing to the file; that Grok takes a prompt argument at
// session start is unconfirmed (T6619).
func (Adapter) Interactive(req adapter.InteractiveRequest) (adapter.Command, error) {
	args := []string{"--no-alt-screen"}
	m, err := modelArgs(req.Model)
	if err != nil {
		return adapter.Command{}, err
	}
	args = append(args, m...)
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

// Headless builds
//
//	grok -p --output-format json [--model m] [session] [--permission-mode plan]
//	     --tools <kind's tools> --disallowed-tools <Lockdown minus them> --no-subagents [--max-turns N]
//
// with the prompt file as stdin. AllowedTools may only name tools the
// kind already permits (compared case-insensitively), and never the MCP
// meta-tools. Review runs with --permission-mode plan, MaxTurns sets
// --max-turns, and research and news need IsolatedCwd. Web search outside
// research and news, and RefImages (no recorded flag), are ErrUnsupported.
// Grok takes no per-run hook flag (it does load Claude's settings.json
// hooks by default), so GuardCommand is not wired: TODO(M2 hooks).
func (Adapter) Headless(req adapter.HeadlessRequest) (adapter.Command, error) {
	if err := adapter.CheckAbs("prompt file", req.PromptFile); err != nil {
		return adapter.Command{}, err
	}
	if err := adapter.CheckAbs("cwd", req.Cwd); err != nil {
		return adapter.Command{}, err
	}
	if len(req.RefImages) > 0 {
		return adapter.Command{}, fmt.Errorf("grok: reference images: %w", adapter.ErrUnsupported)
	}
	if err := adapter.CheckWebSearch(adapter.Grok, req); err != nil {
		return adapter.Command{}, err
	}
	if err := adapter.CheckIsolated(adapter.Grok, req); err != nil {
		return adapter.Command{}, err
	}
	var tools []string
	plan := false
	switch req.Kind {
	case adapter.Research, adapter.News:
		tools = slices.Clone(SearchTools)
	case adapter.Image:
		tools = slices.Clone(ImageTools)
		if req.Resume != "" {
			tools = append(tools, "image_edit")
		}
	case adapter.Video:
		tools = slices.Clone(VideoTools)
	case adapter.Review:
		tools, plan = slices.Clone(ReadTools), true
	case adapter.Edit:
		tools = slices.Clone(EditTools)
	default:
		return adapter.Command{}, fmt.Errorf("grok: unknown kind %q", req.Kind)
	}
	for _, t := range req.AllowedTools {
		c, ok := canonical(req.Kind, t)
		if !ok || slices.Contains(alwaysDenied, c) {
			return adapter.Command{}, fmt.Errorf("grok: %s may use only %s, not %q", req.Kind, strings.Join(permitted(req.Kind), ", "), t)
		}
		if !slices.Contains(tools, c) {
			tools = append(tools, c)
		}
	}
	var deny []string
	for _, d := range Lockdown {
		if !slices.Contains(tools, d) || slices.Contains(alwaysDenied, d) {
			deny = append(deny, d)
		}
	}
	args := []string{"-p", "--output-format", "json"}
	m, err := modelArgs(req.Model)
	if err != nil {
		return adapter.Command{}, err
	}
	args = append(args, m...)
	s, err := sessionArgs(req.SessionID, req.Resume, false)
	if err != nil {
		return adapter.Command{}, err
	}
	args = append(args, s...)
	if plan {
		args = append(args, "--permission-mode", "plan")
	}
	args = append(args, "--tools", strings.Join(tools, ","), "--disallowed-tools", strings.Join(deny, ","), NoSubagents)
	if req.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	return adapter.Command{
		Path: Bin, Args: args, Env: req.Env, Dir: req.Cwd,
		Stdin: adapter.StdinFile, StdinPath: req.PromptFile,
	}, nil
}

// TextFields are the reply fields read for the final text, in order. Only
// sessionId is recorded in Grok's JSON; these names are unconfirmed.
var TextFields = []string{"result", "text", "response"}

// ParseHeadless first fails closed when the CLI rejected a lockdown flag
// or printed UnknownToolWarning (ErrLockdown). It then reads the JSON reply: sessionId (recorded; checked with
// adapter.ValidSessionID, and a pre-assigned or resumed id is kept, with
// an error if the reply names another), and the final text from the first of
// TextFields present.
//
// The error class comes from the JSON "error" field, mapped with
// adapter.ClassifyName (Grok's classes mirror Claude Code's), and
// is_error. The CLI's stderr is read for "rate_limit" only when the run
// exited non-zero with no JSON error.
func (Adapter) ParseHeadless(req adapter.HeadlessRequest, out adapter.Output) (adapter.HeadlessResult, error) {
	assigned := req.SessionID
	if assigned == "" {
		assigned = req.Resume
	}
	res := adapter.HeadlessResult{SessionID: assigned}
	if err := lockdownErr(out); err != nil {
		res.Error, res.ErrorDetail = adapter.ErrLockdown, strings.TrimSpace(string(out.Stderr))
		return res, err
	}
	var obj map[string]any
	parsed := false
	lines := bytes.Split(bytes.TrimSpace(out.Stdout), []byte("\n"))
	for i := len(lines) - 1; i >= 0 && !parsed; i-- {
		l := bytes.TrimSpace(lines[i])
		parsed = len(l) > 0 && l[0] == '{' && json.Unmarshal(l, &obj) == nil
	}
	errName := ""
	isError := false
	var idErr error
	if parsed {
		reported, _ := obj["sessionId"].(string)
		// A pre-assigned or resumed id must come back unchanged.
		res.SessionID, idErr = adapter.ParsedSessionID(adapter.Grok, assigned, reported)
		for _, f := range TextFields {
			if s, ok := obj[f].(string); ok {
				res.Text = s
				break
			}
		}
		errName, _ = obj["error"].(string)
		isError, _ = obj["is_error"].(bool)
		res.Tokens = tokens(obj)
	}
	if out.ExitCode == 0 && errName == "" && !isError && parsed {
		return res, idErr
	}
	stderr := string(out.Stderr)
	switch {
	case errName != "":
		res.Error = adapter.ClassifyName(errName)
	case out.ExitCode != 0 && strings.Contains(stderr, "rate_limit"):
		res.Error = adapter.ErrRateLimit
	default:
		res.Error = adapter.ErrUnknown
	}
	res.ErrorDetail = strings.TrimSpace(errName + " " + stderr)
	if !parsed && out.ExitCode == 0 {
		return res, fmt.Errorf("grok: %w", adapter.ErrUnparsed)
	}
	return res, idErr
}

// tokens reads a usage object's input_tokens and output_tokens (or its
// total_tokens), as Claude Code reports them. Grok's usage field is not
// recorded (unconfirmed): a reply without one counts 0.
func tokens(obj map[string]any) int64 {
	u, ok := obj["usage"].(map[string]any)
	if !ok {
		return 0
	}
	num := func(k string) int64 {
		if f, ok := u[k].(float64); ok && f > 0 && f < 1e15 {
			return int64(f)
		}
		return 0
	}
	if n := num("input_tokens") + num("output_tokens"); n > 0 {
		return n
	}
	return num("total_tokens")
}

// lockdownErr fails the run closed when Grok refused a lockdown flag, or
// warned that a --tools name is unknown: it then ignores the whole
// allowlist, so the child ran with every tool.
func lockdownErr(out adapter.Output) error {
	if err := adapter.CheckLockdown(adapter.Grok, out); err != nil {
		return err
	}
	for _, line := range strings.Split(string(out.Stderr), "\n") {
		if strings.Contains(line, UnknownToolWarning) {
			return fmt.Errorf("grok: %w: Grok ignored the --tools allowlist because a tool name is unknown (%s)",
				adapter.ErrLockdownRejected, strings.TrimSpace(line))
		}
	}
	return nil
}

// Home is Grok's config directory: $GROK_HOME, else ~/.grok.
func (a Adapter) Home() string {
	getenv := a.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if h := getenv("GROK_HOME"); h != "" {
		return filepath.Clean(h)
	}
	if h := getenv("HOME"); h != "" {
		return filepath.Join(h, ".grok")
	}
	return ""
}

// EncodeCwd is how a working directory names its folder under sessions/.
// Recorded only as "url-encoded"; url.QueryEscape (which encodes "/" as
// %2F) is unconfirmed, so Artifacts falls back to a scan.
func EncodeCwd(cwd string) string { return url.QueryEscape(cwd) }

// Artifacts lists every file in sessions/<encoded cwd>/<sessionId>/images/
// under the Grok home (Grok replies with the path relative to the session
// folder, images/1.jpg), or, for the video kind, in .../videos/, where
// Grok's video tools write <n>.mp4 (grok-build 2bdd1d6a,
// xai-grok-tools/src/implementations/grok_build/video_gen/mod.rs), for the
// borrow's artifact gate to judge. When
// that folder is missing (the cwd encoding is unconfirmed, or the CLI saw
// the cwd with symlinks resolved), it falls back to the one
// sessions/*/<sessionId>/images folder; several matches are an error, not
// a guess. An id that fails adapter.ValidSessionID is an error. It
// returns paths only and copies nothing.
func (a Adapter) Artifacts(req adapter.HeadlessRequest, res adapter.HeadlessResult) ([]adapter.Artifact, error) {
	if req.Kind == adapter.Video {
		return a.sessionFiles(req, res, "videos", "video")
	}
	return a.sessionFiles(req, res, "images", "image")
}

// sessionFiles lists every file in the sub folder of the session's folder
// as kind artifacts; see Artifacts.
func (a Adapter) sessionFiles(req adapter.HeadlessRequest, res adapter.HeadlessResult, sub, kind string) ([]adapter.Artifact, error) {
	id := res.SessionID
	if id == "" {
		return nil, nil
	}
	if err := adapter.ValidSessionID(adapter.Grok, id); err != nil {
		return nil, err
	}
	home := a.Home()
	if !filepath.IsAbs(home) {
		return nil, fmt.Errorf("grok: cannot locate the Grok home")
	}
	sessions := filepath.Join(home, "sessions")
	folder := func(cwdDir string) string { return filepath.Join(sessions, cwdDir, id, sub) }
	if req.Cwd != "" {
		got, err := adapter.ListFiles(folder(EncodeCwd(req.Cwd)), kind)
		if err != nil || len(got) > 0 {
			return got, err
		}
	}
	dirs, err := adapter.SubDirs(sessions)
	if err != nil {
		return nil, err
	}
	var found []adapter.Artifact
	matches := 0
	for _, d := range dirs {
		got, err := adapter.ListFiles(folder(d), kind)
		if err != nil {
			return nil, err
		}
		if len(got) > 0 {
			matches++
			found = got
		}
	}
	if matches > 1 {
		return nil, fmt.Errorf("grok: session %s has %s under %d working dirs", id, sub, matches)
	}
	return found, nil
}

// TranscriptTail returns ErrNoTranscript: Grok's transcript format is not
// recorded in docs/adapters.md.
func (Adapter) TranscriptTail(adapter.SessionRef, int) (adapter.Transcript, error) {
	return adapter.Transcript{}, adapter.ErrNoTranscript
}

// ConfigPatches returns no patches yet.
func (Adapter) ConfigPatches(adapter.ConfigContext) ([]patch.Patch, error) { return nil, nil }
