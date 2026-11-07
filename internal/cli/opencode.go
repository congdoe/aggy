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
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
)

// maxInstruction caps one instruction file aggy copies for OpenCode.
const maxInstruction = 512 << 10

// openCodeSetup is what launching OpenCode decided.
type openCodeSetup struct {
	launch adapter.OpenCodeLaunch
	warns  []string
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
}

// prepareOpenCode builds the launch's folder (docs/design.md, section 15):
// its config, an empty OPENCODE_CONFIG_DIR, the instruction copies, and
// the plugin. The folder is per launch, so another session's guard in the
// same repo is never disturbed, and nothing planted in one reaches a later
// launch. A launch that cannot be safely set up is an error, whatever
// [policy] says: without --standalone or the guard's binary, none of
// aggy's rules would apply.
func prepareOpenCode(in openCodeInput) (openCodeSetup, error) {
	root, _ := repoRoot(in.cwd)
	trusted, trustWarn := openCodeTrusted(root)
	var out openCodeSetup
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
	providers := in.cfg.OpenCodeProviders()
	if len(providers) == 0 {
		return out, errors.New("no OpenCode provider is left for this repo: its allowed_vendors and [opencode] providers have none in common")
	}
	if in.model != "" {
		p, _, ok := strings.Cut(in.model, "/")
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
	for _, d := range []string{in.dir, base, configDir, pluginDir, notesDir} {
		if err := safefs.Mkdir(d, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return out, err
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
	notes, warns, err := openCodeInstructions(root, notesDir, in.memory, in.brief)
	out.warns = append(out.warns, warns...)
	if err != nil {
		return out, err
	}
	data, err := parity.OpenCodeConfig{
		Trusted: trusted, Providers: providers, Model: in.model, Instructions: notes,
		Plugin: pluginDir, Aggy: bin, Root: root, NoPolicy: in.noPolicy,
	}.Render()
	if err != nil {
		return out, err
	}
	out.launch.ConfigFile = filepath.Join(base, "opencode.json")
	out.launch.ConfigDir = configDir
	if err := safefs.WriteFile(out.launch.ConfigFile, data, 0o600); err != nil {
		return out, err
	}
	return out, nil
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

// openCodeUntrustedNote says what an untrusted repo gets.
func openCodeUntrustedNote(cwd string) string {
	root, _ := repoRoot(cwd)
	return fmt.Sprintf("this repo is not trusted for OpenCode, so it gets opencode mini, the whitelist, and a model chosen at launch. For anything more, use Claude Code here, or aggy trust --opencode %s", root)
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
  aggy gives guardrails against mistakes, not protection from a hostile repo or a
  tricked model. A model's shell runs as you with no OS sandbox.
  Full auto ([opencode] auto = true, only in a repo you ran aggy trust --opencode on)
  has no classifier like Claude's: whatever is not denied runs with no prompt.
`

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
