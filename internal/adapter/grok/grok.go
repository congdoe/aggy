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

// Tested is the Grok Build CLI version range this adapter was tested on: the
// version the facts in docs/adapters.md were verified with.
var Tested = adapter.VersionRange{Min: "1.0.70", Max: "1.0.70"}

// TestedVersions returns Tested.
func (Adapter) TestedVersions() adapter.VersionRange { return Tested }

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

// Tool allowlists per kind, passed with --tools. The names come from the
// verified runs in docs/adapters.md: the news run kept web_search,
// x_keyword_search, and x_semantic_search, image generation used
// image_gen, and the deny list named the file and shell tools.
var (
	// SearchTools is research and news: search only, no file, shell, or
	// fetch tool. The verified isolation run used --disallowed-tools; that
	// --tools alone is an allowlist that removes every other tool is
	// unconfirmed (it did drop the bundled imagine skill in the image run).
	SearchTools = []string{"web_search", "x_keyword_search", "x_semantic_search"}
	// ImageTools is image generation. image_edit, added on resume, is
	// unverified headless.
	ImageTools = []string{"image_gen"}
	// VideoTools is video generation, unverified headless.
	VideoTools = []string{"video_gen"}
	// ReadTools is review, which also runs with --permission-mode plan.
	ReadTools = []string{"read_file", "grep", "list_dir"}
	// EditTools is edit, in its own worktree.
	EditTools = []string{"read_file", "grep", "list_dir", "write", "search_replace", "run_terminal_cmd"}
	// isolationDenied may never be added to research or news: the verified
	// isolation deny list.
	isolationDenied = []string{"read_file", "grep", "list_dir", "write", "search_replace", "run_terminal_cmd", "web_fetch", "Agent"}
	// writeTools may not be added to a read-only kind.
	writeTools = []string{"write", "search_replace", "run_terminal_cmd"}
)

// modelArgs passes a model through unchanged. --model is not recorded in
// docs/adapters.md (unconfirmed); an empty model adds nothing.
func modelArgs(model string) []string {
	if model == "" {
		return nil
	}
	return []string{"--model", model}
}

// sessionArgs is as on Claude: --session-id <uuid>, --resume <id>, and
// --fork-session.
func sessionArgs(sessionID, resume string, fork bool) ([]string, error) {
	if sessionID != "" && !adapter.IsUUID(sessionID) {
		return nil, fmt.Errorf("grok: --session-id takes a UUID, got %q", sessionID)
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
	args = append(args, modelArgs(req.Model)...)
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

// Headless builds grok -p --output-format json with the prompt file as
// stdin (that grok -p reads its prompt from stdin is unconfirmed). Tools
// are an allowlist per kind (see SearchTools and the others) plus
// AllowedTools, and WebSearch adds web_search. Research and news refuse
// any file, shell, or fetch tool; other read-only kinds refuse write
// tools. Review runs with --permission-mode plan. MaxTurns sets
// --max-turns. RefImages are ErrUnsupported (no recorded flag).
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
	extra := slices.Clone(req.AllowedTools)
	if req.WebSearch {
		extra = append(extra, "web_search")
	}
	for _, t := range extra {
		switch {
		case t == "" || strings.ContainsAny(t, ", \t\n"):
			return adapter.Command{}, fmt.Errorf("grok: bad tool name %q", t)
		case (req.Kind == adapter.Research || req.Kind == adapter.News) && slices.Contains(isolationDenied, t):
			return adapter.Command{}, fmt.Errorf("grok: %s may not use %s: research and news stay isolated", req.Kind, t)
		case req.Kind.ReadOnly() && slices.Contains(writeTools, t):
			return adapter.Command{}, fmt.Errorf("grok: %s is read-only and may not use %s", req.Kind, t)
		}
		if !slices.Contains(tools, t) {
			tools = append(tools, t)
		}
	}
	args := []string{"-p", "--output-format", "json"}
	args = append(args, modelArgs(req.Model)...)
	s, err := sessionArgs(req.SessionID, req.Resume, false)
	if err != nil {
		return adapter.Command{}, err
	}
	args = append(args, s...)
	if plan {
		args = append(args, "--permission-mode", "plan")
	}
	args = append(args, "--tools", strings.Join(tools, ","))
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

// ParseHeadless reads the JSON reply: sessionId (recorded), the final text
// from the first of TextFields present, and the error class from an
// "error" field or the stderr text. Grok's classes mirror Claude Code's,
// so rate_limit maps to rate_limit.
func (Adapter) ParseHeadless(req adapter.HeadlessRequest, out adapter.Output) (adapter.HeadlessResult, error) {
	res := adapter.HeadlessResult{SessionID: req.SessionID}
	if res.SessionID == "" {
		res.SessionID = req.Resume
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
	if parsed {
		if id, ok := obj["sessionId"].(string); ok && id != "" {
			res.SessionID = id
		}
		for _, f := range TextFields {
			if s, ok := obj[f].(string); ok {
				res.Text = s
				break
			}
		}
		errName, _ = obj["error"].(string)
		isError, _ = obj["is_error"].(bool)
	}
	if out.ExitCode == 0 && errName == "" && !isError && parsed {
		return res, nil
	}
	stderr := string(out.Stderr)
	switch {
	case errName != "":
		res.Error = adapter.ClassifyName(errName)
	case strings.Contains(stderr, "rate_limit"):
		res.Error = adapter.ErrRateLimit
	default:
		res.Error = adapter.ErrUnknown
	}
	res.ErrorDetail = strings.TrimSpace(errName + " " + stderr)
	if !parsed && out.ExitCode == 0 {
		return res, fmt.Errorf("grok: %w", adapter.ErrUnparsed)
	}
	return res, nil
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

// Artifacts lists the images in sessions/<encoded cwd>/<sessionId>/images/
// under the Grok home. When that folder is missing (the cwd encoding is
// unconfirmed, or the CLI saw the cwd with symlinks resolved), it falls
// back to the one sessions/*/<sessionId>/images folder; several matches are
// an error, not a guess. It returns paths only and copies nothing.
func (a Adapter) Artifacts(req adapter.HeadlessRequest, res adapter.HeadlessResult) ([]adapter.Artifact, error) {
	id := res.SessionID
	if id == "" || id != filepath.Base(id) || id == "." || id == ".." {
		return nil, nil
	}
	home := a.Home()
	if !filepath.IsAbs(home) {
		return nil, fmt.Errorf("grok: cannot locate the Grok home")
	}
	sessions := filepath.Join(home, "sessions")
	images := func(cwdDir string) string { return filepath.Join(sessions, cwdDir, id, "images") }
	if req.Cwd != "" {
		got, err := adapter.ListFiles(images(EncodeCwd(req.Cwd)), "image", ".jpg")
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
		got, err := adapter.ListFiles(images(d), "image", ".jpg")
		if err != nil {
			return nil, err
		}
		if len(got) > 0 {
			matches++
			found = got
		}
	}
	if matches > 1 {
		return nil, fmt.Errorf("grok: session %s has images under %d working dirs", id, matches)
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
