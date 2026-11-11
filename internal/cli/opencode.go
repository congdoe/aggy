package cli

import (
	"context"
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
	"github.com/congdoe/aggy/internal/adapter/opencode"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/keyring"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/policyhook"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// maxInstruction caps one instruction file aggy copies for OpenCode.
const maxInstruction = 512 << 10

// openCodeSetup is what launching OpenCode decided.
type openCodeSetup struct {
	launch adapter.OpenCodeLaunch
	warns  []string
	root   string // the repo root, by its real path
	// keys are the keys read for this launch, written only by writeKeys,
	// once the launch is recorded (#138 review L2), and zeroed by wipeKeys.
	keys []openCodeKey
	// dir is the launch's own folder, and base its opencode folder.
	dir, base string
}

// wipeKeys zeroes the keys read for the launch; the launch defers it.
func (s *openCodeSetup) wipeKeys() {
	for _, k := range s.keys {
		k.value.Wipe()
	}
	s.keys = nil
}

// openCodeInput is what a launch hands prepareOpenCode.
type openCodeInput struct {
	cfg      config.Effective
	caps     []string
	cwd      string // the child's working directory
	dir      string // the launch's private folder, removed when the CLI exits
	model    string
	memory   string // framed shared memory, or ""
	brief    string // the brief's text, or ""
	noPolicy bool
	rules    permissions.Set         // the person's shared rules, for this repo
	policy   []parity.PolicyDecision // the safety policy as decided; nil is every rule a deny
	// plan is the launch's policy plan, for the decision hook's plan file;
	// version is the CLI's version from the probe, or "".
	plan    parity.PolicyPlan
	version string
	ids     parity.ReadIdentity // read pre-approvals' identities, from the index
	// keys are the keys aggy key stored, by name and provider (T6931); person
	// says a person started this launch at a terminal (trustPerson), the only
	// launch a key is read for; readKey reads one from the OS keychain.
	keys    []store.Key
	person  bool
	readKey func(k store.Key) (*keyring.Value, error)
}

// prepareOpenCode builds the launch's folder (docs/design.md, section 15):
// its config, an empty OPENCODE_CONFIG_DIR, the instruction copies, and
// the plugin. The folder is per launch, so another session's guard in the
// same repo is never disturbed, and nothing planted in one reaches a later
// launch. A launch that cannot be safely set up is an error, whatever
// [policy] says: without --standalone or the guard's binary, none of
// aggy's rules would apply.
func prepareOpenCode(in openCodeInput) (openCodeSetup, error) {
	var out openCodeSetup
	// By its real path: a link in a trusted repo to another folder is that
	// folder, whose plugins would run.
	real, err := safefs.RealPath(in.cwd)
	if err != nil {
		return out, err
	}
	root, _ := repoRoot(real)
	out.root = root
	trusted, trustWarn := openCodeTrusted(root)
	if trustWarn != "" {
		out.warns = append(out.warns, trustWarn)
	}
	out.launch.Mini = !trusted
	if why := opencode.Reachable(in.caps, out.launch.Mini); why != "" {
		return out, errors.New("the safety policy cannot be applied: " + why)
	}
	if in.cfg.OpenCode.Auto {
		switch {
		case !trusted:
			out.warns = append(out.warns, "[opencode] auto applies only in a repo trusted for OpenCode, so this one keeps the whitelist")
		case !slices.Contains(in.caps, probe.CapAuto):
			out.warns = append(out.warns, "opencode --help does not list --auto, so this session keeps the whitelist")
		default:
			out.launch.Auto = true
		}
	}
	// T6977: OpenCode's own provider needs the person's setting, the repo's
	// allowed_vendors, and this folder's trust list row, all three.
	here := openCodeFreeHere(real)
	providers := openCodeFreeProviders(in.cfg, here)
	out.warns = append(out.warns, openCodeFreeWarns(in.cfg, here, root)...)
	// T6931: the keys aggy key stored, read now for this launch only, each
	// adding its provider. They are written to their files only once the
	// launch is recorded (writeKeys); their bytes are zeroed on any refusal
	// below, and by the caller's wipeKeys otherwise.
	keyed, keyWarns := openCodeKeys(in)
	handed := false
	defer func() {
		if !handed {
			for _, k := range keyed {
				k.value.Wipe()
			}
		}
	}()
	out.warns = append(out.warns, keyWarns...)
	for _, k := range keyed {
		if !slices.Contains(providers, k.provider) {
			providers = append(providers, k.provider)
		}
	}
	if len(providers) == 0 {
		msg := "no OpenCode provider is left for this repo: its allowed_vendors and [opencode] providers have none in common"
		if in.cfg.OpenCode.FreeModels && !slices.Contains(in.cfg.OpenCodeProviders(), config.OpenCodeFreeProvider) {
			msg += ", and it does not list " + config.OpenCodeFreeRepoName + " for OpenCode's own models"
		} else if in.cfg.OpenCode.FreeModels && !here {
			msg += ", and " + freeModelsRow + " is not ticked for this folder in aggy's trust list (aggy trust " + root + ")"
		}
		return out, errors.New(msg)
	}
	// TODO(T6932): the picker's model list groups by vendor, so /switch names
	// an OpenCode model as provider/model by hand; group it by provider.
	if in.model != "" {
		p, _, ok := strings.Cut(in.model, "/")
		if ok && p == config.OpenCodeFreeProvider && !slices.Contains(providers, p) {
			// The launch returns before printing warns, so the reason goes here.
			return out, openCodeFreeRefusal(in.cfg, in.model, here, root)
		}
		if !ok || !slices.Contains(providers, p) {
			return out, fmt.Errorf("model %s is not on a provider allowed here (%s)", in.model, strings.Join(providers, ", "))
		}
	}
	bin, err := aggyBin()
	if err != nil {
		return out, fmt.Errorf("the guard cannot run: %w", err)
	}
	base := filepath.Join(in.dir, "opencode")
	configDir := filepath.Join(base, "config")
	pluginDir := filepath.Join(base, "plugin")
	notesDir := filepath.Join(base, "instructions")
	// The launch's folder may be there already (its memory copy); the folders
	// inside it are new to this launch, so one already there is refused
	// (#138 review M1): someone else made it.
	if err := safefs.Mkdir(in.dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return out, err
	}
	for _, d := range []string{base, configDir, pluginDir, notesDir} {
		if err := safefs.Mkdir(d, 0o700); err != nil {
			return out, fmt.Errorf("cannot make this launch's own folder %s: %w", d, err)
		}
	}
	files, err := opencode.Plugin()
	if err != nil {
		return out, err
	}
	for _, f := range files {
		if err := safefs.WriteFile(filepath.Join(pluginDir, f.Name), f.Data, 0o600); err != nil {
			return out, err
		}
	}
	keyFiles := openCodeKeyPaths(base, keyed)
	notes, warns, err := openCodeInstructions(root, notesDir, in.memory, in.brief)
	out.warns = append(out.warns, warns...)
	if err != nil {
		return out, err
	}
	shared, skipped := parity.OpenCodeShared(in.rules)
	if n := len(skipped); n > 0 {
		out.warns = append(out.warns, skippedNote(n, "OpenCode", !in.rules.Trusted))
	}
	// The decision hook's plan (T6976), for an on-screen launch with the policy.
	decide, sum := "", ""
	if !in.noPolicy && in.plan.OnScreen {
		// A read pre-approval needs the repo trusted for OpenCode too.
		rules := in.rules
		rules.Trusted = rules.Trusted && trusted
		d := policyhook.NewDecidePlan(string(adapter.OpenCode), in.version, out.launch.Auto, in.plan, parity.PreApprovedFrom(rules, root, in.ids))
		d.Root, d.Protected = root, protectedPaths()
		if decide, sum, err = writeDecidePlan(base, d); err != nil {
			return out, err
		}
	}
	data, err := parity.OpenCodeConfig{
		Trusted: trusted, Providers: providers, Model: in.model, Instructions: notes,
		Plugin: pluginDir, Aggy: bin, Root: root, NoPolicy: in.noPolicy, Auto: out.launch.Auto,
		Shared: shared, Policy: in.policy, Decide: decide, DecideSum: sum, KeyFiles: keyFiles,
	}.Render()
	if err != nil {
		return out, err
	}
	out.launch.ConfigFile = filepath.Join(base, "opencode.json")
	out.launch.ConfigDir = configDir
	if err := safefs.WriteFile(out.launch.ConfigFile, data, 0o600); err != nil {
		return out, err
	}
	out.keys, out.dir, out.base, handed = keyed, in.dir, base, true
	return out, nil
}

// openCodeKeyPaths are the files the keys will be in, by provider, for the
// config: base/keys/<provider>.
func openCodeKeyPaths(base string, keyed []openCodeKey) map[string]string {
	if len(keyed) == 0 {
		return nil
	}
	files := map[string]string{}
	for _, k := range keyed {
		files[k.provider] = filepath.Join(base, "keys", k.provider)
	}
	return files
}

// writeKeys writes each key read for this launch to its own 0600 file in
// base/keys (base is in the launch's own 0700 folder under aggy's run folder,
// removed when the CLI exits and swept at the next launch after a crash,
// sweepMemoryDirs), and zeroes the key's bytes. OpenCode reads the file at
// config load, through "{file:<path>}" in the provider's apiKey
// (docs/design.md, section 15; docs/verification.md, "Key file"). The launch
// calls it only once it is recorded (#138 review L2), so a launch planned
// again leaves no key file behind. It returns the launch's lines.
//
// #138 review M1: the keys folder must be new, made with the chain from the
// launch's folder through descriptors that follow no link; each file must be
// new (O_EXCL, never through a link) and, once written, a regular file of
// this user's, mode 0600, with one link, in that very folder. On any doubt
// every key file written is removed and the launch refused.
func (s *openCodeSetup) writeKeys() (lines []string, err error) {
	defer s.wipeKeys()
	if len(s.keys) == 0 {
		return nil, nil
	}
	dir := filepath.Join(s.base, "keys")
	var written []string
	defer func() {
		if err != nil {
			for _, p := range written {
				if fi, lerr := safefs.Lstat(p); lerr == nil {
					_ = safefs.RemoveSame(p, fi)
				}
			}
		}
	}()
	if err := safefs.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot make the folder OpenCode reads keys from, so no key is handed over: %w", err)
	}
	dirFi, err := safefs.Lstat(s.dir)
	if err != nil || !ownPrivateDir(dirFi) {
		return nil, errors.New("this launch's folder is not a private folder of yours, so no key is handed to OpenCode")
	}
	rel, err := filepath.Rel(s.dir, dir)
	if err != nil {
		return nil, err
	}
	keysFi, err := safefs.MkdirChain(s.dir, dirFi, strings.Split(rel, string(filepath.Separator)), 0o700)
	if err != nil || !ownPrivateDir(keysFi) {
		return nil, fmt.Errorf("the folder OpenCode reads keys from is not this launch's own, so no key is handed over: %v", err)
	}
	for _, k := range s.keys {
		p := filepath.Join(dir, k.provider)
		err := safefs.WriteFileNoFollowIn(keysFi, p, k.value.Bytes(), 0o600, nil)
		k.value.Wipe()
		if err != nil {
			return lines, fmt.Errorf("cannot write the file OpenCode reads key %s from, so no key is handed over: %w", k.name, err)
		}
		written = append(written, p)
		fi, err := safefs.Lstat(p)
		now, derr := safefs.Lstat(dir)
		if err != nil || derr != nil || !ownPrivateFile(fi) || !os.SameFile(now, keysFi) {
			return lines, fmt.Errorf("the file OpenCode would read key %s from is not the private file aggy wrote, so no key is handed over", k.name)
		}
		dlog.Log(context.Background(), diag.Rec{Event: diag.EvKeyPassed, Msg: "key passed", Vendor: string(adapter.OpenCode),
			Attrs: map[string]any{"provider": k.provider}})
		lines = append(lines, fmt.Sprintf("OpenCode gets your key %s for %s, in a 0600 file in this launch's own folder, removed when OpenCode exits (after a crash, at the next aggy launch)", k.name, k.provider))
	}
	lines = append(lines, "any process running as you (OpenCode's shell, tools, and plugins among them) can read a key it was given while OpenCode runs, and so can anything that asks OpenCode's local server for its config")
	return lines, nil
}

// openCodeInstructions copies what the session should read into dir and
// returns the copies' absolute paths. OpenCode reads no CLAUDE.md, and
// project config off drops AGENTS.md (fake model), so aggy opens them itself,
// refusing a symlink: passed by repo path, one to a canary reached the
// request body (fake model). A file that cannot be copied is a warning: the
// session goes on without it, and says so.
func openCodeInstructions(root, dir, memory, brief string) (paths, warns []string, err error) {
	add := func(name, text string) error {
		if text == "" {
			return nil
		}
		p := filepath.Join(dir, name)
		if err := safefs.WriteFile(p, []byte(text), 0o600); err != nil {
			return err
		}
		paths = append(paths, p)
		return nil
	}
	for _, f := range []string{"AGENTS.md", "CLAUDE.md"} {
		text, err := readInstruction(filepath.Join(root, f))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				warns = append(warns, fmt.Sprintf("%s not passed to OpenCode: %v", f, err))
			}
			continue
		}
		if err := add(strings.ToLower(f), text); err != nil {
			return nil, warns, err
		}
	}
	if err := add("memory.md", memory); err != nil {
		return nil, warns, err
	}
	if brief != "" {
		brief = "This is aggy's handoff brief for this session. Continue the work it describes.\n\n" + brief
	}
	if err := add("brief.md", brief); err != nil {
		return nil, warns, err
	}
	return paths, warns, nil
}

// readInstruction reads one repo file through safefs.OpenRegular: never
// through a symlink, never a FIFO or device, and capped.
func readInstruction(path string) (string, error) {
	f, err := safefs.OpenRegular(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxInstruction+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxInstruction {
		return "", fmt.Errorf("it is over %d bytes", maxInstruction)
	}
	return parity.CleanMemory(string(b)), nil
}

// openCodeUntrustedNote says what an untrusted repo gets. root is the real
// one, as trust records it: a link to a repo must name the repo it leads to.
func openCodeUntrustedNote(root string, held int) string {
	note := "this repo is not trusted for OpenCode, so it gets opencode mini, the whitelist, and a model chosen at launch. For anything more, use Claude Code here, or aggy trust --opencode " + root
	switch held {
	case 0:
		return note
	case 1:
		return note + ". Its .claude/settings.local.json has 1 allow rule aggy holds until then; aggy trust " + root + " or aggy trust --opencode " + root + " applies it"
	}
	return fmt.Sprintf("%s. Its .claude/settings.local.json has %d allow rules aggy holds until then; aggy trust %s or aggy trust --opencode %s applies them", note, held, root, root)
}

// openCodeTranscript reads a session's turns with `session export`, the only
// way aggy reads one: sessions sit in opencode.db with the sign-ins.
func openCodeTranscript(ctx context.Context, ref adapter.SessionRef, turns int) (adapter.Transcript, error) {
	path, err := probe.LookPath(opencode.Bin)
	if err != nil {
		return adapter.Transcript{}, fmt.Errorf("%w: %v", adapter.ErrNoTranscript, err)
	}
	empty, err := safefs.MkdirTemp("", "aggy-opencode-")
	if err != nil {
		return adapter.Transcript{}, err
	}
	defer func() { _ = safefs.RemoveAll(empty) }()
	cwd := ref.Cwd
	if cwd == "" {
		cwd = empty
	}
	cmd, err := opencode.ExportCommand(path, ref.ID, cwd, empty)
	if err != nil {
		return adapter.Transcript{}, err
	}
	out, err := execrun.OpenCodeRunner(openCodeExportTimeout)(ctx, cmd)
	if err != nil {
		return adapter.Transcript{}, fmt.Errorf("%w: %v", adapter.ErrNoTranscript, err)
	}
	if out.ExitCode != 0 {
		return adapter.Transcript{}, fmt.Errorf("%w: opencode session export exited %d", adapter.ErrNoTranscript, out.ExitCode)
	}
	return opencode.ParseExport(out.Stdout, turns)
}

// openCodeManagedPaths are the system and managed config OpenCode 2.2.5
// reads, whose policies outrank aggy's and no launch setting removes
// (packages/core/src/config/discovery.ts at v2.2.5). The per-user plist is
// added by openCodeManagedFindings.
var openCodeManagedPaths = []string{
	"/Library/Application Support/opencode",
	"/Library/Managed Preferences/ai.opencode.managed.plist",
	"/etc/opencode",
}

// openCodeManagedFindings warns about system and managed OpenCode config,
// by whether it exists only: aggy never reads it.
func openCodeManagedFindings() []checkup.Finding {
	paths := slices.Clone(openCodeManagedPaths)
	if u := os.Getenv("USER"); u != "" && !strings.ContainsAny(u, "/\\") {
		paths = append(paths, "/Library/Managed Preferences/"+u+"/ai.opencode.managed.plist")
	}
	var out []checkup.Finding
	for _, p := range paths {
		if _, err := safefs.Lstat(p); err == nil {
			out = append(out, checkup.Finding{Code: codeOpenCodeManaged, Level: checkup.LevelWarn, Vendor: adapter.OpenCode, Path: p,
				Message: "system or managed OpenCode config exists: its policies outrank aggy's under any launch"})
		}
	}
	return out
}

// codeOpenCodeManaged is the finding above's stable code.
const codeOpenCodeManaged = "opencode-managed-config"

// openCodeSetupNote is what setup says about OpenCode, the design's own
// words (docs/design.md, section 15).
const openCodeSetupNote = `  Under aggy, your global OpenCode config does nothing: each launch brings its own.
  aggy runs it when you name it (aggy opencode). To offer it in the switch
  list, add it to [switch] order in aggy's config.
  aggy gives guardrails against mistakes, not protection from a hostile repo or a
  tricked model. A model's shell runs as you with no OS sandbox.
  Full auto ([opencode] auto = true, only in a repo you ran aggy trust --opencode on)
  has no classifier like Claude's: whatever is not denied runs with no prompt.
`

// openCodeVersionCheck is the version_checks row that says the person has been
// told about this CLI version, once.
const openCodeVersionCheck = "screen_warned"

// openCodeNewVersionNote warns about a CLI newer than aggy was tested on, once
// for each version. It never refuses: the owner wants a vendor update to keep
// working, and on screen the person can see what happens.
func openCodeNewVersionNote(ctx context.Context, s *store.Store, version string) string {
	if c, err := adapter.CompareVersions(version, opencode.Tested.Max); err != nil || c <= 0 {
		return ""
	}
	v := string(adapter.OpenCode)
	if _, err := s.GetVersionCheck(ctx, v, version, openCodeVersionCheck, opencode.Tested.Max); err == nil {
		return ""
	}
	_ = s.PutVersionCheck(ctx, store.VersionCheck{Vendor: v, Version: version, Check: openCodeVersionCheck, Stamp: opencode.Tested.Max, Passed: true, Reason: "told", At: time.Now()})
	return fmt.Sprintf("OpenCode %s is newer than the %s aggy was tested on. It starts anyway, but aggy's rules, guard, and switch may not hold: watch for the guard's warning, and aggy says this once for each version.", version, opencode.Tested.Max)
}

// openCodeExportTimeout bounds one `session export`.
const openCodeExportTimeout = 30 * time.Second

// openCodeTrustFile is where the repos trusted for OpenCode are kept, in
// aggy's state folder, so a repo can never name itself.
func openCodeTrustFile() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "opencode-trust.json"), nil
}

// openCodeTrust is that file's content.
type openCodeTrust struct {
	Repos []string `json:"repos"`
}

// openCodeTrusted reports whether the repo at root is trusted for OpenCode.
// A file that cannot be read trusts nothing, and warns.
func openCodeTrusted(root string) (bool, string) {
	path, err := openCodeTrustFile()
	if err != nil {
		return false, "cannot find the OpenCode trust file: " + err.Error()
	}
	b, err := safefs.ReadConfig(path, config.MaxFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return false, ""
	}
	if err != nil {
		return false, "cannot read the OpenCode trust file, so this repo is not trusted: " + err.Error()
	}
	var t openCodeTrust
	if err := json.Unmarshal(b, &t); err != nil {
		return false, "the OpenCode trust file is not valid, so this repo is not trusted"
	}
	key, err := safefs.CanonicalPath(root)
	if err != nil {
		return false, "cannot resolve this repo's path, so it is not trusted: " + err.Error()
	}
	return slices.Contains(t.Repos, key), ""
}
