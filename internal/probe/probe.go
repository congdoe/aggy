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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

// Feature is one capability, present when Flag appears as an option in the
// output of help invocation Help.
type Feature struct {
	Name string // capability name recorded in the store
	Help int    // index into Spec.Help
	Flag string // the option as it appears in help, e.g. "--session-id"
}

// Spec says how to probe one vendor's CLI.
type Spec struct {
	Help     [][]string // argument lists that print help, e.g. {"--help"}
	Features []Feature
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
			{CapHeadless, 0, "-p"},
			{CapJSONOutput, 0, "--output-format"},
			{CapSessionID, 0, "--session-id"},
			{CapResume, 0, "--resume"},
			{CapForkSession, 0, "--fork-session"},
			{CapPermissionMode, 0, "--permission-mode"},
		},
	},
	adapter.Grok: {
		Help: [][]string{{"--help"}},
		Features: []Feature{
			{CapHeadless, 0, "-p"},
			{CapJSONOutput, 0, "--output-format"},
			{CapSessionID, 0, "--session-id"},
			{CapResume, 0, "--resume"},
			{CapForkSession, 0, "--fork-session"},
			{CapPermissionMode, 0, "--permission-mode"},
			{CapNoAltScreen, 0, "--no-alt-screen"},
			{CapTools, 0, "--tools"},
			{CapDisallowTools, 0, "--disallowed-tools"},
			{CapMaxTurns, 0, "--max-turns"},
		},
	},
	adapter.Codex: {
		Help: [][]string{{"--help"}, {"exec", "--help"}},
		Features: []Feature{
			{CapHeadless, 0, "exec"},
			{CapResume, 0, "resume"},
			{CapNoAltScreen, 0, "--no-alt-screen"},
			{CapConfigOverride, 0, "-c"},
			{CapJSONOutput, 1, "--json"},
			{CapSandbox, 1, "--sandbox"},
			{CapDisable, 1, "--disable"},
			{CapImageInput, 1, "-i"},
		},
	},
}

// ErrNoVersion means the CLI printed no version.
var ErrNoVersion = errors.New("probe: no version in --version output")

var semver = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?`)

// ParseVersion returns the first x.y.z version in out. When there is none
// it falls back to the first non-empty line, at most 64 bytes, so a format
// change still caches per distinct output instead of failing.
func ParseVersion(out []byte) (string, error) {
	if v := semver.Find(out); v != nil {
		return string(v), nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 64 {
			line = line[:64]
			for !utf8.ValidString(line) {
				line = line[:len(line)-1]
			}
		}
		return line, nil
	}
	return "", ErrNoVersion
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
		if f.Help < len(helps) && helps[f.Help] != nil && hasFlag(string(helps[f.Help]), f.Flag) {
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
	version, err := Version(ctx, run, path)
	if err != nil {
		return p, err
	}
	p.Version = version
	hs, _ := helps(ctx, run, spec, path)
	p.Capabilities = Capabilities(spec, hs)
	return p, nil
}

// Version runs path --version and parses it.
func Version(ctx context.Context, run adapter.Runner, path string) (string, error) {
	out, err := run(ctx, adapter.Command{Path: path, Args: []string{"--version"}})
	if err != nil {
		return "", fmt.Errorf("probe: %s --version: %w", path, err)
	}
	if out.ExitCode != 0 {
		return "", fmt.Errorf("probe: %s --version exited %d", path, out.ExitCode)
	}
	return ParseVersion(out.Stdout)
}

// helps runs each help invocation. A non-zero exit leaves that output nil:
// the CLI lacks it. A runner error (a timeout, a cancelled ctx) also leaves
// it nil, and is returned so the partial result is not cached.
func helps(ctx context.Context, run adapter.Runner, spec Spec, path string) ([][]byte, error) {
	out := make([][]byte, len(spec.Help))
	var errs []error
	for i, args := range spec.Help {
		o, err := run(ctx, adapter.Command{Path: path, Args: args})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if o.ExitCode != 0 {
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
// is probed in full and recorded. When a help run fails in the runner (a
// timeout), Probe returns the degraded result with that error and records
// nothing.
func (p *Prober) Probe(ctx context.Context, vendor adapter.Vendor, path string) (res adapter.Probe, cached bool, err error) {
	version, err := Version(ctx, p.Run, path)
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
	spec, ok := Specs[vendor]
	if !ok {
		return adapter.Probe{}, false, fmt.Errorf("probe: no spec for vendor %q", vendor)
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
		return res, false, fmt.Errorf("probe: %s help: %w", path, herr)
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
