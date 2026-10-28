package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
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
	// Caps are the capabilities a probe of this CLI found, plus
	// probe.CapKernelSandbox when the host can apply Grok's sandbox.
	// Review, research, and news need both and are refused without them.
	Caps []string
	// GOOS picks the OS-specific deny rules. Empty means runtime.GOOS.
	GOOS string
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
	// ImageTools is image generation. image_gen takes only a prompt and
	// an aspect ratio (grok-build 2bdd1d6a, image_gen/mod.rs ImageGenInput).
	ImageTools = []string{"image_gen"}
	// That image_gen alone, with these denied, leaves the child no tool
	// that reads a local path is checked live by test/smoke/smoke.sh
	// (grok-image-tool-list); not yet recorded in docs/verification.md.
	//
	// FileTools read a local file at any absolute path the model names
	// (tokio::fs::read, no cwd check) and upload it to xAI: image_edit
	// (image_edit/mod.rs resolve_to_data_url), image_to_video and
	// reference_to_video (video_gen/mod.rs resolve_image_reference). They
	// are off unless HeadlessRequest.FileTools, the user's opt-in.
	FileTools = []string{"image_edit", "image_to_video", "reference_to_video"}
	// VideoTools is video generation, behind FileTools: Grok has no
	// text-to-video tool, so the child makes a still with image_gen and
	// animates it with image_to_video. Unverified headless.
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
	// recorded help) is unverified and not needed, so it is not passed.
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
// AllowedTools, nothing else. The FileTools need fileTools.
func permitted(k adapter.Kind, fileTools bool) []string {
	switch k {
	case adapter.Research, adapter.News:
		return SearchTools
	case adapter.Image:
		if fileTools {
			return append(slices.Clone(ImageTools), "image_edit")
		}
		return ImageTools
	case adapter.Video:
		if fileTools {
			return VideoTools
		}
		return nil
	case adapter.Review:
		return ReadTools
	case adapter.Edit:
		return EditTools
	}
	return nil
}

// canonical returns the permitted tool named t, compared
// case-insensitively, or false.
func canonical(k adapter.Kind, fileTools bool, t string) (string, bool) {
	for _, p := range permitted(k, fileTools) {
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
	if req.Memory != "" {
		// Inline text only; there is no file variant
		// (docs/verification.md, T6619).
		args = append(args, "--rules", req.Memory)
	}
	for _, r := range req.Deny {
		// --deny splits its value on commas (src/app/cli.rs at 2bdd1d6a).
		if r == "" || strings.ContainsAny(r, ",\x00") {
			return adapter.Command{}, fmt.Errorf("grok: deny rule %q cannot be passed as one --deny value", r)
		}
		args = append(args, "--deny", r)
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
//	grok --prompt-file <abs prompt file> --output-format json [--model m] [session] [--permission-mode plan]
//	     [--sandbox strict --deny Read(<HOME>/Library/**) --deny Grep(<HOME>/Library/**)] --tools <kind's tools> --disallowed-tools <Lockdown minus them> --no-subagents [--max-turns N]
//
// with no stdin, so the prompt never goes in argv. Not -p: in grok 1.0.70
// -p is --single <PROMPT>, which takes the prompt itself as its value
// (live diagnostic 2026-10-28, grok 1.0.70; docs/verification.md).
//
// Review, research, and news run with --sandbox strict: Grok auto-allows
// file reads in every permission mode, and the strict sandbox kept
// read_file out of $HOME in a live check (live diagnostic 2026-10-28,
// grok 1.0.70; grok-src profiles.rs). When applying the profile failed
// (nested in another Seatbelt sandbox), grok 1.0.70 refused to start,
// exit 1 (live, 2026-10-28); the source also has paths that log
// "continuing without sandbox" and run on (xai-grok-sandbox/src/lib.rs:
// 206-216, :236-247). So these kinds are ErrNoCapability unless Caps
// holds both probe.CapSandbox (help lists --sandbox) and
// probe.CapKernelSandbox (the host has Seatbelt or Landlock), and
// ParseHeadless fails closed on Grok's refusal and, as a weak extra
// guard, on those log lines (sandboxErr). Strict still allows reads of
// ~/Library, so they also deny Read and Grep there, and on darwin under
// the /System/Volumes/Data firmlink (sandboxDeny). AllowedTools may only name tools the
// kind already permits (compared case-insensitively), and never the MCP
// meta-tools. Review runs with --permission-mode plan, MaxTurns sets
// --max-turns, and research and news need IsolatedCwd. Web search outside
// research and news, and RefImages (no recorded flag), are ErrUnsupported.
// Grok takes no per-run hook flag (it does load Claude's settings.json
// hooks by default), so GuardCommand is not wired: TODO(M2 hooks).
func (a Adapter) Headless(req adapter.HeadlessRequest) (adapter.Command, error) {
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
	plan, sandbox := false, false
	switch req.Kind {
	case adapter.Research, adapter.News:
		tools, sandbox = slices.Clone(SearchTools), true
	case adapter.Image:
		// TODO(T6742): --sandbox strict for image and video, once an
		// output file write under it is checked live.
		tools = slices.Clone(ImageTools)
		if req.Resume != "" {
			// An edit needs image_edit, which reads any local path.
			if !req.FileTools {
				return adapter.Command{}, fmt.Errorf("grok: an image edit (image_edit) %w", adapter.ErrFileTools)
			}
			tools = append(tools, "image_edit")
		}
	case adapter.Video:
		if !req.FileTools {
			return adapter.Command{}, fmt.Errorf("grok: video (image_to_video) %w", adapter.ErrFileTools)
		}
		tools = slices.Clone(VideoTools)
	case adapter.Review:
		tools, plan, sandbox = slices.Clone(ReadTools), true, true
	case adapter.Edit:
		tools = slices.Clone(EditTools)
	default:
		return adapter.Command{}, fmt.Errorf("grok: unknown kind %q", req.Kind)
	}
	var denyRules []string
	if sandbox {
		switch {
		case !slices.Contains(a.Caps, probe.CapSandbox):
			return adapter.Command{}, fmt.Errorf("grok: %s runs only under --sandbox strict, which this grok's --help does not list: %w", req.Kind, adapter.ErrNoCapability)
		case !slices.Contains(a.Caps, probe.CapKernelSandbox):
			return adapter.Command{}, fmt.Errorf("grok: %s runs only under Grok's kernel sandbox, which this host cannot apply (macOS, or Linux with Landlock in /sys/kernel/security/lsm): %w", req.Kind, adapter.ErrNoCapability)
		}
		var err error
		if denyRules, err = a.sandboxDeny(); err != nil {
			return adapter.Command{}, err
		}
	}
	for _, t := range req.AllowedTools {
		c, ok := canonical(req.Kind, req.FileTools, t)
		if !ok || slices.Contains(alwaysDenied, c) {
			return adapter.Command{}, fmt.Errorf("grok: %s may use only %s, not %q", req.Kind, strings.Join(permitted(req.Kind, req.FileTools), ", "), t)
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
	args := []string{"--prompt-file", req.PromptFile, "--output-format", "json"}
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
	if sandbox {
		args = append(args, "--sandbox", "strict")
		for _, r := range denyRules {
			args = append(args, "--deny", r)
		}
	}
	args = append(args, "--tools", strings.Join(tools, ","), "--disallowed-tools", strings.Join(deny, ","), NoSubagents)
	if req.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	return adapter.Command{Path: Bin, Args: args, Env: req.Env, Dir: req.Cwd, Stdin: adapter.StdinNull}, nil
}

// usesSandbox reports whether Headless runs kind under --sandbox strict.
func usesSandbox(k adapter.Kind) bool {
	return k == adapter.Review || k == adapter.Research || k == adapter.News
}

// globMeta are the characters a --deny path may not hold: glob syntax,
// the rule's own parentheses, the comma --deny splits on, and escapes.
const globMeta = "*?[]{}()!,\\\x00\n\r"

// sandboxDeny returns the deny rules a sandboxed kind adds: Read and Grep
// of <HOME>/Library/**, which the strict profile leaves readable (browser
// cookies, app tokens), and on darwin of /System/Volumes/Data/**, the
// firmlink spelling of the same files. A deny outranks any allow (decision_rank,
// xai-grok-permission-rules/src/policy.rs:791 at grok-build 2bdd1d6a);
// live-verified to refuse read_file there (2026-10-28, grok 1.0.70). HOME
// must be a clean absolute path with no glob or rule syntax, else the
// kind is refused.
func (a Adapter) sandboxDeny() ([]string, error) {
	getenv := a.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	home := getenv("HOME")
	if err := adapter.CheckAbs("HOME", home); err != nil || home == "/" || strings.ContainsAny(home, globMeta) {
		return nil, fmt.Errorf("grok: HOME %q cannot be named in a --deny rule, so ~/Library cannot be denied: %w", home, adapter.ErrNoCapability)
	}
	rules := []string{"Read(" + home + "/Library/**)", "Grep(" + home + "/Library/**)"}
	goos := a.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos == "darwin" {
		// Grok matches deny rules on the lexical path, and a firmlink is
		// not a symlink: /System/Volumes/Data<HOME>/Library/... read the
		// canary past the rule above, live (G32-9, 2026-10-28, grok
		// 1.0.70). Nothing aggy names lives under that spelling.
		rules = append(rules, "Read(/System/Volumes/Data/**)", "Grep(/System/Volumes/Data/**)")
	}
	return rules, nil
}

// TextFields are the reply fields read for the final text, in order.
// grok 1.0.70 puts the answer in text (live 2026-10-28); result and
// response are unconfirmed fallbacks. thought, the model's reasoning, is
// never the text.
var TextFields = []string{"text", "result", "response"}

// ParseHeadless first fails closed when the CLI rejected a lockdown flag,
// printed UnknownToolWarning, refused to start for want of its sandbox,
// or (on a sandboxed kind) said it ran without it (ErrLockdown). It then reads the JSON reply: sessionId (recorded; checked with
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
	err := lockdownErr(out)
	if err == nil {
		err = sandboxErr(out, usesSandbox(req.Kind))
	}
	if err != nil {
		res.Error, res.ErrorDetail = adapter.ErrLockdown, strings.TrimSpace(string(out.Stderr))
		return res, err
	}
	obj, parsed := decodeReply(out.Stdout)
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

// decodeReply reads Grok's JSON reply. grok 1.0.70 prints one
// pretty-printed object over many lines (live 2026-10-28), so all of
// stdout is tried first; then the last line that holds a whole object.
func decodeReply(stdout []byte) (map[string]any, bool) {
	var obj map[string]any
	all := bytes.TrimSpace(stdout)
	if len(all) > 0 && all[0] == '{' && json.Unmarshal(all, &obj) == nil {
		return obj, true
	}
	lines := bytes.Split(all, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		l := bytes.TrimSpace(lines[i])
		obj = nil
		if len(l) > 0 && l[0] == '{' && json.Unmarshal(l, &obj) == nil {
			return obj, true
		}
	}
	return nil, false
}

// tokens reads a usage object's input_tokens and output_tokens (or its
// total_tokens), as Claude Code reports them, and as grok 1.0.70 does
// (live 2026-10-28). A reply without one counts 0.
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

// SandboxRefused are the stderr lines of a grok that could not apply the
// sandbox profile it was asked for and refused to start, exit 1: live,
// grok 1.0.70 nested in sandbox-exec (2026-10-28), "sandbox
// initialization failed: Operation not permitted" then "error: could not
// apply the 'strict' sandbox profile; see the warning above for the
// cause. Refusing to start with its protections missing."
var SandboxRefused = []string{"Refusing to start with its protections missing", "could not apply the '"}

// SandboxNotApplied are the messages the source logs when it runs on
// without the sandbox (xai-grok-sandbox/src/lib.rs at grok-build
// 2bdd1d6a): ":209-210" "Sandbox not supported on this platform,
// continuing without sandbox", ":240-241" "Sandbox could not be applied,
// continuing without sandbox", and ":252" "Sandbox enforcement
// unavailable (built without 'enforce' feature)". They are tracing
// events, and headless tracing is off by default (live, grok 1.0.70: a
// sandboxed run printed no sandbox line on stderr, even with RUST_LOG),
// so this is a weak guard: the host check before the run
// (probe.CapKernelSandbox) is the main one.
var SandboxNotApplied = []string{"continuing without sandbox", "Sandbox enforcement unavailable"}

// sandboxErr fails a run closed (ErrLockdownRejected) when Grok refused
// to start because it could not apply its sandbox, or, on a sandboxed
// kind, said it ran without it: that output came from unconfined reads.
func sandboxErr(out adapter.Output, sandboxed bool) error {
	// stderr only: the reply text is the model's, and may quote these.
	for _, line := range strings.Split(string(out.Stderr), "\n") {
		for _, m := range SandboxRefused {
			if strings.Contains(line, m) {
				return fmt.Errorf("grok: %w: Grok could not apply its sandbox and refused to start (%s); this often means aggy itself runs inside another sandbox",
					adapter.ErrLockdownRejected, strings.TrimSpace(line))
			}
		}
		for _, m := range SandboxNotApplied {
			if sandboxed && strings.Contains(line, m) {
				return fmt.Errorf("grok: %w: Grok ran without its sandbox (%s), so the output is discarded",
					adapter.ErrLockdownRejected, strings.TrimSpace(line))
			}
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
