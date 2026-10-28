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
	// entry of a "Commands:" block. With Kind SessionArg it is unused.
	// Several features may share a Name: the capability is present when
	// any of them is found.
	Flag string
	Kind MatchKind
}

// MatchKind says how a Feature is found in help output.
type MatchKind int

// Match kinds.
const (
	Option     MatchKind = iota // Flag appears as an option
	Subcommand                  // Flag is listed in a Commands: block
	// SessionArg: the help names a session-id argument, such as
	// "[SESSION_ID]" or "<session-id>". A help that merely exits 0 proves
	// nothing: clap may have taken the subcommand name as a prompt.
	SessionArg
)

// Spec says how to probe one vendor's CLI.
type Spec struct {
	// Programs are the names the CLI gives itself in --version output,
	// such as "codex-cli" or "Claude Code". A version line keyed on one of
	// them wins over any other version-shaped line (see ParseVersion).
	Programs []string
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
	// CapIgnoreUserConfig is codex --ignore-user-config, which keeps a
	// research child from loading the user's MCP servers. Not recorded in
	// docs/adapters.md: present only when help lists it.
	CapIgnoreUserConfig = "ignore-user-config"
)

// Specs holds the probe spec of each vendor, from the flags recorded in
// docs/adapters.md.
var Specs = map[adapter.Vendor]Spec{
	adapter.Claude: {
		Programs: []string{"Claude Code", "claude"},
		Help:     [][]string{{"--help"}},
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
		Programs: []string{"grok"},
		Help:     [][]string{{"--help"}},
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
		Programs: []string{"codex-cli", "codex"},
		Help:     [][]string{{"--help"}, {"exec", "--help"}, {"exec", "resume", "--help"}},
		Features: []Feature{
			{CapHeadless, 0, "exec", Subcommand},
			// exec resume is present when exec lists it as a command, or
			// when its own help names a session-id argument.
			{CapResume, 1, "resume", Subcommand},
			{CapResume, 2, "", SessionArg},
			{CapNoAltScreen, 0, "--no-alt-screen", Option},
			{CapConfigOverride, 0, "-c", Option},
			{CapJSONOutput, 1, "--json", Option},
			{CapSandbox, 1, "--sandbox", Option},
			{CapDisable, 1, "--disable", Option},
			{CapImageInput, 1, "-i", Option},
			{CapIgnoreUserConfig, 0, "--ignore-user-config", Option},
			{CapIgnoreUserConfig, 1, "--ignore-user-config", Option},
		},
	},
}

// ErrNoVersion means the CLI printed no version.
var ErrNoVersion = errors.New("probe: no version in --version output")

// ErrAmbiguousVersion means --version printed several different versions
// and none is keyed on the CLI's program name, so aggy will not guess.
var ErrAmbiguousVersion = errors.New("probe: several versions in --version output")

// semver is x.y.z with an optional v prefix and an optional pre-release or
// build suffix; its group is the version without the v.
const semver = `v?(\d+\.\d+(?:\.\d+)?(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)`

// note is an optional parenthesized remark after the version, such as
// "(Claude Code)" or "(fake)".
const note = `(?:\s+\([^)]*\))?`

// anyLine matches a whole --version line: an optional program name of one
// or more words, the version, and an optional note. The exact --version
// formats are not recorded in docs/adapters.md, so this is the fallback
// when no line names the program.
var anyLine = regexp.MustCompile(`^(?:[A-Za-z][\w.-]*(?:\s+[A-Za-z][\w.-]*)*\s+)?` + semver + note + `$`)

// programLine matches a whole line that names one of programs: "<name>
// [v]x.y.z [(note)]" or "x.y.z (<name>)", case-insensitively. A name of
// several words matches with any run of spaces between them.
func programLine(programs []string) *regexp.Regexp {
	if len(programs) == 0 {
		return nil
	}
	var alts []string
	for _, p := range programs {
		words := strings.Fields(p)
		for i, w := range words {
			words[i] = regexp.QuoteMeta(w)
		}
		alts = append(alts, strings.Join(words, `\s+`))
	}
	names := `(?:` + strings.Join(alts, "|") + `)`
	return regexp.MustCompile(`(?i)^(?:` + names + `\s+` + semver + note + `|` + semver + `\s+\(` + names + `\))$`)
}

// versions returns the distinct versions on whole lines of out that re
// matches, in order of appearance.
func versions(out []byte, re *regexp.Regexp) []string {
	var vs []string
	for _, line := range strings.Split(string(out), "\n") {
		m := re.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		v := ""
		for _, g := range m[1:] {
			if g != "" {
				v = g
				break
			}
		}
		if !slices.Contains(vs, v) {
			vs = append(vs, v)
		}
	}
	return vs
}

// ParseVersion returns the CLI's version from its --version output. Only
// whole lines count, so a version mentioned in a notice ("update 3.0.0
// available") is not taken.
//
// A line keyed on one of programs (the Spec's program names) wins: "codex-
// cli 0.164.0" over "node v20.11.1". When no line names the program, a
// single version-shaped line is taken. Several different candidates of the
// same rank are ErrAmbiguousVersion, never a guess. stdout is read first,
// then stderr.
func ParseVersion(stdout, stderr []byte, programs []string) (string, error) {
	if re := programLine(programs); re != nil {
		vs := append(versions(stdout, re), versions(stderr, re)...)
		vs = slices.Compact(vs)
		switch {
		case len(vs) == 1:
			return vs[0], nil
		case len(vs) > 1:
			return "", fmt.Errorf("%w: %s", ErrAmbiguousVersion, strings.Join(vs, ", "))
		}
	}
	for _, out := range [][]byte{stdout, stderr} {
		vs := versions(out, anyLine)
		switch {
		case len(vs) == 1:
			return vs[0], nil
		case len(vs) > 1:
			return "", fmt.Errorf("%w: %s", ErrAmbiguousVersion, strings.Join(vs, ", "))
		}
	}
	return "", ErrNoVersion
}

// commandsBlock returns the subcommand names listed under a "Commands:"
// heading. The first entry's indentation is the command column: a line
// indented exactly that far starts an entry, a line indented further is a
// wrapped description and is skipped, and a blank line, a heading, or a
// line indented less ends the block.
func commandsBlock(help string) []string {
	var names []string
	in := false
	col := -1
	for _, line := range strings.Split(help, "\n") {
		trimmed := strings.TrimSpace(line)
		indent := indentWidth(line)
		switch {
		case strings.EqualFold(strings.TrimSuffix(trimmed, ":"), "commands") && strings.HasSuffix(trimmed, ":"):
			in, col = true, -1
		case !in:
		case trimmed == "" || indent == 0:
			in = false
		case col < 0 || indent == col:
			col = indent
			names = append(names, strings.Fields(trimmed)[0])
		case indent > col:
			// A wrapped description line.
		default:
			in = false
		}
	}
	return names
}

// indentWidth is the width of line's leading blanks, a tab counting to the
// next multiple of 8.
func indentWidth(line string) int {
	w := 0
	for _, r := range line {
		switch r {
		case ' ':
			w++
		case '\t':
			w += 8 - w%8
		default:
			return w
		}
	}
	return w
}

// sessionArg matches a help text naming a session-id argument.
var sessionArg = regexp.MustCompile(`[<\[](?i:session[_-]?id)[>\]]|\bSESSION_ID\b`)

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
		case SessionArg:
			found = sessionArg.MatchString(help)
		}
		if found && !slices.Contains(caps, f.Name) {
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
	version, err := Version(ctx, run, path, spec.Programs)
	if err != nil && !unreadable(err) {
		return p, err
	}
	// An unreadable version is not a failed probe: Version stays empty
	// and adapter.VersionWarning reports it.
	p.Version = version
	hs, _ := helps(ctx, run, spec, path)
	p.Capabilities = Capabilities(spec, hs)
	return p, nil
}

// Version runs path --version and parses it, keyed on the program names
// (see ParseVersion).
func Version(ctx context.Context, run adapter.Runner, path string, programs []string) (string, error) {
	out, err := run(ctx, adapter.Command{Path: path, Args: []string{"--version"}})
	if err != nil {
		return "", fmt.Errorf("probe: %s --version: %w", path, err)
	}
	if out.ExitCode != 0 {
		return "", fmt.Errorf("probe: %s --version exited %d", path, out.ExitCode)
	}
	return ParseVersion(out.Stdout, out.Stderr, programs)
}

// unreadable reports whether err means --version ran but printed no
// version aggy can read.
func unreadable(err error) bool {
	return errors.Is(err, ErrNoVersion) || errors.Is(err, ErrAmbiguousVersion)
}

// ErrIncomplete matches a probe in which a help invocation failed. Its
// result has the capabilities the other invocations showed, and is not
// cached, so the next probe runs every invocation again.
var ErrIncomplete = errors.New("probe: incomplete")

// usageError matches a CLI's refusal of an argument list: clap's
// "unrecognized subcommand" and "unexpected argument", and the "unknown
// option" or "unknown command" of other parsers. It means the invocation
// does not exist in this version, a stable answer.
var usageError = regexp.MustCompile(`(?i)\b(?:unrecognized subcommand|unexpected argument|unknown (?:option|command|subcommand)|invalid subcommand)\b`)

// helps runs each help invocation. A non-zero exit whose output is a
// usage error (see usageError) leaves that output nil with no error: the
// invocation does not exist, so its capabilities are absent, and that
// result may be cached. Any other non-zero exit or a runner error (a
// timeout, a cancelled ctx) also leaves the output nil but is reported in
// the error, which matches ErrIncomplete, so the partial result is not
// cached.
func helps(ctx context.Context, run adapter.Runner, spec Spec, path string) ([][]byte, error) {
	out := make([][]byte, len(spec.Help))
	var errs []error
	for i, args := range spec.Help {
		o, err := run(ctx, adapter.Command{Path: path, Args: args})
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: %s %s: %w", ErrIncomplete, path, strings.Join(args, " "), err))
			continue
		}
		if o.ExitCode != 0 && (usageError.Match(o.Stderr) || usageError.Match(o.Stdout)) {
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
	version, err := Version(ctx, p.Run, path, spec.Programs)
	if err != nil && !unreadable(err) {
		return adapter.Probe{Vendor: vendor, Path: path}, false, err
	}
	if version == "" {
		// No version to key a cache entry on: probe in full, record
		// nothing, and let adapter.VersionWarning report it.
		res, err := Detect(ctx, p.Run, vendor, path)
		res.ProbedAt = time.Now().UTC().Truncate(time.Millisecond)
		if p.Now != nil {
			res.ProbedAt = p.Now().UTC().Truncate(time.Millisecond)
		}
		return res, false, err
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
