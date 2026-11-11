package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/hermes"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/policyhook"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/termsafe"
)

// Hermes Agent on screen (T6933, docs/design.md section 15, "Hermes"). aggy runs the
// classic screen in a profile of its own for each repo, whose config.yaml
// carries aggy's block (adapter/hermes.Block): manual approvals, the policy's
// denies as approvals.deny, and two hooks that run aggy hook hermes. Hermes
// runs every hook in its daemon, with no AGGY_* variable and the daemon's
// folder, so the hook finds its launch in the index (store.HermesLaunch), by
// the profile on its command and the session id in its payload.

// hermesRunTimeout bounds one Hermes subcommand aggy runs (profile create,
// config get, sessions export): each starts Python.
const hermesRunTimeout = 60 * time.Second

// hermesRun runs the Hermes subcommands; a test replaces it.
var hermesRun = execrun.HermesRunner(hermesRunTimeout)

// hermesRoot is Hermes' home: $HERMES_HOME (its root, when it names a
// profile), else ~/.hermes (hermes_constants.py:57-64, 190-197 at v0.23.1).
func hermesRoot(getenv func(string) string) (string, error) {
	if v := getenv("HERMES_HOME"); v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("HERMES_HOME=%q is not an absolute path", v)
		}
		v = filepath.Clean(v)
		if filepath.Base(filepath.Dir(v)) == "profiles" {
			return filepath.Dir(filepath.Dir(v)), nil
		}
		return v, nil
	}
	h := getenv("HOME")
	if h == "" || !filepath.IsAbs(h) {
		return "", errors.New("HOME is not an absolute path")
	}
	return filepath.Join(h, ".hermes"+getenv("HERMES_DATA_DIR_SUFFIX")), nil
}

// hermesTicked reports whether this folder's trust list has Hermes ticked.
func hermesTicked(dir string) bool {
	tg, err := trustTargetFor(dir)
	if err != nil {
		return false
	}
	path, err := trustFile()
	if err != nil {
		return false
	}
	f, err := readTrustRecord(path)
	if err != nil {
		return false
	}
	return slices.Contains(f.Folders[tg.key].On, trustRowHermes)
}

// hermesTickInPlace offers, at a launch in a folder whose trust list does not
// tick Hermes, to tick it there and go on (T6979, askInPlace). Only Hermes'
// row changes: the folder's other answers, and whether aggy trusts the repo
// for its rules, stay as they are. yes says it is ticked now.
func hermesTickInPlace(ctx context.Context, in hermesInput) (bool, error) {
	if in.in == nil || in.getenv == nil {
		return false, nil
	}
	tg, err := trustTargetFor(in.cwd)
	if err != nil {
		return false, nil
	}
	path, err := trustFile()
	if err != nil {
		return false, err
	}
	f, err := readTrustRecord(path)
	if err != nil {
		return false, nil // the refusal names aggy trust, which says why
	}
	ans := f.Folders[tg.key]
	if !slices.Contains(ans.Shown, trustRowHermes) {
		ans.Shown = append(slices.Clone(ans.Shown), trustRowHermes)
	}
	ans.On = append(slices.Clone(ans.On), trustRowHermes)
	ans.Never = false
	p, changes, err := trustFileChange(path, tg.key, 0, tg.key, &ans)
	if err != nil || !changes {
		return false, err
	}
	yes, asked := askInPlace(in.getenv, in.in, in.out, inPlace{
		change: fmt.Sprintf("Hermes Agent is not ticked in aggy's trust list for %s. aggy can tick it there, and only it: %s.", tg.root, hermesTrustWarning),
		undo:   "aggy unwire undoes it, or aggy trust " + tg.root + " and untick it.",
		ask:    "Tick Hermes Agent here and start it?",
	})
	if !asked || !yes {
		return false, nil
	}
	runID, err := applyPatches(ctx, []patch.Patch{p})
	if err != nil {
		return false, err
	}
	fmt.Fprintf(in.out, "Done (run %s). aggy unwire --run %s undoes it.\n", runID, runID)
	return true, nil
}

// hermesProfileFor is aggy's profile for the repo holding dir, and the
// repo's root by its real path.
func hermesProfileFor(dir string) (profile, root string, err error) {
	real, err := safefs.RealPath(dir)
	if err != nil {
		return "", "", err
	}
	root, _ = repoRoot(real)
	key, err := safefs.CanonicalPath(root)
	if err != nil {
		return "", "", err
	}
	return hermes.ProfileName(key), root, nil
}

// hermesInput is what a launch hands prepareHermes.
type hermesInput struct {
	s        *store.Store
	caps     []string
	path     string // the CLI's absolute path
	cwd      string
	dir      string // the launch's private folder, removed when the CLI exits
	runDir   string // the supervisors' sockets
	noPolicy bool
	plan     parity.PolicyPlan
	rules    permissions.Set
	version  string
	ids      parity.ReadIdentity
	// getenv, in, and out ask in place (askInPlace); a nil in asks nothing.
	getenv func(string) string
	in     io.Reader
	out    io.Writer
}

// hermesSetup is what preparing a Hermes launch decided.
type hermesSetup struct {
	launch            adapter.HermesLaunch
	root, profileDir  string
	planPath, planSum string
	token             string
	warns             []string
}

// hermesErr is a refused Hermes launch, with its code.
type hermesErr struct {
	code diag.Code
	err  error
}

func (e *hermesErr) Error() string { return e.err.Error() }
func (e *hermesErr) Unwrap() error { return e.err }

func hermesFail(code diag.Code, format string, a ...any) error {
	return &hermesErr{code: code, err: fmt.Errorf(format, a...)}
}

// prepareHermes readies aggy's profile for the repo and the launch's plan.
// A launch that cannot be set up safely is an error, whatever [policy] says:
// without the block, none of aggy's approvals and hooks would reach Hermes.
func prepareHermes(ctx context.Context, in hermesInput) (hermesSetup, error) {
	var out hermesSetup
	profile, root, err := hermesProfileFor(in.cwd)
	if err != nil {
		return out, err
	}
	out.root, out.launch.Profile = root, profile
	if !hermesTicked(in.cwd) {
		yes, err := hermesTickInPlace(ctx, in)
		if err != nil {
			return out, err
		}
		if !yes {
			return out, hermesFail(diag.CodeHermesUntrusted, "Hermes is not ticked in aggy's trust list for this folder, so aggy does not start it here: aggy trust %s lists it, with what ticking it means", root)
		}
	}
	if why := hermes.Reachable(in.caps); why != "" {
		return out, hermesFail(diag.CodeHermesProfile, "the safety policy cannot be applied: %s; update Hermes (aggy was checked on 0.23.1), or pass --no-policy to start it without aggy's decisions", why)
	}
	if err := hermesUnclaimed(ctx, in.s, in.runDir, profile); err != nil {
		return out, err
	}
	bin, err := aggyBin()
	if err != nil {
		return out, hermesFail(diag.CodeHermesProfile, "aggy's hooks cannot run: %v; start the aggy you installed (make build), not a temporary one", err)
	}
	stateDir, err := home.Dir()
	if err != nil {
		return out, err
	}
	hroot, err := hermesRoot(os.Getenv)
	if err != nil {
		return out, hermesFail(diag.CodeHermesProfile, "cannot find Hermes' home: %v; set HERMES_HOME to an absolute path, or unset it", err)
	}
	out.profileDir = filepath.Join(hroot, "profiles", profile)
	created, err := ensureHermesProfile(ctx, in.path, profile, out.profileDir)
	if err != nil {
		return out, err
	}
	// The repo's policy, whatever --no-policy says: the profile is shared by
	// every session in this repo, so one launch's flag does not loosen
	// another's denies. --no-policy only turns the hook's decisions off.
	block, err := hermes.Block{Bin: bin, Home: stateDir, Profile: profile, Deny: parity.HermesDeny(in.plan.Decide())}.Render()
	if err != nil {
		return out, hermesFail(diag.CodeHermesProfile, "%v", err)
	}
	written, err := writeHermesBlock(ctx, filepath.Join(out.profileDir, "config.yaml"), block)
	if err != nil {
		return out, err
	}
	if err := checkHermesBlock(ctx, in.path, profile, filepath.Join(out.profileDir, "config.yaml"), block, bin, stateDir, created || written); err != nil {
		return out, err
	}
	if created || written {
		dlog.Log(ctx, diag.Rec{Event: diag.EvHermesProfile, Vendor: string(adapter.Hermes), Attrs: map[string]any{"created": created, "written": written}})
	}
	if !in.noPolicy && in.plan.OnScreen {
		d := policyhook.NewDecidePlan(string(adapter.Hermes), in.version, false, in.plan, parity.PreApprovedFrom(in.rules, root, in.ids))
		d.Root, d.Protected = root, append(protectedPaths(), out.profileDir)
		if out.planPath, out.planSum, err = writeDecidePlan(in.dir, d); err != nil {
			return out, err
		}
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return out, err
	}
	out.token = hex.EncodeToString(b[:])
	out.warns = append(out.warns, hermesLaunchNotes(ctx, in.path, out.profileDir, bin, stateDir, profile, created)...)
	return out, nil
}

// hermesUnclaimed refuses a launch while another live one in the same
// profile has no session id yet: the first hook naming a new session claims
// the oldest such launch, so two at once could be swapped. A row whose
// supervisor is gone is removed.
func hermesUnclaimed(ctx context.Context, s *store.Store, runDir, profile string) error {
	if s == nil {
		return nil
	}
	ls, err := s.HermesLaunches(ctx, profile)
	if err != nil {
		return err
	}
	for _, l := range ls {
		if hermesGone(runDir, l, time.Now()) {
			_ = s.DeleteHermesLaunch(ctx, l.ID)
			continue
		}
		if l.SessionID == "" {
			return hermesFail(diag.CodeHermesUnclaimed, "another aggy Hermes session in this repo has not started a turn yet; type a first message there, or quit it, then start this one")
		}
	}
	return nil
}

// hermesGrace is how long a launch's row counts as live before its
// supervisor holds its lock: the row is recorded just before the CLI starts.
const hermesGrace = time.Minute

// hermesGone reports a launch whose supervisor is surely not running: no one
// holds its lock, and the row is older than hermesGrace. A lock that cannot
// be checked counts as running.
func hermesGone(runDir string, l store.HermesLaunch, now time.Time) bool {
	if now.Sub(l.Started) < hermesGrace {
		return false
	}
	live, err := supervisor.Live(runDir, l.ParentID)
	return err == nil && !live
}

// ensureHermesProfile makes aggy's profile with `hermes profile create` when
// it is not there; created says it did. A folder that is there but is no
// profile Hermes made fails at -p (hermes_cli/profiles.py:2335-2350).
func ensureHermesProfile(ctx context.Context, path, profile, dir string) (created bool, err error) {
	if _, err := safefs.Lstat(dir); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, hermesFail(diag.CodeHermesProfile, "cannot look at aggy's Hermes profile: %v", err)
	}
	cmd, err := hermes.ProfileCreateCommand(path, profile)
	if err != nil {
		return false, err
	}
	o, err := hermesRun(ctx, cmd)
	switch {
	case err != nil:
		return false, hermesFail(diag.CodeHermesProfile, "hermes profile create %s did not run: %v; run hermes profile create %s --no-alias --no-skills yourself to see why, then start again", profile, err, profile)
	case o.ExitCode != 0:
		return false, hermesFail(diag.CodeHermesProfile, "hermes profile create %s exited %d; run hermes profile create %s --no-alias --no-skills yourself to see why, then start again", profile, o.ExitCode, profile)
	}
	if fi, err := safefs.Lstat(dir); err != nil || !fi.IsDir() {
		return false, hermesFail(diag.CodeHermesProfile, "hermes profile create %s made no folder at %s; check HERMES_HOME, and run hermes profile list to see where it went", profile, dir)
	}
	return true, nil
}

// writeHermesBlock puts aggy's block in the profile's config.yaml through the
// manifest, so aggy unwire takes it out. The file's other lines are Hermes'
// own, seeded from the person's root config (hermes_cli/profiles.py:735-748),
// which may hold a provider's key, so the change keeps no backup and undoes
// by taking the block out (hermes.Reverse). written says it changed the file.
func writeHermesBlock(ctx context.Context, file string, block []byte) (written bool, err error) {
	before, err := safefs.ReadFileLimit(file, hermesMaxConfig)
	p := patch.Patch{File: file, Summary: "aggy's block in its Hermes profile's config.yaml (approvals, hooks, updates, and auth)",
		Transform: func(src []byte) ([]byte, error) { return hermes.SetBlock(src, block) }}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		p.ExpectedHash = patch.NoFile
	case err != nil:
		return false, hermesFail(diag.CodeHermesProfile, "cannot read aggy's Hermes profile config: %v", err)
	default:
		if _, err := hermes.SetBlock(before, block); err != nil {
			return false, hermesFail(diag.CodeHermesProfile, "%v; edit %s: move those settings into aggy's block or delete them, then start again (aggy unwire takes aggy's block out)", err, file)
		}
		p.ExpectedHash, p.NoBackup, p.Reverse = patch.Hash(before), true, hermes.Reverse
	}
	pl, err := patch.Plan(p)
	if err != nil {
		return false, hermesFail(diag.CodeHermesProfile, "cannot plan aggy's block: %v", err)
	}
	if pl.Action == patch.NoOp {
		return false, nil
	}
	if _, err := applyPatches(ctx, []patch.Patch{p}); err != nil {
		return false, hermesFail(diag.CodeHermesProfile, "cannot write aggy's block into its Hermes profile: %v; if Hermes changed %s while aggy wrote it, start again", err, file)
	}
	return true, nil
}

// hermesMaxConfig caps the profile's config.yaml aggy reads.
const hermesMaxConfig = 1 << 20

// checkHermesBlock reads the config back: the file must hold the block as
// rendered, and, after a write, Hermes itself must read aggy's settings
// (hermes -p <profile> config get ... --json) as aggy wrote them. Anything
// else refuses the launch.
func checkHermesBlock(ctx context.Context, path, profile, file string, block []byte, bin, stateDir string, ask bool) error {
	b, err := safefs.ReadFileLimit(file, hermesMaxConfig)
	if err != nil || !bytes.Contains(b, block) {
		return hermesFail(diag.CodeHermesProfile, "aggy's Hermes profile config %s does not read back as aggy wrote it; start again, and if it still differs, aggy unwire takes aggy's block out so it can be written fresh", file)
	}
	if !ask {
		return nil
	}
	get := func(key string) ([]byte, error) {
		cmd, err := hermes.ConfigGetCommand(path, profile, key)
		if err != nil {
			return nil, err
		}
		o, err := hermesRun(ctx, cmd)
		if err != nil {
			return nil, err
		}
		if o.ExitCode != 0 {
			return nil, fmt.Errorf("hermes config get %s exited %d", key, o.ExitCode)
		}
		return o.Stdout, nil
	}
	fail := func(what string) error {
		return hermesFail(diag.CodeHermesProfile, "Hermes does not read aggy's profile config as aggy wrote it: %s; check that no other copy of %s overrides it (hermes -p %s config get %s), then start again", what, what, profile, what)
	}
	mode, err := get("approvals.mode")
	var m string
	if err != nil || json.Unmarshal(bytes.TrimSpace(mode), &m) != nil || m != "manual" {
		return fail("approvals.mode")
	}
	if auto, err := get("hooks_auto_accept"); err != nil {
		return fail("hooks_auto_accept")
	} else if v, ok := hermes.ParseBool(auto); !ok || v {
		return fail("hooks_auto_accept")
	}
	hooks, err := get("hooks")
	var hs map[string][]struct {
		Command string `json:"command"`
	}
	if err != nil || json.Unmarshal(bytes.TrimSpace(hooks), &hs) != nil {
		return fail("hooks")
	}
	for _, ev := range hermes.Events {
		want, _ := hermes.HookCommand(bin, stateDir, profile, ev)
		if !slices.ContainsFunc(hs[ev], func(h struct {
			Command string `json:"command"`
		}) bool {
			return h.Command == want
		}) {
			return fail("hooks." + ev)
		}
	}
	return nil
}

// hermesLaunchNotes are the lines a Hermes launch prints: the profile made,
// the hook consent Hermes asks for, the daemon, and the
// person's root config when it adopts other CLIs' logins.
func hermesLaunchNotes(ctx context.Context, path, profileDir, bin, stateDir, profile string, created bool) []string {
	var out []string
	if created {
		out = append(out, "made aggy's Hermes profile "+profile+" for this repo (hermes profile create); hermes profile delete "+profile+" removes it")
	}
	if missing := hermesConsentMissing(profileDir, bin, stateDir, profile); len(missing) > 0 {
		out = append(out, "Hermes asks you once, at its own prompt, to allow aggy's hooks ("+strings.Join(missing, " and ")+"): answer y, or aggy's policy decisions and switching cannot reach this session")
	}
	out = append(out, "Hermes runs its tools and hooks in its own daemon, which outlives this session and is not aggy's: aggy never starts, stops, or signals it")
	if hermesAdopts(ctx, path) {
		out = append(out, hermesAdoptNote)
	}
	return out
}

// hermesAdoptNote warns that the person's root config lets Hermes take other
// CLIs' logins. aggy's profile sets it false, and aggy never changes the
// root config.
const hermesAdoptNote = "your Hermes config has auth.adopt_external_logins on: Hermes may read and refresh Codex's and Claude Code's logins, whose refresh tokens are single-use, which can sign those CLIs out (aggy's profile turns it off; hermes config set auth.adopt_external_logins false turns it off for yours)"

// hermesAdopts reports the person's default profile adopting other CLIs'
// logins, asked through `hermes config get`, never by opening a file that
// may hold secrets. One it cannot tell is false.
func hermesAdopts(ctx context.Context, path string) bool {
	cmd, err := hermes.ConfigGetCommand(path, "", hermes.AdoptExternalLogins)
	if err != nil {
		return false
	}
	o, err := hermesRun(ctx, cmd)
	if err != nil || o.ExitCode != 0 {
		return false
	}
	v, ok := hermes.ParseBool(o.Stdout)
	return ok && v
}

// hermesConsentMissing names the hook events Hermes' allowlist in the profile
// does not hold yet (shell-hooks-allowlist.json: {"approvals": [{event,
// command, ...}]}, agent/shell_hooks.py:703-790 at v0.23.1). aggy only reads
// it, to say whether Hermes will ask; it never writes it. One it cannot read
// counts as missing.
func hermesConsentMissing(profileDir, bin, stateDir, profile string) []string {
	var al struct {
		Approvals []struct{ Event, Command string } `json:"approvals"`
	}
	b, err := safefs.ReadFileLimit(filepath.Join(profileDir, "shell-hooks-allowlist.json"), hermesMaxConfig)
	if err == nil {
		_ = json.Unmarshal(b, &al)
	}
	var out []string
	for _, ev := range hermes.Events {
		cmd, _ := hermes.HookCommand(bin, stateDir, profile, ev)
		if !slices.ContainsFunc(al.Approvals, func(a struct{ Event, Command string }) bool { return a.Event == ev && a.Command == cmd }) {
			out = append(out, ev)
		}
	}
	return out
}

// hermesTranscript reads a session's turns with `hermes sessions export`,
// the only way aggy reads one: sessions sit in state.db, which safefs refuses.
// The profile is the one aggy made for the session's repo. The brief scrubs
// the text (internal/redact) like any other transcript tail.
func hermesTranscript(ctx context.Context, ref adapter.SessionRef, turns int) (adapter.Transcript, error) {
	path, err := probe.LookPath(hermes.Bin)
	if err != nil {
		return adapter.Transcript{}, fmt.Errorf("%w: %v", adapter.ErrNoTranscript, err)
	}
	if ref.Cwd == "" {
		return adapter.Transcript{}, fmt.Errorf("%w: the session's folder is not recorded", adapter.ErrNoTranscript)
	}
	profile, _, err := hermesProfileFor(ref.Cwd)
	if err != nil {
		return adapter.Transcript{}, fmt.Errorf("%w: %v", adapter.ErrNoTranscript, err)
	}
	cmd, err := hermes.ExportCommand(path, profile, ref.ID)
	if err != nil {
		return adapter.Transcript{}, err
	}
	out, err := hermesRun(ctx, cmd)
	if err != nil {
		return adapter.Transcript{}, fmt.Errorf("%w: %v", adapter.ErrNoTranscript, err)
	}
	if out.ExitCode != 0 {
		return adapter.Transcript{}, fmt.Errorf("%w: hermes sessions export exited %d", adapter.ErrNoTranscript, out.ExitCode)
	}
	return hermes.ParseExport(out.Stdout, turns)
}

// hermesFindings are aggy doctor's Hermes lines.
func hermesFindings(ctx context.Context, vr checkup.Vendor) []checkup.Finding {
	out := []checkup.Finding{{Code: codeHermesScope, Level: checkup.LevelInfo, Vendor: adapter.Hermes,
		Message: "Hermes' approvals cover terminal commands only, and its daemon is its own: aggy never starts, stops, or signals it"}}
	cmd, err := hermes.ConfigGetCommand(vr.Path, "", hermes.AdoptExternalLogins)
	if err != nil {
		return out
	}
	o, err := hermesRun(ctx, cmd)
	if err != nil || o.ExitCode != 0 {
		return append(out, checkup.Finding{Code: codeHermesAdopt, Level: checkup.LevelWarn, Vendor: adapter.Hermes,
			Message: "cannot tell whether your Hermes config adopts other CLIs' logins (auth.adopt_external_logins)"})
	}
	if v, ok := hermes.ParseBool(o.Stdout); ok && v {
		out = append(out, checkup.Finding{Code: codeHermesAdopt, Level: checkup.LevelWarn, Vendor: adapter.Hermes, Message: hermesAdoptNote})
	}
	return out
}

// The doctor findings' stable codes.
const (
	codeHermesScope = "hermes-scope"
	codeHermesAdopt = "hermes-adopt-logins"
)

// hermesSetupNote is what setup says about Hermes.
const hermesSetupNote = `  aggy runs Hermes when you name it (aggy hermes), in a profile of its own for
  each repo, once you tick Hermes in that folder's trust list (aggy trust <dir>).
  Hermes asks you once to allow aggy's two hooks there: answer y. Its approvals
  cover terminal commands only, and its daemon is its own: aggy never starts,
  stops, or signals it. Sign in with Hermes itself (hermes auth add, hermes model).
`

// hermesHookArgs reads aggy hook hermes's arguments: --home <aggy's state
// folder> --profile <aggy's profile> --event <event>, in that order.
func hermesHookArgs(args []string) (homeDir, profile, event string, ok bool) {
	if len(args) != 6 || args[0] != "--home" || args[2] != "--profile" || args[4] != "--event" {
		return "", "", "", false
	}
	homeDir, profile, event = args[1], args[3], args[5]
	if !filepath.IsAbs(homeDir) || filepath.Clean(homeDir) != homeDir || !hermes.ValidProfile(profile) || !slices.Contains(hermes.Events, event) {
		return "", "", "", false
	}
	return homeDir, profile, event, true
}

// hermesHookWait bounds the hook's index and socket work: pre_tool_call's
// entry gives it hermes.PreToolTimeout seconds and then blocks the call.
const hermesHookWait = 6 * time.Second

// hermesHook is aggy hook hermes, run by Hermes' daemon. It finds the launch
// (hermesFind), then answers pre_tool_call with the safety policy's decision
// (policyhook.JudgeHermes) and passes on_session_end to the launch's
// supervisor as a turn end. It reads the payload's tool, session id, and
// profile, and on_session_end's failed and interrupted flags; it never logs,
// stores, or forwards anything else of the payload, extra included, which may
// hold the conversation. On pre_tool_call, every failure blocks the call; on
// on_session_end, nothing it does reaches Hermes.
func hermesHook(args []string, in io.Reader, stdout, stderr io.Writer) int {
	homeDir, profile, event, ok := hermesHookArgs(args)
	if !ok {
		drain(in)
		return cmdUsage(stderr, "hook hermes", "want --home <aggy's state folder> --profile <aggy's Hermes profile> --event pre_tool_call|on_session_end", hookUsage)
	}
	// The daemon passes no AGGY_* variable: the state folder is the command's.
	_ = os.Setenv(home.Env, homeDir)
	ctx, cancel := context.WithTimeout(context.Background(), hermesHookWait)
	defer cancel()
	raw, err := io.ReadAll(io.LimitReader(in, hook.MaxPayload+1))
	tooBig := err == nil && len(raw) > hook.MaxPayload
	if err != nil || tooBig {
		raw = nil
		drain(in)
	}
	p, pok := policyhook.ParseHermes(raw)
	pre := event == hermes.EventPreToolCall
	refuse := func(why string) int {
		dlog.Log(ctx, diag.Rec{Event: diag.EvHermesRefused, Code: diag.CodeHermesNoLaunch, Vendor: string(adapter.Hermes), Attrs: map[string]any{"event": event}})
		if pre {
			_, _ = stdout.Write(policyhook.HermesBlock("aggy: " + why + ", so this call is refused " + dlog.Tag(diag.CodeHermesNoLaunch)))
		}
		return 0
	}
	switch {
	case tooBig:
		// Its launch cannot be told without the payload's session id.
		return refuse("the call is larger than aggy's hook reads (1 MiB)")
	case !pok || p.Profile != profile:
		return refuse("the hook's payload is not from aggy's Hermes profile")
	}
	l, moved, err := hermesFind(ctx, profile, p.SessionID)
	if err != nil {
		return refuse("no aggy launch is running for this Hermes session (start it with aggy run hermes)")
	}
	runDir := filepath.Join(homeDir, "run")
	if moved {
		_ = hermesSend(runDir, l, "SessionStart", p.SessionID) // a hint; the turn end below says the rest
	}
	if !pre {
		ev := "Stop"
		var end struct {
			Extra struct {
				Failed      bool `json:"failed"`
				Interrupted bool `json:"interrupted"`
			} `json:"extra"`
		}
		_ = json.Unmarshal(raw, &end)
		switch {
		case end.Extra.Interrupted:
			ev = "StopCancelled"
		case end.Extra.Failed:
			ev = "StopFailure"
		}
		if err := hermesSend(runDir, l, ev, p.SessionID); err != nil {
			dlog.Log(ctx, diag.Rec{Event: diag.EvHookForwardFailed, Code: diag.CodeUnreachable, Msg: "the supervisor's socket could not be reached", Vendor: string(adapter.Hermes)})
			fmt.Fprintf(stderr, "aggy hook hermes: the supervisor could not be reached: %s %s\n", termsafe.Line(err.Error()), dlog.Tag(diag.CodeUnreachable))
		}
		return 0
	}
	if l.Plan == "" {
		return 0 // --no-policy: no decision
	}
	return hermesDecide(l, raw, stdout, stderr)
}

// hermesDecide answers one pre_tool_call from the launch's plan, recording a
// start and an end as aggy hook decide does.
func hermesDecide(l store.HermesLaunch, raw []byte, stdout, stderr io.Writer) int {
	start := time.Now()
	q := dlog.Queue(8)
	defer q.Close(decideLogWait)
	plan, planErr := readDecidePlan(l.Plan, l.PlanSum)
	vv := plan.VendorVersion
	if vv == "" {
		vv = "unknown"
	}
	v := string(adapter.Hermes)
	startID := q.Log(diag.Rec{Event: diag.EvDecideStarted, Vendor: v, Attrs: map[string]any{"vendor_version": vv}})
	end := func(code diag.Code, decision, group string) {
		q.Log(diag.Rec{Event: diag.EvDecideEnded, Code: code, Vendor: v, Cause: startID, Duration: max(time.Since(start), time.Millisecond),
			Attrs: map[string]any{"vendor_version": vv, "decision": decision, "group": group}})
	}
	if planErr != nil {
		code := diag.CodeDecidePlan
		if errors.Is(planErr, policyhook.ErrPlanChanged) {
			code = diag.CodeDecidePlanChanged
		}
		calls, ok := policyhook.HermesCalls(raw, "")
		d := policyhook.PlanFailed(!ok || len(calls) > 0, "")
		_, _ = stdout.Write(policyhook.HermesOutput(d))
		end(code, string(d.Verdict), "")
		fmt.Fprintf(stderr, "aggy hook hermes: the launch's decision plan is missing, unreadable, or changed, so this call is asked about %s\n", dlog.Tag(code))
		return 0
	}
	d := policyhook.JudgeHermes(plan, raw)
	_, _ = stdout.Write(policyhook.HermesOutput(d))
	end(decideCodes[d.Reason], string(d.Verdict), d.Group)
	return 0
}

// errNoHermesLaunch is a hook whose launch cannot be told.
var errNoHermesLaunch = errors.New("no aggy launch for this Hermes session")

// hermesFind finds the live launch a hook belongs to, in aggy's index: the
// one that recorded this session id; else the oldest one with none yet,
// which the id now claims; else, when only one launch is live in the profile,
// that one, whose session moved (moved: /new or /resume inside Hermes).
// Anything else, two live launches with neither id among them included, is
// errNoHermesLaunch. Rows whose supervisor is gone are removed.
func hermesFind(ctx context.Context, profile, id string) (l store.HermesLaunch, moved bool, err error) {
	if adapter.ValidSessionID(adapter.Hermes, id) != nil {
		return l, false, errNoHermesLaunch
	}
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return l, false, err
	}
	defer func() { _ = s.Close() }()
	dir, err := home.Dir()
	if err != nil {
		return l, false, err
	}
	runDir := filepath.Join(dir, "run")
	ls, err := s.HermesLaunches(ctx, profile)
	if err != nil {
		return l, false, err
	}
	var live []store.HermesLaunch
	for _, x := range ls {
		if hermesGone(runDir, x, time.Now()) {
			_ = s.DeleteHermesLaunch(ctx, x.ID)
			continue
		}
		live = append(live, x)
	}
	for _, x := range live {
		if x.SessionID == id {
			return x, false, nil
		}
	}
	for _, x := range live {
		if x.SessionID == "" {
			if ok, err := s.ClaimHermesSession(ctx, x.ID, "", id); err == nil && ok {
				x.SessionID = id
				return x, false, nil
			}
		}
	}
	if len(live) == 1 {
		x := live[0]
		if ok, err := s.ClaimHermesSession(ctx, x.ID, x.SessionID, id); err == nil && ok {
			x.SessionID = id
			return x, true, nil
		}
	}
	return l, false, errNoHermesLaunch
}

// hermesSend tells the launch's supervisor of an event, with the launch's
// token in place of a lineage, and the session id as the whole payload.
func hermesSend(runDir string, l store.HermesLaunch, event, id string) error {
	payload, _ := json.Marshal(map[string]string{"session_id": id})
	resp, err := supervisor.Send(runDir, filepath.Join(runDir, l.ParentID+".sock"),
		supervisor.Request{Type: supervisor.ReqHookEvent, Vendor: string(adapter.Hermes), Event: event, Payload: payload, Token: l.Token}, hookTimeout)
	if err == nil && !resp.OK {
		err = errors.New(resp.Error)
	}
	return err
}
