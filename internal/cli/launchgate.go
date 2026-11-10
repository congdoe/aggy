package cli

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/safefs"
)

// gated is what gateLaunch hands the launch: the CLI's words without the
// person's --settings, which go in layer, to merge under aggy's own.
type gated struct {
	extra []string
	layer map[string]any
	// settingsFile says a --settings value was a file, whose content must not
	// be copied onto argv (writeSettingsFile).
	settingsFile bool
}

// stderrTerminal reports whether aggy's stderr is a terminal; a test sets it.
var stderrTerminal = func() bool { return isTerminal(os.Stderr) }

// gateLaunch is the one check every path that starts a vendor CLI runs before
// anything starts or is recorded (PR #124 review): on screen (launchOnce), in
// print mode and the flags that run like it (runPrint), as given (runBare), and
// a management subcommand (runSubcommand). It refuses, in order:
//   - a borrow child (AGGY_DELEGATED): it may not start another vendor CLI;
//   - a vendor the repo's allowed_vendors leaves out;
//   - a folder flag that reaches a home's keys or a vendor's config (gateDirFlags);
//   - a Claude --settings layer that defeats the policy (claudeSettings), or
//     sets a credential;
//   - --no-policy, or any flag only it lets through, unless a person asked for
//     it: not a session's own tool shell, and a terminal on stdin or stderr
//     (stderr, so git diff | aggy -p --no-policy works from a person's shell);
//   - shared rules that cannot be read, when rules is set (the policy cannot be
//     built without them).
//
// t's words must already be sorted (ParseWith, which refuses the loosening
// flags without --no-policy and the ones that clash); gateLaunch moves what
// --no-policy lets through into t.Loosens and out of t.SafeExtra.
func gateLaunch(ctx context.Context, t *target, v adapter.Vendor, cwd string, cfg config.Config, rules bool) (gated, diag.Code, error) {
	out := gated{extra: t.Extra}
	if os.Getenv(borrow.Delegated) != "" {
		return out, diag.CodeOneHop, errors.New("a borrow child cannot start a vendor CLI through aggy (" + borrow.Delegated + " is set)")
	}
	if _, err := allowedIn(cfg, cwd, v); err != nil {
		return out, launchCode(err, diag.CodeConfig), err
	}
	if err := gateDirFlags(t, cwd); err != nil {
		return out, diag.CodeFlagLoosens, err
	}
	if v == adapter.Claude {
		layer, rest, loosens, fromFile, err := claudeSettings(t.Extra, cwd, t.NoPolicy)
		if err != nil {
			return out, settingsCode(err), err
		}
		out.extra, out.layer, out.settingsFile = rest, layer, fromFile
		if loosens {
			t.Loosens = append(t.Loosens, "--settings")
			_, t.SafeExtra = adapter.TakeFlag(t.SafeExtra, "--settings")
		}
	}
	if t.NoPolicy || len(t.Loosens) > 0 {
		if err := loosenGate(os.Getenv, stdinTerminal() || stderrTerminal()); err != nil {
			return out, diag.CodeNoTerminal, err
		}
	}
	if rules {
		if s := loadPermissions(ctx, cwd, cfg); s.FileErr != nil {
			return out, diag.CodePermBadFile, s.FileErr
		}
	}
	return out, diag.Code{}, nil
}

// loosenGate refuses --no-policy, or a flag only it lets through, that a
// model's shell asks for: a session's own tool shell (aggy's variables), or no
// terminal at all. Like personGate it guards against mistakes, not a
// determined model (docs/design.md, section 10).
func loosenGate(getenv func(string) string, tty bool) error {
	for _, v := range []string{"AGGY_SOCKET", "AGGY_PARENT", "AGGY_VENDOR"} {
		if getenv(v) != "" {
			return errors.New("--no-policy and the flags only it lets through are for you at a terminal, and this looks like a session's own tool shell, which a model can use; run it yourself outside the session")
		}
	}
	if !tty {
		return errors.New("--no-policy and the flags only it lets through are for you at a terminal, and neither stdin nor stderr is one")
	}
	return nil
}

// writeSettingsFile writes Claude's settings layer for req to a 0600 file in
// dir, through safefs, and points req at it, when g's --settings was a file:
// that file's content must not land on argv, where ps shows it (PR #124
// review). The file goes with dir when the run ends.
func writeSettingsFile(req *adapter.InteractiveRequest, g gated, dir string) error {
	if !g.settingsFile {
		return nil
	}
	js, ok, err := claude.SettingsJSON(*req)
	if err != nil || !ok {
		return err
	}
	// The folder is made when the shared memory goes in it (addMemory), so it
	// may not be there yet; the launch removes it either way.
	if _, err := safefs.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := safefs.Mkdir(dir, 0o700); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	path := filepath.Join(dir, "settings.json")
	if err := safefs.WriteFile(path, js, 0o600); err != nil {
		return err
	}
	req.SettingsPath = path
	return nil
}

// settingsNotes are the launch lines for the keys of the person's --settings
// that aggy's own layer replaces.
func settingsNotes(req adapter.InteractiveRequest) []string {
	var out []string
	for _, k := range claude.SettingsOverrides(req) {
		out = append(out, "your --settings "+k+" is replaced by aggy's for this run")
	}
	return out
}
