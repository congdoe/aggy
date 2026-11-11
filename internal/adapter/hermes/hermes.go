package hermes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/probe"
)

// Source cited below is the Hermes Agent checkout at v0.23.1 (commit
// 3b42157858), paths relative to its root. LIVE marks a check recorded in
// docs/verification.md ("Hermes Agent 0.23.1").

// Bin is the CLI's program name (pyproject.toml:592-595, hermes =
// hermes_cli.main:main).
const Bin = "hermes"

// Adapter is the Hermes adapter. The zero value is ready to use.
type Adapter struct {
	// Path is the CLI's path for Probe. Empty looks it up on PATH.
	Path string
}

var _ adapter.Adapter = Adapter{}

// New returns a Hermes adapter.
func New() Adapter { return Adapter{} }

// Vendor returns adapter.Hermes.
func (Adapter) Vendor() adapter.Vendor { return adapter.Hermes }

// Tested is the Hermes version the facts in docs/adapters.md were read or
// checked at: `hermes --version` printed "Hermes Agent v0.23.1 (2026.9.24)"
// first (LIVE).
var Tested = adapter.VersionRange{Min: "0.23.1", Max: "0.23.1"}

// TestedVersions returns Tested.
func (Adapter) TestedVersions() adapter.VersionRange { return Tested }

// LockdownCaps returns nil: Hermes takes no borrows.
func (Adapter) LockdownCaps(adapter.Kind) []string { return nil }

// SelfTestKind returns research, for the interface: with no borrows, the
// new-version self-test never runs on Hermes.
func (Adapter) SelfTestKind() adapter.Kind { return adapter.Research }

// loosening are Hermes' flags that loosen approvals, hook trust, or what the
// session loads (hermes_cli/_parser.py:148-153, :193-212, :270-271,
// :310-319): passed only with --no-policy.
var loosening = []adapter.RefusedFlag{
	{Names: []string{"--yolo"}, Why: "it bypasses every dangerous-command approval", UnlessNoPolicy: true},
	{Names: []string{"--accept-hooks"}, Why: "it approves every hook without asking you; aggy's hooks are approved once, by you, at Hermes' own prompt", UnlessNoPolicy: true},
	{Names: []string{"--ignore-user-config"}, Why: "it drops the profile config that carries aggy's approvals and hooks", UnlessNoPolicy: true},
	{Names: []string{"--safe-mode"}, Why: "it turns off every hook, aggy's too", UnlessNoPolicy: true},
	{Names: []string{"--ignore-rules"}, Why: "it drops the instructions and memory aggy passes", UnlessNoPolicy: true},
}

// PassFlags is `aggy hermes`'s table. aggy takes over -m and --resume, and
// refuses the flags that clash with the launch (its profile, folder, screen,
// and first turn) or loosen it. --api-key and --base-url are always refused:
// aggy never puts a key or a base URL on a command line. Every other flag
// passes, after `chat`.
func (Adapter) PassFlags() adapter.PassFlags {
	refused := append(slices.Clone(loosening),
		adapter.RefusedFlag{Names: []string{"--api-key"}, Why: "aggy never passes a key on the command line; sign in with hermes auth add"},
		adapter.RefusedFlag{Names: []string{"--base-url"}, Why: "aggy never sets a base URL"},
		adapter.RefusedFlag{Names: []string{"-z", "--oneshot"}, Why: "one-shot mode auto-approves every dangerous command; aggy runs Hermes on screen only"},
		adapter.RefusedFlag{Names: []string{"-p", "--profile"}, Why: "aggy runs Hermes in its own profile for this repo"},
		adapter.RefusedFlag{Names: []string{"--in", "-w", "--worktree", "--no-restore-cwd"}, Why: "aggy starts the session in its own folder"},
		adapter.RefusedFlag{Names: []string{"--tui", "--native", "--tui-native", "--dev"}, Why: "aggy runs the classic screen (--cli)"},
		adapter.RefusedFlag{Names: []string{"--cli"}, Why: "aggy always passes it"},
		adapter.RefusedFlag{Names: []string{"-q", "--query", "--query-file", "-Q", "--quiet", "--format", "--image", "--usage-file"}, Why: "aggy runs Hermes on screen only and passes no prompt yet; type it once the session starts"},
		adapter.RefusedFlag{Names: []string{"--source", "--create-if-missing", "--pass-session-id"}, Why: "it changes the session aggy records"},
	)
	return adapter.PassFlags{
		Model:    []string{"-m", "--model"},
		Continue: []string{"-c", "--continue"},
		Resume:   []string{"-r", "--resume"},
		Bare:     []string{"--version", "-V"},
		Refused:  refused,
		// Value flags from hermes_cli/_parser.py:165-178, :198, :264-266,
		// :300-309.
		Values: []string{"--provider", "--reasoning", "-t", "--toolsets", "-s", "--skills", "--max-turns", "--run-budget", "--cpu-profile"},
		// The subcommands `hermes --help` lists (LIVE).
		Commands: []string{"chat", "model", "auth", "config", "profile", "sessions", "hooks", "gateway"},
		Effect:   loosenEffect,
		// Hermes' parser takes abbreviations (argparse allow_abbrev,
		// hermes_cli/_parser.py:354-397; LIVE: `hermes chat --yol`, `--api-ke
		// x`, `--accept-hook`, `--safe-mod`, and `--onesho` were each taken),
		// so on screen aggy passes only these spellings, in full (the flags of
		// _parser.py:144-212, :264-319 that aggy neither takes over nor
		// refuses), and refuses any other dash-led word.
		Only: []string{"--provider", "--reasoning", "-t", "--toolsets", "-s", "--skills", "--max-turns", "--run-budget",
			"--cpu-profile", "--checkpoints", "-v", "--verbose", "--version", "-V", "-h", "--help"},
	}
}

// SignIn returns Hermes' own sign-in steps (hermes_cli/subcommands/auth.py;
// LIVE: `hermes auth list` shows the provider). There is no status check
// whose output format is recorded, so the status is unknown. aggy signs in to
// nothing and stores no token.
func (Adapter) SignIn() adapter.SignIn {
	return adapter.SignIn{Steps: "run `hermes auth add <provider>`, or `hermes model`"}
}

// Probe finds the CLI's version and which recorded flags its help lists.
func (a Adapter) Probe(ctx context.Context, run adapter.Runner) (adapter.Probe, error) {
	path := a.Path
	if path == "" {
		p, err := probe.LookPath(Bin)
		if err != nil {
			return adapter.Probe{Vendor: adapter.Hermes}, err
		}
		path = p
	}
	return probe.Detect(ctx, run, adapter.Hermes, path)
}

// Reachable says why aggy's launch cannot reach this CLI, given its probed
// capabilities, or "".
func Reachable(caps []string) string {
	for _, c := range []struct{ cap, why string }{
		{probe.CapClassicScreen, "hermes --help does not list --cli"},
		{probe.CapResume, "hermes --help does not list --resume"},
	} {
		if !slices.Contains(caps, c.cap) {
			return c.why
		}
	}
	return ""
}

// profileRE is a profile id: hermes_constants.py:401, PROFILE_ID_RE.
var profileRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ProfilePrefix starts every profile aggy makes.
const ProfilePrefix = "aggy-"

// ProfileName is the aggy-owned profile for the repo whose canonical path is
// key: "aggy-" and 12 hex digits of its SHA-256. It never names a reserved
// profile (hermes_cli/profiles.py:280) or the person's default.
func ProfileName(key string) string {
	s := sha256.Sum256([]byte(key))
	return ProfilePrefix + hex.EncodeToString(s[:])[:12]
}

// ValidProfile reports a profile name aggy made.
func ValidProfile(p string) bool {
	return profileRE.MatchString(p) && strings.HasPrefix(p, ProfilePrefix) && len(p) == len(ProfilePrefix)+12
}

// Interactive builds
//
//	hermes -p <profile> chat --cli [--resume <id> --no-restore-cwd] [-m <model>] [extra...] [--query-file <brief>]
//
// in the session's folder. -p sets HERMES_HOME to the profile before argparse
// (hermes_cli/_parser.py:13-16, main.py:508-566). --cli forces the classic
// screen (:216, :326). A new session's id is minted by Hermes
// (hermes_state_ids.py:56-60), so aggy cannot set it; a resume names it, and
// --no-restore-cwd keeps the folder aggy recorded (:281-282). The brief is the
// first turn's text, read by Hermes from the file (--query-file, :241-245: on
// a terminal it seeds the session as its first turn, :236-240); with a resume
// that is unverified. The memory goes as HERMES_EPHEMERAL_SYSTEM_PROMPT, a
// session client variable that is never persisted
// (agent/session_client_env.py:23, hermes_cli/cli_init_mixin.py:349); whether
// it reaches a resumed session is unverified.
func (a Adapter) Interactive(req adapter.InteractiveRequest) (adapter.Command, error) {
	l := req.Hermes
	if l == nil {
		return adapter.Command{}, errors.New("hermes: no launch profile, so aggy's approvals and hooks would not reach it")
	}
	if !ValidProfile(l.Profile) {
		return adapter.Command{}, fmt.Errorf("hermes: %q is not a profile aggy makes", l.Profile)
	}
	if req.ResumeName != "" {
		return adapter.Command{}, errors.New("hermes: aggy resumes Hermes sessions by aggy's id only")
	}
	if req.SessionID != "" {
		return adapter.Command{}, fmt.Errorf("hermes: a new session's id is Hermes' own: %w", adapter.ErrUnsupported)
	}
	if req.Fork {
		return adapter.Command{}, fmt.Errorf("hermes: forking a session: %w", adapter.ErrUnsupported)
	}
	if err := adapter.CheckAbs("working directory", req.Cwd); err != nil {
		return adapter.Command{}, err
	}
	if err := adapter.ValidModel(req.Model); err != nil {
		return adapter.Command{}, err
	}
	args := []string{"-p", l.Profile, "chat", "--cli"}
	if req.Resume != "" {
		if err := adapter.ValidSessionID(adapter.Hermes, req.Resume); err != nil {
			return adapter.Command{}, err
		}
		args = append(args, "--resume", req.Resume, "--no-restore-cwd")
	}
	if req.Model != "" {
		args = append(args, "-m", req.Model)
	}
	for _, x := range req.Extra {
		if x == "--api-key" || strings.HasPrefix(x, "--api-key=") || x == "--base-url" || strings.HasPrefix(x, "--base-url=") {
			return adapter.Command{}, errors.New("hermes: aggy never passes a key or a base URL")
		}
	}
	args = append(args, req.Extra...)
	if req.BriefFile != "" {
		if err := adapter.CheckAbs("brief file", req.BriefFile); err != nil {
			return adapter.Command{}, err
		}
		args = append(args, "--query-file", req.BriefFile)
	}
	env := slices.Clone(req.Env)
	if req.Memory != "" {
		if strings.ContainsRune(req.Memory, 0) {
			return adapter.Command{}, errors.New("hermes: the memory holds a NUL byte")
		}
		env = append(env, MemoryEnv+"="+req.Memory)
	}
	return adapter.Command{Path: Bin, Args: args, Env: env, Dir: req.Cwd, Stdin: adapter.StdinTerminal}, nil
}

// MemoryEnv carries aggy's shared memory into the session.
const MemoryEnv = "HERMES_EPHEMERAL_SYSTEM_PROMPT"

// Headless refuses: no open harness is routed to.
func (Adapter) Headless(adapter.HeadlessRequest) (adapter.Command, error) {
	return adapter.Command{}, fmt.Errorf("hermes runs on screen only: %w", adapter.ErrUnsupported)
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
// ExportCommand and hands the output to ParseExport. state.db, which holds
// the transcripts, is never opened (safefs refuses it).
func (Adapter) TranscriptTail(adapter.SessionRef, int) (adapter.Transcript, error) {
	return adapter.Transcript{}, adapter.ErrNoTranscript
}

// ConfigPatches returns none: the profile's block is written per launch
// (internal/cli, prepareHermes), and the person's own profiles are never
// edited.
func (Adapter) ConfigPatches(adapter.ConfigContext) ([]patch.Patch, error) { return nil, nil }

// ProfileCreateCommand builds `hermes profile create <name> --no-alias
// --no-skills`: a profile needs Hermes' own identity files and layout
// (hermes_cli/profiles.py:1344-1420, create_profile refuses a folder with
// no identity), so aggy cannot make one by writing a folder. LIVE: such a
// profile sees the root's sign-in and runs a query. --no-alias writes no
// wrapper script; --no-skills seeds no bundled skills.
func ProfileCreateCommand(path, name string) (adapter.Command, error) {
	if !ValidProfile(name) {
		return adapter.Command{}, fmt.Errorf("hermes: %q is not a profile aggy makes", name)
	}
	return adapter.Command{Path: path, Args: []string{"profile", "create", name, "--no-alias", "--no-skills"}}, nil
}

// ProfileDeleteCommand builds `hermes profile delete <name> --yes`, for aggy
// unwire to remove a profile aggy made (LIVE: it removed the profile;
// hermes_cli/profiles.py). Only a name aggy makes: never the default.
func ProfileDeleteCommand(path, name string) (adapter.Command, error) {
	if !ValidProfile(name) {
		return adapter.Command{}, fmt.Errorf("hermes: %q is not a profile aggy makes", name)
	}
	return adapter.Command{Path: path, Args: []string{"profile", "delete", name, "--yes"}}, nil
}

// ProfileReverse names, on a marker row in aggy's changes, a Hermes profile
// aggy made with hermes profile create, which aggy unwire deletes (#134
// review). The row's file is the profile's folder.
const ProfileReverse = "hermes-profile"

// ConfigGetCommand builds `hermes [-p <profile>] config get <key> --json`
// (hermes_cli/subcommands/config.py:20-25; hermes_cli/config.py:3450-3483:
// the merged value, credentials masked). profile "" reads the person's
// default profile. LIVE: `hermes config get <key>` prints the value.
func ConfigGetCommand(path, profile, key string) (adapter.Command, error) {
	if !configKeyRE.MatchString(key) {
		return adapter.Command{}, fmt.Errorf("hermes: %q is not a config key aggy reads", key)
	}
	var args []string
	if profile != "" {
		if !ValidProfile(profile) {
			return adapter.Command{}, fmt.Errorf("hermes: %q is not a profile aggy makes", profile)
		}
		args = []string{"-p", profile}
	}
	return adapter.Command{Path: path, Args: append(args, "config", "get", key, "--json")}, nil
}

var configKeyRE = regexp.MustCompile(`^[a-z_]+(\.[a-z_]+)*$`)

// AdoptExternalLogins is the root config key that makes Hermes read and
// refresh other CLIs' logins (hermes_cli/config_defaults.py:1733-1739,
// default true).
const AdoptExternalLogins = "auth.adopt_external_logins"

// ParseBool reads a `config get --json` boolean; ok is false for anything
// else.
func ParseBool(out []byte) (v, ok bool) {
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &v); err != nil {
		return false, false
	}
	return v, true
}

// ExportCommand builds `hermes -p <profile> sessions export - --session-id
// <id> --format jsonl --redact`: the one session as one JSON line on stdout
// (hermes_cli/subcommands/sessions.py:100-127; sessions_cmd.py:114-118 writes
// "-" to stdout alone), with Hermes' own secret redaction. It is the only way
// aggy reads a session: they are in state.db.
func ExportCommand(path, profile, id string) (adapter.Command, error) {
	if !ValidProfile(profile) {
		return adapter.Command{}, fmt.Errorf("hermes: %q is not a profile aggy makes", profile)
	}
	if err := adapter.ValidSessionID(adapter.Hermes, id); err != nil {
		return adapter.Command{}, err
	}
	return adapter.Command{Path: path, Args: []string{"-p", profile, "sessions", "export", "-", "--session-id", id, "--format", "jsonl", "--redact"}}, nil
}

// exportSession is the part of an exported session aggy reads: the dict
// export_session returns (hermes_state_portability.py:347-355), its messages
// the rows of the messages table (hermes_state_common.py:537-547) through
// _row_to_message_dict (hermes_state_messages.py:1308-1330): role, content
// (a string, or a decoded JSON list), and timestamp in epoch seconds.
type exportSession struct {
	ID       *string `json:"id"`
	Messages []struct {
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Timestamp float64         `json:"timestamp"`
	} `json:"messages"`
}

// ParseExport reads up to n of the latest turns (all when n <= 0) of the
// first session in `sessions export --format jsonl` output: user messages
// and assistant text. Tool results, system rows, and anything else are left
// out. A shape it does not recognize is ErrNoTranscript, never a guess.
func ParseExport(out []byte, n int) (adapter.Transcript, error) {
	line, _, _ := strings.Cut(strings.TrimLeft(string(out), "\n"), "\n")
	var e exportSession
	if err := json.Unmarshal([]byte(line), &e); err != nil || e.ID == nil || e.Messages == nil {
		return adapter.Transcript{}, fmt.Errorf("%w: hermes sessions export format not recognized", adapter.ErrNoTranscript)
	}
	var turns []adapter.Turn
	for _, m := range e.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		text := contentText(m.Content)
		if text == "" {
			continue
		}
		sec := int64(m.Timestamp)
		t := time.Unix(sec, int64((m.Timestamp-float64(sec))*1e9)).UTC()
		turns = append(turns, adapter.Turn{Role: m.Role, Text: text, Time: t})
	}
	if n > 0 && len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	return adapter.Transcript{Turns: turns}, nil
}

// contentText is a message's text: a string, or the text parts of a list.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}
