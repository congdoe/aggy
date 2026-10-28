// Package probe finds what an installed vendor CLI can do. It runs the CLI's
// --version and --help through an adapter.Runner, records the result in
// the store's probes table, and reuses that record until the CLI at that
// path reports a different version.
//
// A missing or unrecognized feature is recorded as an absent capability,
// never a crash: adapters check capabilities and degrade. Probing never
// reads credential files and never asks a CLI whether it is signed in, so
// Connected stays false here; adapters add sign-in checks.
package probe

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

// Feature is one capability, found in the output of help invocation Help.
type Feature struct {
	Name string // capability name recorded in the store
	Help int    // index into Spec.Help
	// Flag is the option as it appears in help, e.g. "--session-id".
	// With Kind Subcommand it is a subcommand name, found only as an
	// entry of a "Commands:" block. With Kind HelpRuns it is unused: the
	// capability is present when the help invocation itself succeeds.
	Flag string
	Kind MatchKind
}

// MatchKind says how a Feature is found in help output.
type MatchKind int

// Match kinds.
const (
	Option     MatchKind = iota // Flag appears as an option
	Subcommand                  // Flag is listed in a Commands: block
	HelpRuns                    // the help invocation exits 0
)

// Spec says how to probe one vendor's CLI.
type Spec struct {
	Help     [][]string // argument lists that print help, e.g. {"--help"}
	Features []Feature
	// Version matches a --version line; its last group is the version.
	// Nil means versionLine.
	Version *regexp.Regexp
}

// Capability names, shared across vendors.
const (
	CapHeadless       = "headless"        // -p, or codex exec
	CapJSONOutput     = "json-output"     // --output-format json, or exec --json
	CapSessionID      = "session-id"      // --session-id: pre-assign a new session's id
	CapResume         = "resume"          // --resume <id>, or codex resume
	CapForkSession    = "fork-session"    // --fork-session
	CapPermissionMode = "permission-mode" // --permission-mode (plan)
	CapNoAltScreen    = "no-alt-screen"   // --no-alt-screen
	CapTools          = "tools"           // --tools
	CapDisallowTools  = "disallowed-tools"
	CapMaxTurns       = "max-turns"
	CapSandbox        = "sandbox"         // codex exec --sandbox
	CapDisable        = "disable"         // codex exec --disable <feature>
	CapImageInput     = "image-input"     // codex exec -i
	CapConfigOverride = "config-override" // codex -c key=value
)

// Specs holds the probe spec of each vendor, from the flags recorded in
// docs/adapters.md.
var Specs = map[adapter.Vendor]Spec{
	adapter.Claude: {
		Help: [][]string{{"--help"}},
		Features: []Feature{
			{CapHeadless, 0, "-p", Option},
			{CapJSONOutput, 0, "--output-format", Option},
			{CapSessionID, 0, "--session-id", Option},
			{CapResume, 0, "--resume", Option},
			{CapForkSession, 0, "--fork-session", Option},
			{CapPermissionMode, 0, "--permission-mode", Option},
		},
	},
	adapter.Grok: {
		Help: [][]string{{"--help"}},
		Features: []Feature{
			{CapHeadless, 0, "-p", Option},
			{CapJSONOutput, 0, "--output-format", Option},
			{CapSessionID, 0, "--session-id", Option},
			{CapResume, 0, "--resume", Option},
			{CapForkSession, 0, "--fork-session", Option},
			{CapPermissionMode, 0, "--permission-mode", Option},
			{CapNoAltScreen, 0, "--no-alt-screen", Option},
			{CapTools, 0, "--tools", Option},
			{CapDisallowTools, 0, "--disallowed-tools", Option},
			{CapMaxTurns, 0, "--max-turns", Option},
		},
	},
	adapter.Codex: {
		Help: [][]string{{"--help"}, {"exec", "--help"}, {"exec", "resume", "--help"}},
		Features: []Feature{
			{CapHeadless, 0, "exec", Subcommand},
			{CapResume, 2, "", HelpRuns},
			{CapNoAltScreen, 0, "--no-alt-screen", Option},
			{CapConfigOverride, 0, "-c", Option},
			{CapJSONOutput, 1, "--json", Option},
			{CapSandbox, 1, "--sandbox", Option},
			{CapDisable, 1, "--disable", Option},
			{CapImageInput, 1, "-i", Option},
		},
	},
}

// ErrNoVersion means the CLI printed no version.
var ErrNoVersion = errors.New("probe: no version in --version output")

// versionLine matches a whole --version line: an optional program name,
// x.y.z with an optional pre-release or build suffix, and an optional
// parenthesized note such as "(Claude Code)". The exact --version
// formats are not recorded in docs/adapters.md; the program names follow
// how the versions were reported there ("codex-cli 0.164.0", "grok
// 1.0.70"). A Spec can set its own.
var versionLine = regexp.MustCompile(`^(?:[A-Za-z][\w.-]*\s+)?v?(\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?)(?:\s+\([^)]*\))?$`)

// ParseVersion returns the version from the last line of --version output
// that matches re (versionLine when nil). Only whole lines anchored at
// their start count, so a version mentioned in a notice ("update 3.0.0
// available") is not taken. stderr is read when stdout has no match.
func ParseVersion(stdout, stderr []byte, re *regexp.Regexp) (string, error) {
	if re == nil {
		re = versionLine
	}
	for _, out := range [][]byte{stdout, stderr} {
		lines := strings.Split(string(out), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if m := re.FindStringSubmatch(strings.TrimSpace(lines[i])); m != nil {
				return m[len(m)-1], nil
			}
		}
	}
	return "", ErrNoVersion
}

// commandsBlock returns the subcommand names listed under a "Commands:"
// heading: the first word of each indented line until a blank line or the
// next heading.
func commandsBlock(help string) []string {
	var names []string
	in := false
	for _, line := range strings.Split(help, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.EqualFold(strings.TrimSuffix(trimmed, ":"), "commands") && strings.HasSuffix(trimmed, ":"):
			in = true
		case !in:
		case trimmed == "" || !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t"):
			in = false
		default:
			names = append(names, strings.Fields(trimmed)[0])
		}
	}
	return names
}

// hasFlag reports whether help mentions flag as an option or command:
// bounded by start of line, space, comma, or bracket before, and by end,
// space, comma, '=', '<', or bracket after.
func hasFlag(help, flag string) bool {
	re := regexp.MustCompile(`(?m)(?:^|[\s,\[(|])` + regexp.QuoteMeta(flag) + `(?:$|[\s,=<\[\])|])`)
	return re.MatchString(help)
}

// Capabilities returns the names of the spec's features found in the help
// outputs. A help output that is missing (nil) contributes nothing.
func Capabilities(spec Spec, helps [][]byte) []string {
	caps := []string{}
	for _, f := range spec.Features {
		if f.Help >= len(helps) || helps[f.Help] == nil {
			continue
		}
		help := string(helps[f.Help])
		found := false
		switch f.Kind {
		case Option:
			found = hasFlag(help, f.Flag)
		case Subcommand:
			found = slices.Contains(commandsBlock(help), f.Flag)
		case HelpRuns:
			found = true
		}
		if found {
			caps = append(caps, f.Name)
		}
	}
	return caps
}

// Detect probes the CLI at path with no cache: --version, then each help
// invocation. A failed or non-zero --version is an error, since the CLI
// cannot be used. A failed help invocation only removes the capabilities
// it would have shown.
func Detect(ctx context.Context, run adapter.Runner, vendor adapter.Vendor, path string) (adapter.Probe, error) {
	p := adapter.Probe{Vendor: vendor, Path: path, Capabilities: []string{}}
	spec, ok := Specs[vendor]
	if !ok {
		return p, fmt.Errorf("probe: no spec for vendor %q", vendor)
	}
	version, err := Version(ctx, run, path, spec.Version)
	if err != nil {
		return p, err
	}
	p.Version = version
	hs, _ := helps(ctx, run, spec, path)
	p.Capabilities = Capabilities(spec, hs)
	return p, nil
}

// Version runs path --version and parses it with re (nil for the default).
func Version(ctx context.Context, run adapter.Runner, path string, re *regexp.Regexp) (string, error) {
	out, err := run(ctx, adapter.Command{Path: path, Args: []string{"--version"}})
	if err != nil {
		return "", fmt.Errorf("probe: %s --version: %w", path, err)
	}
	if out.ExitCode != 0 {
		return "", fmt.Errorf("probe: %s --version exited %d", path, out.ExitCode)
	}
	return ParseVersion(out.Stdout, out.Stderr, re)
}

// ErrIncomplete matches a probe in which a help invocation failed. Its
// result has the capabilities the other invocations showed, and is not
// cached, so the next probe runs every invocation again.
var ErrIncomplete = errors.New("probe: incomplete")

// helps runs each help invocation. A non-zero exit or a runner error (a
// timeout, a cancelled ctx) leaves that output nil and is reported in the
// error, which matches ErrIncomplete, so the partial result is not cached.
func helps(ctx context.Context, run adapter.Runner, spec Spec, path string) ([][]byte, error) {
	out := make([][]byte, len(spec.Help))
	var errs []error
	for i, args := range spec.Help {
		o, err := run(ctx, adapter.Command{Path: path, Args: args})
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: %s %s: %w", ErrIncomplete, path, strings.Join(args, " "), err))
			continue
		}
		if o.ExitCode != 0 {
			errs = append(errs, fmt.Errorf("%w: %s %s exited %d", ErrIncomplete, path, strings.Join(args, " "), o.ExitCode))
			continue
		}
		// Some CLIs print help on stderr.
		out[i] = append(append([]byte{}, o.Stdout...), o.Stderr...)
	}
	return out, errors.Join(errs...)
}

// Prober probes CLIs and caches the results in the store.
type Prober struct {
	Store *store.Store
	Run   adapter.Runner
	Now   func() time.Time // defaults to time.Now
}

// Probe returns the capabilities of vendor's CLI at path. It always runs
// --version; when the store already has a probe for that path and version
// it returns it (cached is true) without running help again. A new version
// is probed in full and recorded. When any help run fails (a non-zero exit
// or a runner error such as a timeout), Probe returns the degraded result
// with an error matching ErrIncomplete and records nothing, so the next
// call probes again.
func (p *Prober) Probe(ctx context.Context, vendor adapter.Vendor, path string) (res adapter.Probe, cached bool, err error) {
	spec, ok := Specs[vendor]
	if !ok {
		return adapter.Probe{}, false, fmt.Errorf("probe: no spec for vendor %q", vendor)
	}
	version, err := Version(ctx, p.Run, path, spec.Version)
	if err != nil {
		return adapter.Probe{Vendor: vendor, Path: path}, false, err
	}
	row, err := p.Store.LatestProbe(ctx, string(vendor), path, version)
	if err == nil {
		return adapter.Probe{
			Vendor: vendor, Path: path, Version: version, Connected: row.Connected,
			Capabilities: row.Capabilities, ProbedAt: row.ProbedAt,
		}, true, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return adapter.Probe{}, false, err
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	hs, herr := helps(ctx, p.Run, spec, path)
	res = adapter.Probe{
		Vendor: vendor, Path: path, Version: version,
		Capabilities: Capabilities(spec, hs),
		ProbedAt:     now().UTC().Truncate(time.Millisecond),
	}
	if herr != nil {
		// Degraded but usable; not cached, so the next call probes again.
		return res, false, herr
	}
	if err := p.Store.InsertProbe(ctx, store.Probe{
		Vendor: string(vendor), CLIPath: path, Version: version,
		Capabilities: res.Capabilities, ProbedAt: res.ProbedAt,
	}); err != nil {
		return res, false, err
	}
	return res, false, nil
}

// Has reports whether a probe found capability name.
func Has(p adapter.Probe, name string) bool {
	for _, c := range p.Capabilities {
		if c == name {
			return true
		}
	}
	return false
}
