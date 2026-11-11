package antigravity

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/probe"
)

// Sources cited below. LIVE: a check recorded in docs/verification.md
// ("Antigravity CLI"), on agy 1.5.3 (help, version, flags, the trust dialog)
// or agy 1.5.5 (hooks, --settings, exit and resume), macOS, 2026-11-11.
// DOCS: Google's pages at antigravity.google/docs, read 2026-11-11, not run.

// Bin is the CLI's program name (LIVE).
const Bin = "agy"

// Adapter is the Antigravity adapter. The zero value is ready to use.
type Adapter struct {
	// Path is the CLI's path for Probe. Empty looks it up on PATH.
	Path string
}

var _ adapter.Adapter = Adapter{}

// New returns an Antigravity adapter.
func New() Adapter { return Adapter{} }

// Vendor returns adapter.Antigravity.
func (Adapter) Vendor() adapter.Vendor { return adapter.Antigravity }

// Tested is the range the facts in docs/adapters.md were checked on: help,
// version, and the flags on 1.5.3, hooks, --settings, and exit on 1.5.5 (the
// binary updated itself between the two checks). 1.5.4 changed the hook
// payload (DOCS changelog: hookEventName, cwd, toolCall.id).
var Tested = adapter.VersionRange{Min: "1.5.3", Max: "1.5.5"}

// TestedVersions returns Tested.
func (Adapter) TestedVersions() adapter.VersionRange { return Tested }

// LockdownCaps returns nil: Antigravity takes no borrows.
func (Adapter) LockdownCaps(adapter.Kind) []string { return nil }

// SelfTestKind returns research, for the interface: with no borrows, the
// new-version self-test never runs here.
func (Adapter) SelfTestKind() adapter.Kind { return adapter.Research }

// UpdateEnv turns agy's background self-update off (DOCS: the variable is
// listed; LIVE: the binary updated itself without it, and stayed put with it
// set). aggy never sets it: aggy doctor and the untested-version line say how.
const UpdateEnv = "AGY_CLI_DISABLE_AUTO_UPDATE"

// PlanEnv and PlanSumEnv carry the launch's decision plan and its SHA-256 to
// aggy's hook, which agy runs with its own environment (LIVE: a variable set
// on agy reached the hook).
const (
	PlanEnv    = "AGGY_DECIDE_PLAN"
	PlanSumEnv = "AGGY_DECIDE_SHA256"
)

// TermsNote is the one sentence the trust row and a first launch carry
// (owner decision, 2026-11-11; antigravity.google/terms clause 6 and the
// FAQ, read 2026-11-11).
const TermsNote = "Google's terms do not address running agy under another program, its FAQ calls wrapping agy in third-party applications \"not supported under consumer plans\", and the account is your risk"

// printWhy is why every print-mode flag is refused: what the FAQ names.
const printWhy = "aggy runs Antigravity on screen only: Google's FAQ answers a question about headless mode by calling agy wrapped in third-party applications not supported under consumer plans"

// printFlags are agy's print-mode flags (LIVE help, 1.5.3): -p/--print,
// --prompt (an alias of --print), and the flags that only shape a print run.
var printFlags = []string{"-p", "--print", "--prompt", "--output-format", "--input-format", "--json-schema", "--print-timeout"}

// PassFlags is `aggy antigravity`'s table. agy parses its flags with Go's
// flag package (LIVE: "Usage of agy:", and an unknown flag prints "flags
// provided but not defined: -no-such-flag"), so -name and --name are one flag
// and a boolean takes =true or =false: GoFlags reads every word that way.
// aggy takes over --model, -c/--continue, and --conversation, and passes only
// the flags in Only; every other dash-led word is refused.
func (Adapter) PassFlags() adapter.PassFlags {
	return adapter.PassFlags{
		GoFlags:  true,
		Model:    []string{"--model"},
		Continue: []string{"-c", "--continue"},
		Resume:   []string{"--conversation"},
		Bare:     []string{"--version", "--help", "-h"},
		Print:    printFlags,
		Refused: append([]adapter.RefusedFlag{
			{Names: printFlags, Why: printWhy},
			{Names: []string{"--settings"}, Why: "aggy passes its own --settings with the safety policy's denies and asks; put your own agy settings in agy's settings.json"},
			{Names: []string{"--remote-control", "--remote-agent"}, Why: "it runs the session somewhere aggy does not supervise"},
			{Names: []string{"--project", "--new-project", "--agent"}, Why: "it changes what aggy starts and records; start agy itself for it"},
			{Names: []string{"--log-file"}, Why: "agy's log may hold the conversation, and aggy chooses where nothing is written"},
			{Names: []string{"--disable-slash-commands"}, Why: "aggy switches by typing /exit at a turn end"},
		}, loosening...),
		Checked: []adapter.CheckedFlag{
			{Names: []string{"--mode"}, Check: checkMode},
			{Names: []string{"--add-dir"}, Check: func(v string) error {
				if err := adapter.AnyValue(v); err != nil {
					return err
				}
				return fmt.Errorf("--add-dir: %w", adapter.ErrLoosens)
			}},
		},
		DirFlags: []string{"--add-dir"},
		// --effort takes low|medium|high|xhigh|max, -i the first prompt
		// (LIVE help).
		Values: []string{"--effort", "-i", "--prompt-interactive"},
		Once:   []string{"-i", "--prompt-interactive"},
		Noted: []adapter.NotedFlag{{Names: []string{"--sandbox"},
			Note: "agy's own --sandbox is on for this session; what it confines is from Google's docs, not checked under aggy"}},
		// The subcommands agy --help lists (LIVE, 1.5.3): none runs through aggy.
		Commands: []string{"acp", "agent", "agents", "changelog", "help", "install", "mcp", "mic-serve", "models",
			"plugin", "plugins", "remote-control", "update"},
		Owned:  []string{"--settings"},
		Effect: loosenEffect,
		Only: []string{"--effort", "-i", "--prompt-interactive", "--sandbox", "--mode", "--add-dir",
			"--dangerously-skip-permissions", "--version", "--help", "-h"},
	}
}

// checkMode lets --mode plan through (it only tightens) and asks about the
// others (LIVE help: accept-edits, auto, plan). A value aggy does not know
// loosens as far as it can tell.
func checkMode(v string) error {
	if err := adapter.AnyValue(v); err != nil {
		return err
	}
	if v == "plan" {
		return nil
	}
	return fmt.Errorf("--mode %s: %w", v, adapter.ErrLoosens)
}

// SignIn returns agy's own sign-in: aggy signs in to nothing and reads no
// token (the token is in the OS keychain, with an undocumented file fallback,
// DOCS).
func (Adapter) SignIn() adapter.SignIn {
	return adapter.SignIn{Steps: "run agy and sign in with your Google account in its own screen"}
}

// Probe finds the CLI's version and which recorded flags its help lists.
func (a Adapter) Probe(ctx context.Context, run adapter.Runner) (adapter.Probe, error) {
	path := a.Path
	if path == "" {
		p, err := probe.LookPath(Bin)
		if err != nil {
			return adapter.Probe{Vendor: adapter.Antigravity}, err
		}
		path = p
	}
	return probe.Detect(ctx, run, adapter.Antigravity, path)
}

// Reachable says why aggy's launch cannot reach this CLI, given its probed
// capabilities, or "": aggy's policy rides in --settings and aggy resumes
// with --conversation.
func Reachable(caps []string) string {
	for _, c := range []struct{ cap, why string }{
		{probe.CapSettings, "agy --help does not list --settings"},
		{probe.CapResume, "agy --help does not list --conversation"},
	} {
		if !slices.Contains(caps, c.cap) {
			return c.why
		}
	}
	return ""
}

// Interactive builds
//
//	agy --settings <file> [--conversation=<id>] [--model <m>] [extra...] [-i <brief pointer>]
//
// in the session's folder. A new conversation's id is agy's own (it comes
// back in hooks as conversationId, LIVE), so aggy cannot set it; a resume
// names it with --conversation=<id>, the form agy prints at exit (LIVE; that
// it reopens the conversation is DOCS). The brief is the first prompt, a
// short fixed pointer to the brief file, never its content (-i, LIVE help;
// unverified under aggy). The plan reaches the hook through the environment.
func (a Adapter) Interactive(req adapter.InteractiveRequest) (adapter.Command, error) {
	l := req.Antigravity
	if l == nil {
		return adapter.Command{}, errors.New("antigravity: no launch settings, so aggy's policy would not reach it")
	}
	if err := adapter.CheckAbs("settings file", l.SettingsFile); err != nil {
		return adapter.Command{}, err
	}
	switch {
	case req.Print:
		return adapter.Command{}, errors.New("antigravity: " + printWhy)
	case req.ResumeName != "":
		return adapter.Command{}, errors.New("antigravity: aggy resumes Antigravity conversations by id only")
	case req.SessionID != "":
		return adapter.Command{}, fmt.Errorf("antigravity: a new conversation's id is agy's own: %w", adapter.ErrUnsupported)
	case req.Fork:
		return adapter.Command{}, fmt.Errorf("antigravity: forking a conversation: %w", adapter.ErrUnsupported)
	case (l.Plan == "") != (l.PlanSum == ""):
		return adapter.Command{}, errors.New("antigravity: a decision plan needs its SHA-256")
	}
	if err := adapter.CheckAbs("working directory", req.Cwd); err != nil {
		return adapter.Command{}, err
	}
	if err := adapter.ValidModel(req.Model); err != nil {
		return adapter.Command{}, err
	}
	pf := a.PassFlags()
	for _, x := range req.Extra {
		if strings.HasPrefix(x, "-") && slices.Contains(pf.Owned, goName(x)) {
			return adapter.Command{}, errors.New("antigravity: aggy passes its own --settings")
		}
	}
	args := []string{"--settings", l.SettingsFile}
	if req.Resume != "" {
		if err := adapter.ValidSessionID(adapter.Antigravity, req.Resume); err != nil {
			return adapter.Command{}, err
		}
		args = append(args, "--conversation="+req.Resume)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	args = append(args, req.Extra...)
	if req.BriefFile != "" {
		if err := adapter.CheckAbs("brief file", req.BriefFile); err != nil {
			return adapter.Command{}, err
		}
		args = append(args, "-i", adapter.BriefPrompt(req.BriefFile))
	}
	env := slices.Clone(req.Env)
	if l.Plan != "" {
		if err := adapter.CheckAbs("decision plan", l.Plan); err != nil {
			return adapter.Command{}, err
		}
		if !hexSum(l.PlanSum) {
			return adapter.Command{}, errors.New("antigravity: the decision plan's SHA-256 is not 64 hex digits")
		}
		env = append(env, PlanEnv+"="+l.Plan, PlanSumEnv+"="+l.PlanSum)
	}
	return adapter.Command{Path: Bin, Args: args, Env: env, Dir: req.Cwd, Stdin: adapter.StdinTerminal}, nil
}

// goName is a word's flag name as Go's flag package reads it, written
// canonically (one dash for one letter, two for more).
func goName(w string) string {
	n, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(w, "-"), "-"), "=")
	if len(n) == 1 {
		return "-" + n
	}
	return "--" + n
}

// Headless refuses: no borrow goes to Antigravity.
func (Adapter) Headless(adapter.HeadlessRequest) (adapter.Command, error) {
	return adapter.Command{}, fmt.Errorf("antigravity runs on screen only: %w", adapter.ErrUnsupported)
}

// ParseHeadless refuses: there is no headless command to read.
func (Adapter) ParseHeadless(adapter.HeadlessRequest, adapter.Output) (adapter.HeadlessResult, error) {
	return adapter.HeadlessResult{Error: adapter.ErrUnknown}, adapter.ErrUnsupported
}

// Artifacts lists none: there are no headless runs.
func (Adapter) Artifacts(adapter.HeadlessRequest, adapter.HeadlessResult) ([]adapter.Artifact, error) {
	return nil, nil
}

// ErrNoTranscript is why aggy reads no Antigravity transcript: it sits under
// ~/.gemini/antigravity-cli/brain/ (LIVE: the hook payload's transcriptPath),
// the same tree as agy's undocumented token-file fallback (DOCS), which safefs
// refuses whole until that file's place is known. TODO(T6957): a reader, once
// a live check names the token file and the transcript's format.
var ErrNoTranscript = fmt.Errorf("%w: aggy reads no Antigravity transcript yet (it sits beside agy's sign-in under ~/.gemini, which aggy never opens)", adapter.ErrNoTranscript)

// TranscriptTail returns ErrNoTranscript.
func (Adapter) TranscriptTail(adapter.SessionRef, int) (adapter.Transcript, error) {
	return adapter.Transcript{}, ErrNoTranscript
}

// ConfigPatches returns none: aggy's hooks entry is written per launch
// (internal/cli, prepareAntigravity), and agy's own settings are never edited.
func (Adapter) ConfigPatches(adapter.ConfigContext) ([]patch.Patch, error) { return nil, nil }

// hexSum reports whether s is a SHA-256 in lower-case hex, as
// policyhook.ValidSum does (copied: policyhook imports parity, which this
// package must not, since the manifest imports it for StripHooks).
func hexSum(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune("0123456789abcdef", rune(s[i])) {
			return false
		}
	}
	return true
}
