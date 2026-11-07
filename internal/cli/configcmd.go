package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/termsafe"
)

const configUsage = `Usage: aggy config <command>

aggy's own settings. A change applies at the next launch or switch.

  list              Show the settings below and their values
  set KEY VALUE     Change one, for you at a terminal

  KEY                  VALUE
  default-harness      claude, codex, grok, or opencode; none lets aggy pick
  model.<harness>      a model name; none leaves it to the CLI
  permission-mode      default, acceptEdits, plan, auto, or dontAsk; none leaves
                       the mode in Claude's settings in force. OpenCode keeps its
                       own (docs/design.md); Codex has no such modes.
  harness.<harness>    on or off. Off, aggy neither opens it nor borrows from it,
                       and a repo cannot turn it back on.

  --dry-run         show the change and make none

Settings live in ~/.aggy/config.toml, which you can also edit by hand; aggy doctor
reports a mistake by line. aggy unwire undoes a change made here. A session's
tool shell is refused.
`

// configKey is what a key of aggy config set edits.
type configKey struct {
	setting config.Setting
	// value checks a person's value and returns it as TOML ("" removes the key).
	value func(string) (string, error)
}

// parseConfigKey finds a key.
func parseConfigKey(key string) (configKey, error) {
	vendor := func(name string) (adapter.Vendor, error) {
		v := adapter.Vendor(name)
		if !slices.Contains(adapter.Known(), v) {
			return "", fmt.Errorf("%q is not a harness: use %s", name, knownVendorNames())
		}
		return v, nil
	}
	switch key {
	case "default-harness":
		return configKey{config.Setting{Key: "default_vendor"}, func(val string) (string, error) {
			if val == "none" {
				return "", nil
			}
			v, err := vendor(val)
			if err != nil {
				return "", err
			}
			return config.Quote(string(v)), nil
		}}, nil
	case "permission-mode":
		return configKey{config.Setting{Table: "permissions", Key: "mode"}, func(val string) (string, error) {
			if val == "none" {
				return "", nil
			}
			m, err := permissions.CheckMode(val)
			if err != nil {
				return "", err
			}
			return config.Quote(m), nil
		}}, nil
	}
	head, name, ok := strings.Cut(key, ".")
	if ok && (head == "model" || head == "harness") {
		v, err := vendor(name)
		if err != nil {
			return configKey{}, err
		}
		table := "vendors." + string(v)
		if head == "model" {
			return configKey{config.Setting{Table: table, Key: "model"}, func(val string) (string, error) {
				if val == "none" {
					return "", nil
				}
				if err := adapter.ValidModel(val); err != nil {
					return "", err
				}
				return config.Quote(val), nil
			}}, nil
		}
		return configKey{config.Setting{Table: table, Key: "off"}, func(val string) (string, error) {
			switch val {
			case "on":
				return "", nil
			case "off":
				return "true", nil
			}
			return "", errors.New("use on or off")
		}}, nil
	}
	return configKey{}, fmt.Errorf("%q is not a setting aggy config changes", key)
}

func knownVendorNames() string {
	var names []string
	for _, v := range adapter.Known() {
		names = append(names, string(v))
	}
	return strings.Join(names, ", ")
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, configUsage)
		return 0
	}
	switch args[0] {
	case "list":
		if len(args) > 1 {
			return cmdUsage(stderr, "config list", "takes no arguments", configUsage)
		}
		return configList(stdout, stderr)
	case "set":
		return configSet(args[1:], stdout, stderr)
	}
	return cmdUsage(stderr, "config", fmt.Sprintf("unknown command %q", args[0]), configUsage)
}

func configList(stdout, stderr io.Writer) int {
	cfg, err := config.LoadDefault()
	if err != nil {
		return cmdErr(stderr, "config list", diag.CodeConfig, err)
	}
	orNone := func(s string) string {
		if s == "" {
			return "(none)"
		}
		return termsafe.Field(s)
	}
	fmt.Fprintf(stdout, "default-harness    %s\n", orNone(string(cfg.DefaultVendor)))
	fmt.Fprintf(stdout, "permission-mode    %s\n", orNone(cfg.Permissions.Mode))
	for _, v := range adapter.Known() {
		fmt.Fprintf(stdout, "model.%-12s %s\n", v, orNone(cfg.Vendors.For(v).Model))
	}
	for _, v := range adapter.Known() {
		state := "on"
		if cfg.Vendors.For(v).Off {
			state = "off"
		}
		fmt.Fprintf(stdout, "harness.%-10s %s\n", v, state)
	}
	return 0
}

func configSet(args []string, stdout, stderr io.Writer) int {
	const name = "config set"
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	dryRun := fl.Bool("dry-run", false, "")
	var pos []string
	for rest := args; ; {
		if err := fl.Parse(rest); err != nil {
			return cmdUsage(stderr, name, err.Error(), configUsage)
		}
		if fl.NArg() == 0 {
			break
		}
		pos = append(pos, fl.Arg(0))
		rest = fl.Args()[1:]
	}
	if len(pos) != 2 {
		return cmdUsage(stderr, name, "want a key and a value", configUsage)
	}
	fail := func(code diag.Code, msg string) int {
		refuseWith(writerFor(*dryRun), context.Background(), stderr, name, diag.EvConfigRefused, code, msg, diag.Rec{})
		return 1
	}
	key, err := parseConfigKey(pos[0])
	if err != nil {
		return fail(diag.CodeConfigBadValue, err.Error())
	}
	value, err := key.value(pos[1])
	if err != nil {
		return fail(diag.CodeConfigBadValue, err.Error())
	}
	path, err := config.Path()
	if err != nil {
		return fail(diag.CodeStore, err.Error())
	}
	hash := patch.NoFile
	src, err := safefs.ReadConfig(path, config.MaxFileSize)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		src = nil
	case err != nil:
		return fail(diag.CodeConfig, err.Error())
	default:
		hash = patch.Hash(src)
	}
	transform := func(b []byte) ([]byte, error) {
		out, err := config.SetValue(b, key.setting, value)
		if err != nil {
			return nil, err
		}
		// The result must load: a mistake here would stop every aggy command.
		if _, err := config.Parse(string(out)); err != nil {
			return nil, err
		}
		return out, nil
	}
	out, err := transform(src)
	if err != nil {
		return fail(diag.CodeConfigBadValue, err.Error())
	}
	if string(out) == string(src) {
		fmt.Fprintf(stdout, "%s is already %s.\n", pos[0], termsafe.Field(pos[1]))
		return 0
	}
	ps := []patch.Patch{{File: path, ExpectedHash: hash, Mode: 0o600, Transform: transform}}
	if *dryRun {
		if _, err := manifest.Preview(stdout, ps); err != nil {
			return 1
		}
		return 0
	}
	if err := personGate(os.Getenv); err != nil {
		return fail(diag.CodeNotAtKeyboard, err.Error())
	}
	runID, err := applyPatches(context.Background(), ps)
	if err != nil {
		return fail(diag.CodeConfig, err.Error())
	}
	fmt.Fprintf(stdout, "Done (run %s). It applies at the next launch or switch. aggy unwire --run %s undoes it, once any change made after it is undone.\n", runID, runID)
	return 0
}
