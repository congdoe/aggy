package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/probe"
)

// Bin is the CLI's program name.
const Bin = "opencode"

// Adapter is the OpenCode adapter. The zero value is ready to use.
type Adapter struct {
	// Path is the CLI's path for Probe. Empty looks it up on PATH.
	Path string
}

var _ adapter.Adapter = Adapter{}

// New returns an OpenCode adapter.
func New() Adapter { return Adapter{} }

// Vendor returns adapter.OpenCode.
func (Adapter) Vendor() adapter.Vendor { return adapter.OpenCode }

// Tested is the OpenCode version the facts in docs/adapters.md were
// verified with.
var Tested = adapter.VersionRange{Min: "2.2.5", Max: "2.2.5"}

// LockdownCaps returns nil: OpenCode takes no borrows (Headless refuses), so
// no flag of it stands behind a borrow's lockdown.
func (Adapter) LockdownCaps(adapter.Kind) []string { return nil }

// SelfTestKind returns research, for the interface: with no borrows, the
// new-version self-test never runs on OpenCode.
func (Adapter) SelfTestKind() adapter.Kind { return adapter.Research }

// PassFlags is `aggy opencode`'s table. It passes none of OpenCode's own flags
// yet, so runLaunch and Interactive refuse any Extra: aggy writes the session's
// whole config per launch (docs/design.md, section 15), and no flag of
// OpenCode's has been checked against it. --version, which the probe runs,
// goes to the CLI as given (Bare).
func (Adapter) PassFlags() adapter.PassFlags {
	return adapter.PassFlags{
		Model:    []string{"--model"},
		Continue: []string{"--continue"},
		Resume:   []string{"--resume"},
		Bare:     []string{"--version"},
		Refused: []adapter.RefusedFlag{
			{Names: []string{"--session", "-s"}, Why: "aggy gives each session its id"},
			{Names: []string{"--standalone"}, Why: "aggy always passes it"},
			{Names: []string{"--auto"}, Why: "full auto is the [opencode] auto setting, in a repo trusted for OpenCode"},
		},
	}
}

// TestedVersions returns Tested.
func (Adapter) TestedVersions() adapter.VersionRange { return Tested }

// SignIn returns OpenCode's own sign-in step. Its `auth list` prints a
// list with no documented format, so there is no check and the status is
// unknown; aggy never runs a login.
func (Adapter) SignIn() adapter.SignIn {
	return adapter.SignIn{Steps: "run `opencode auth login`, or /connect on its screen"}
}

// Probe finds the CLI's version and which recorded flags its help lists.
func (a Adapter) Probe(ctx context.Context, run adapter.Runner) (adapter.Probe, error) {
	path := a.Path
	if path == "" {
		p, err := probe.LookPath(Bin)
		if err != nil {
			return adapter.Probe{Vendor: adapter.OpenCode}, err
		}
		path = p
	}
	return probe.Detect(ctx, run, adapter.OpenCode, path)
}

// Reachable says why aggy's launch cannot reach this CLI, given its
// probed capabilities, or "". Without --standalone a running service
// keeps the config it started with, so no per-launch rule would apply.
func Reachable(caps []string, mini bool) string {
	switch {
	case !slices.Contains(caps, probe.CapStandalone):
		return "opencode --help does not list --standalone"
	case mini && !slices.Contains(caps, probe.CapMiniSession):
		return "opencode mini --help does not list --session"
	case !mini && !slices.Contains(caps, probe.CapSession):
		return "opencode --help does not list --session"
	}
	return ""
}

// NewSessionID returns the id for a new session, made from aggy's random
// uuid: `--session <new id>` sets a new session's id (live, T6930).
func NewSessionID(uuid string) string {
	return "ses_" + strings.ReplaceAll(uuid, "-", "")
}

// launchEnv are the variables of every on-screen launch (docs/design.md,
// section 15; docs/verification.md, "Repo config and safety" and "Exact
// rules in permissions"). execrun's PTYEnv removes inherited OPENCODE_*
// first, since a stale OPENCODE_CONFIG_PROJECT_DISABLE=0 beats the
// variable here. mini and run take their location from PWD, so the launch
// must set it. The git variables stop a repo's core.fsmonitor running a
// script from an allowed `git status`.
func launchEnv(cwd string, l *adapter.OpenCodeLaunch) []string {
	return []string{
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_CONFIG=" + l.ConfigFile,
		"OPENCODE_CONFIG_DIR=" + l.ConfigDir,
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"PWD=" + cwd,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.fsmonitor",
		"GIT_CONFIG_VALUE_0=false",
	}
}

// Interactive builds
//
//	opencode --standalone --session <id> [--auto]
//	opencode mini --standalone --session <id> [-m <provider/model>]
//
// with the launch's variables. A new session and a resume are the same
// command: --session continues the id, or creates it at the first prompt
// (cli/src/commands/commands.ts at v2.2.5). The full screen has no model
// flag, so its model is `model` in the launch's config, which a resumed
// session ignores for its own. A brief and the memory are in the config's
// instructions too, so req.BriefFile and req.Memory are not read here.
func (a Adapter) Interactive(req adapter.InteractiveRequest) (adapter.Command, error) {
	if len(req.Extra) > 0 {
		return adapter.Command{}, errors.New("opencode: aggy passes none of its other flags yet")
	}
	l := req.OpenCode
	if l == nil {
		return adapter.Command{}, errors.New("opencode: no launch config, so aggy's rules would not reach it")
	}
	if err := adapter.CheckAbs("config file", l.ConfigFile); err != nil {
		return adapter.Command{}, err
	}
	if err := adapter.CheckAbs("config folder", l.ConfigDir); err != nil {
		return adapter.Command{}, err
	}
	if err := adapter.CheckAbs("working directory", req.Cwd); err != nil {
		return adapter.Command{}, err
	}
	if req.Fork {
		return adapter.Command{}, fmt.Errorf("opencode: forking a session: %w", adapter.ErrUnsupported)
	}
	id := req.Resume
	if id == "" {
		id = req.SessionID
	}
	if err := adapter.ValidSessionID(adapter.OpenCode, id); err != nil {
		return adapter.Command{}, err
	}
	if err := adapter.ValidModel(req.Model); err != nil {
		return adapter.Command{}, err
	}
	var args []string
	switch {
	case l.Mini && l.Auto:
		return adapter.Command{}, fmt.Errorf("opencode mini has no --auto: %w", adapter.ErrUnsupported)
	case l.Mini:
		args = []string{"mini", "--standalone", "--session", id}
		if req.Model != "" {
			args = append(args, "-m", req.Model)
		}
	default:
		args = []string{"--standalone", "--session", id}
		if l.Auto {
			args = append(args, "--auto")
		}
	}
	env := append(slices.Clone(req.Env), launchEnv(req.Cwd, l)...)
	return adapter.Command{Path: Bin, Args: args, Env: env, Dir: req.Cwd, Stdin: adapter.StdinTerminal}, nil
}

// Headless refuses: no open harness is routed to until its lockdown
// passes the same read test as the others (docs/design.md, section 15).
func (Adapter) Headless(adapter.HeadlessRequest) (adapter.Command, error) {
	return adapter.Command{}, fmt.Errorf("opencode runs on screen only: %w", adapter.ErrUnsupported)
}

// ParseHeadless refuses: there is no headless command to read.
func (Adapter) ParseHeadless(adapter.HeadlessRequest, adapter.Output) (adapter.HeadlessResult, error) {
	return adapter.HeadlessResult{Error: adapter.ErrUnknown}, adapter.ErrUnsupported
}

// Artifacts lists none: there are no headless runs.
func (Adapter) Artifacts(adapter.HeadlessRequest, adapter.HeadlessResult) ([]adapter.Artifact, error) {
	return nil, nil
}

// TranscriptTail has no process to read one with: internal/cli runs
// ExportCommand and hands the output to ParseExport.
func (Adapter) TranscriptTail(adapter.SessionRef, int) (adapter.Transcript, error) {
	return adapter.Transcript{}, adapter.ErrNoTranscript
}

// ConfigPatches returns none: every launch rebuilds its own config, and
// the person's OpenCode config is never edited.
func (Adapter) ConfigPatches(adapter.ConfigContext) ([]patch.Patch, error) { return nil, nil }

// ExportCommand builds `opencode session export <id> --standalone`, the
// only way aggy reads a session: sessions are in opencode.db with the
// sign-ins, which aggy never opens. It calls an experimental API (live,
// T6930). configDir is an empty folder aggy owns, so the global config's
// plugins do not load either.
func ExportCommand(path, id, cwd, configDir string) (adapter.Command, error) {
	if err := adapter.ValidSessionID(adapter.OpenCode, id); err != nil {
		return adapter.Command{}, err
	}
	if err := adapter.CheckAbs("config folder", configDir); err != nil {
		return adapter.Command{}, err
	}
	if err := adapter.CheckAbs("working directory", cwd); err != nil {
		return adapter.Command{}, err
	}
	return adapter.Command{
		Path: path,
		Args: []string{"session", "export", id, "--standalone"},
		Dir:  cwd,
		Env: []string{
			"OPENCODE_DISABLE_PROJECT_CONFIG=1",
			"OPENCODE_CONFIG_DIR=" + configDir,
			"OPENCODE_DISABLE_AUTOUPDATE=1",
			"PWD=" + cwd,
		},
	}, nil
}

// export is the part of `session export`'s JSON aggy reads (live, T6930).
type export struct {
	Info     *struct{} `json:"info"`
	Messages []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Time struct {
			Created int64 `json:"created"`
		} `json:"time"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"messages"`
}

// ParseExport reads up to n of the latest turns (all when n <= 0) from
// `session export`'s output: user messages, and the text of assistant
// messages. Reasoning, tool calls, and other entries are left out. A shape
// it does not recognize is ErrNoTranscript, never a guess, and the brief
// then goes on without it. Path is empty: a brief points at no file.
func ParseExport(out []byte, n int) (adapter.Transcript, error) {
	var e export
	if err := json.Unmarshal(out, &e); err != nil || e.Info == nil || e.Messages == nil {
		return adapter.Transcript{}, fmt.Errorf("%w: opencode session export format not recognized", adapter.ErrNoTranscript)
	}
	var turns []adapter.Turn
	for _, m := range e.Messages {
		t := adapter.Turn{Time: time.UnixMilli(m.Time.Created).UTC()}
		switch m.Type {
		case "user":
			t.Role, t.Text = "user", m.Text
		case "assistant":
			var parts []string
			for _, c := range m.Content {
				if c.Type == "text" && c.Text != "" {
					parts = append(parts, c.Text)
				}
			}
			t.Role, t.Text = "assistant", strings.Join(parts, "\n")
		}
		if t.Role != "" && t.Text != "" {
			turns = append(turns, t)
		}
	}
	if n > 0 && len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	return adapter.Transcript{Turns: turns}, nil
}
