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
	// Caps are the capabilities a probe of this CLI found. Interactive
	// adds --no-alt-screen only when they include probe.CapNoAltScreen:
	// the flag is not verified for Codex.
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
)

// Lockdown is the flag set every read-only borrow child gets, live-verified
// on codex-cli 0.164.0 (2026-10-28): the user's config (and so their MCP
// servers) is ignored, the sandbox is read-only, and the shell, exec,
// image-view, apps, plugins, goals, image-generation, and sleep tools are
// off. Without --disable apps --disable plugins, 52 ChatGPT connector
// tools load even under --ignore-user-config. The image kind keeps
// image_generation (see lockdown). --strict-config (help-verified on
// codex exec and exec resume) makes an unknown key an error, and --disable
// <feature> is a key (features.<feature>=false), so a feature a newer
// Codex renamed fails the borrow instead of silently staying on.
var Lockdown = []string{
	"--strict-config", "--ignore-user-config", "--sandbox", "read-only",
	"--disable", "shell_tool", "--disable", "unified_exec", "--disable", "view_image",
	"--disable", "apps", "--disable", "plugins", "--disable", "goals",
	"--disable", "image_generation", "--disable", "sleep_tool",
}

// SkipGitRepoCheck lets codex exec run in a dir that is not a git repo,
// such as a borrow's empty temp dir.
const SkipGitRepoCheck = "--skip-git-repo-check"

// lockdown returns Lockdown for kind k: image keeps image_generation.
func lockdown(k adapter.Kind) []string {
	if k != adapter.Image {
		return slices.Clone(Lockdown)
	}
	var out []string
	for i := 0; i < len(Lockdown); i++ {
		if Lockdown[i] == "--disable" && Lockdown[i+1] == "image_generation" {
			i++
			continue
		}
		out = append(out, Lockdown[i])
	}
	return out
}

// EditLockdown is the flag set of an edit child: Lockdown's config
// isolation (--strict-config, --ignore-user-config, so no user MCP server,
// writable_roots, or network_access from $CODEX_HOME/config.toml applies,
// and apps, plugins, goals, image generation, view_image, and sleep off)
// with --sandbox SandboxEdit, and the shell kept: with only apply_patch the
// child could not read the files it edits. Live-verified on codex-cli
// 0.164.0 (2026-10-28, verification.md "Codex edit lockdown"): a write in
// the cwd works, one outside it is refused. workspace-write alone leaves
// /tmp and $TMPDIR writable; the two sandbox_workspace_write exclude keys
// close them (live, codex-cli 0.164.0, 2026-10-28, verification.md
// "Codex edit lockdown": --strict-config accepts both, and with them a
// shell write to /private/tmp or $TMPDIR fails with "operation not
// permitted" while one in the repo works). Unverified: whether exec's
// default web search is on for an edit child (no recorded flag turns it
// off), and whether Codex treats a linked worktree as its main repo for
// project trust; with the user config ignored, no trust entry is loaded
// either way.
var EditLockdown = editLockdown()

func editLockdown() []string {
	var out []string
	for i := 0; i < len(Lockdown); i++ {
		switch {
		case Lockdown[i] == "--sandbox":
			out = append(out, "--sandbox", SandboxEdit, "-c", "sandbox_workspace_write.exclude_slash_tmp=true",
				"-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true")
			i++
		case Lockdown[i] == "--disable" && (Lockdown[i+1] == "shell_tool" || Lockdown[i+1] == "unified_exec"):
			i++
		default:
			out = append(out, Lockdown[i])
		}
	}
	return out
}

// Refusal returns the one refusal of research and news on Codex, which
// the adapter and internal/router both give, or nil for other kinds.
//
// TODO(T6638): Codex's apply_patch tool cannot be turned off, and its
// error text differs for a right and a wrong guess of a line in a file at
// a known path, so a research child can confirm file contents by guessing
// (docs/verification.md). Research and news stay refused until that
// oracle is closed.
func Refusal(k adapter.Kind) error {
	if k == adapter.Research || k == adapter.News {
		return fmt.Errorf("codex: %s: %w until T6638 (its apply_patch tool can confirm guessed file contents)", k, adapter.ErrUnsupported)
	}
	return nil
}

// SandboxEdit is the sandbox of an edit borrow, which writes its own
// worktree. Confirmed live (verification.md, "Codex resume with sandbox").
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
// its output stays in the terminal's scrollback. The flag is not verified
// for Codex (docs/verification.md), so it is passed only when the probe found
// it in help (Caps has probe.CapNoAltScreen): codex for a new session,
// codex resume <id> to reopen one. The id passes
// adapter.ValidSessionID, so it cannot be read as a flag. Codex cannot be
// given a session id in advance and fork is not recorded, so both are
// ErrUnsupported. A brief is passed as a short first message pointing to
// the file; that Codex takes a prompt argument at session start is
// unconfirmed (T6619).
func (a Adapter) Interactive(req adapter.InteractiveRequest) (adapter.Command, error) {
	if req.SessionID != "" {
		return adapter.Command{}, fmt.Errorf("codex: pre-assigned session id: %w", adapter.ErrUnsupported)
	}
	if req.Fork {
		return adapter.Command{}, fmt.Errorf("codex: fork: %w", adapter.ErrUnsupported)
	}
	var args []string
	if req.Memory != "" {
		// -c is global, so it goes before resume (docs/verification.md,
		// T6619; utils/cli/src/config_override.rs at rust-v0.164.0).
		args = append(args, "-c", "developer_instructions="+tomlString(req.Memory))
	}
	for _, s := range req.MCP {
		m, err := mcpOverrides(s)
		if err != nil {
			return adapter.Command{}, err
		}
		args = append(args, m...)
	}
	if req.Resume != "" {
		if err := adapter.ValidSessionID(adapter.Codex, req.Resume); err != nil {
			return adapter.Command{}, err
		}
		args = append(args, "resume")
	}
	if slices.Contains(a.Caps, probe.CapNoAltScreen) {
		args = append(args, "--no-alt-screen")
	}
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
//	codex exec [-i <ref>]... --json [-c model=...] <Lockdown> [--skip-git-repo-check] [resume <id>] -- -
//
// The exec flags all go before the resume subcommand, which takes only its
// own flags. -i takes one or more values, so the references come first and
// --json ends them. The prompt argument is "-" after "--", and stdin is
// the prompt file: codex exec reads a non-terminal stdin to EOF, and a
// regular file reaches EOF, so it never waits as it would on an open pipe.
//
// Image and review get the Lockdown flag set (image keeps image
// generation); --skip-git-repo-check is added unless GitRepo. Edit runs
// with EditLockdown (M3). Research and news are ErrUnsupported
// (Refusal; TODO(T6638)), as are video, AllowedTools (no recorded
// tool allowlist), and a pre-assigned SessionID. MaxTurns is ignored:
// codex exec has no turn limit. Codex hooks need hash trust and hooks
// passed with -c do not run untrusted, so GuardCommand is not wired:
// TODO(M2 hooks).
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
	case Refusal(req.Kind) != nil:
		return adapter.Command{}, Refusal(req.Kind)
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
	case adapter.Image, adapter.Review:
		args = append(args, lockdown(req.Kind)...)
		if !req.GitRepo {
			args = append(args, SkipGitRepoCheck)
		}
	case adapter.Edit:
		args = append(args, EditLockdown...)
	default:
		return adapter.Command{}, fmt.Errorf("codex: unknown kind %q", req.Kind)
	}
	// An edit child's tools (T6709) as -c overrides, which
	// --ignore-user-config leaves in force, each with its own approval:
	// exec refuses an MCP call otherwise (docs/verification.md,
	// "Launch-time MCP"). Before resume, like every exec flag. A review
	// child stays read-only: no tool says it only reads.
	if len(req.MCP) > 0 && req.Kind != adapter.Edit {
		return adapter.Command{}, fmt.Errorf("codex: tools for a %s child: %w", req.Kind, adapter.ErrUnsupported)
	}
	for _, s := range req.MCP {
		m, err := mcpOverrides(s)
		if err != nil {
			return adapter.Command{}, err
		}
		args = append(args, m...)
		args = append(args, "-c", "mcp_servers."+s.Name+".default_tools_approval_mode="+tomlString("approve"))
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

// ParseHeadless first fails closed when the CLI rejected a lockdown flag
// (ErrLockdown). It then reads the JSON event stream: the session id from the
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
	if err := adapter.CheckLockdown(adapter.Codex, out); err != nil {
		res.Error, res.ErrorDetail = adapter.ErrLockdown, strings.TrimSpace(string(out.Stderr))
		return res, err
	}
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

// Artifacts lists every file in generated_images/<thread-id>/ under the
// Codex home, not only the *.png files recorded there: one request can
// produce several (Codex made two for one request in testing and
// reported one), and the image tool call is not in the JSON stream. The
// borrow's artifact gate then refuses any that is not a real image. It
// returns paths only and copies nothing. An id that fails
// adapter.ValidSessionID is an error.
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
	return adapter.ListFiles(filepath.Join(home, "generated_images", res.SessionID), "image")
}

// TranscriptTail returns ErrNoTranscript: the format of Codex's session
// files is not recorded in docs/adapters.md.
func (Adapter) TranscriptTail(adapter.SessionRef, int) (adapter.Transcript, error) {
	return adapter.Transcript{}, adapter.ErrNoTranscript
}

// ConfigPatches returns no patches here. Codex's CLAUDE.md fallback (the
// project_doc_fallback_filenames patch) is built in internal/parity.
func (Adapter) ConfigPatches(adapter.ConfigContext) ([]patch.Patch, error) { return nil, nil }

// tomlString quotes s as a TOML basic string, for a Codex -c value, which
// Codex parses as TOML (utils/cli/src/config_override.rs at
// rust-v0.164.0).
// mcpOverrides is one stdio MCP server as -c mcp_servers.<name>.* overrides
// (docs/verification.md, T6734: the key path is live-confirmed; command,
// args, cwd, and env_vars are RawMcpServerConfig's fields,
// config/src/mcp_types.rs#L373-L389 at 6aaac69). No approval mode is
// set: the person approves its calls as Codex asks.
func mcpOverrides(s adapter.MCPServer) ([]string, error) {
	if s.Name == "" || strings.Trim(s.Name, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return nil, fmt.Errorf("codex: MCP server name %q is not a bare key", s.Name)
	}
	if s.Command == "" {
		return nil, fmt.Errorf("codex: MCP server %s has no command", s.Name)
	}
	k := "mcp_servers." + s.Name + "."
	out := []string{"-c", k + "command=" + tomlString(s.Command), "-c", k + "args=" + tomlArray(s.Args)}
	if s.Cwd != "" {
		if err := adapter.CheckAbs("MCP server dir", s.Cwd); err != nil {
			return nil, err
		}
		out = append(out, "-c", k+"cwd="+tomlString(s.Cwd))
	}
	if len(s.EnvVars) > 0 {
		out = append(out, "-c", k+"env_vars="+tomlArray(s.EnvVars))
	}
	return out, nil
}

func tomlArray(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = tomlString(x)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
