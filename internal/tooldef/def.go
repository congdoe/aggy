// Package tooldef is the one format every managed tool is described in:
// the built-ins embedded in aggy and the user's own definitions in
// ~/.aggy/tools/*.toml. The switchboard (aggy tool on|off, launch
// wiring, doctor) works only from these definitions.
//
// A definition is untrusted input, built-ins included: each passes the
// same validator, which bounds sizes and counts, refuses control and
// format characters, takes commands only as argv (never a shell string),
// takes a program only as a bare PATH name or an absolute path, and
// refuses base-URL, proxy, and secret environment names. The one route
// to a vendor is a [proxy] part (T6780): a loopback address and, per
// vendor, the one base-URL variable ProxyVars allows. A program is
// still arbitrary code: the preview on add and the explicit turn-on are
// the real gate, and the program checks are defence in depth.
// Definitions are never read from a repo.
package tooldef

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/congdoe/aggy/internal/adapter"
)

// Def is one tool definition, as written in its TOML file.
type Def struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
	Homepage    string `toml:"homepage"`
	// Version is the pinned version aggy installs and checks for.
	Version string `toml:"version"`
	// Default is whether the tool starts on. User tools are always off.
	Default bool `toml:"default"`
	// Scope is "global" or "repo"; a repo-scoped tool may carry a
	// condition in When, such as "has-ui".
	Scope string `toml:"scope"`
	When  string `toml:"when"`
	// Network says the tool's servers reach the network (M78-2). An edit
	// borrow's child never gets one unless the user also sets
	// routing.edit_network_tools: a server runs outside the CLI's own
	// sandbox, so a steered child could read a key file and send it out.
	// The flag is self-declared, so a user definition without the key
	// is taken to reach the network (R80-2); a built-in's is as written.
	Network bool `toml:"network"`

	Install Install `toml:"install"`
	MCP     []MCP   `toml:"mcp"`
	// Proxy is a local proxy the vendor CLI's requests go through
	// (T6780). It is off until the person turns the tool on for a vendor
	// at a terminal and types the confirmation; see Proxy.
	Proxy  *Proxy  `toml:"proxy"`
	Skills []Skill `toml:"skill"`
	Hooks  []Hook  `toml:"hook"`
	Init   []Init  `toml:"init"`
	Cost   Cost    `toml:"cost"`

	// Lifecycle names the Go hooks a built-in has: worktree-copy, gc,
	// refresh. A user tool has none.
	Lifecycle []string `toml:"lifecycle"`
	// Unverified lists the top-level keys whose vendor facts are not yet
	// recorded in docs/verification.md.
	Unverified []string `toml:"unverified"`
	// TelemetryEnv is the tool's own telemetry opt-out, as NAME=VALUE,
	// which aggy sets with DO_NOT_TRACK=1 in every process of the tool's
	// it starts unless the user config allows telemetry (T6822).
	TelemetryEnv []string `toml:"telemetry_env"`
}

// Install says how to find, install, and prepare the tool. None of it
// runs while the tool is off: Check runs on an explicit aggy tool
// install or check, or once the tool is on; Install runs only on an
// explicit aggy tool install, after asking. aggy never installs a tool
// on its own.
type Install struct {
	// Check confirms presence and version. It is required unless the
	// tool is only a skill.
	Check []string `toml:"check"`
	// CheckPattern finds the version in Check's output: a regular
	// expression with exactly one capture group.
	CheckPattern string   `toml:"check_pattern"`
	Install      []string `toml:"install"`
	// Prebuild builds a per-repo index (T6711): a built-in's only, with
	// scope = "repo" and {repo} in its arguments. It runs once per repo,
	// on the aggy tool on --repo that finds no index there.
	Prebuild []string `toml:"prebuild"`
	// Hint tells the person how to install the pinned version by hand.
	// It is text aggy shows (doctor, aggy tool on, the skill-missing
	// refusal) and never runs.
	Hint string `toml:"hint"`
	// Rebuild builds the index again from scratch, where a prebuild left
	// one unfinished (an interrupted build): the same rules as Prebuild,
	// which it needs.
	Rebuild []string `toml:"rebuild"`
}

// MCP is a stdio MCP server for the vendors listed: Argv is the program
// and its arguments, and Env names the variables passed through from the
// user's environment. A definition never holds an environment value.
type MCP struct {
	Vendors []adapter.Vendor `toml:"vendors"`
	Argv    []string         `toml:"argv"`
	Env     []string         `toml:"env"`
}

// Proxy is a local proxy aggy starts for an on-screen session of each
// vendor it is on for, health-checks before the CLI starts, and stops
// when the CLI exits (T6780). Every request of that CLI, its login token
// included, then passes through the proxy's process, so it is never on
// by default, never for a borrow child, and only for a vendor the
// person named and confirmed at a terminal.
type Proxy struct {
	// Vendors are the vendors the proxy supports; Env names, per
	// vendor, the variable the CLI is pointed at it with, which must be
	// that vendor's entry in ProxyVars.
	Vendors []adapter.Vendor          `toml:"vendors"`
	Env     map[adapter.Vendor]string `toml:"env"`
	// Argv starts the proxy in the foreground. It never holds {repo}.
	Argv []string `toml:"argv"`
	// Listen is where it listens: 127.0.0.1:<port> or [::1]:<port>.
	Listen string `toml:"listen"`
	// Health is the HTTP path that answers 2xx once it is ready.
	Health string `toml:"health"`
}

// ProxyVars is the allowlist of the variables a proxy may set, one per
// vendor: the CLI's own base-URL variable. Claude: ANTHROPIC_BASE_URL
// (code.claude.com env-vars docs; docs/adapters.md, "Proxy and API-key
// routes"). Grok: GROK_MODELS_BASE_URL (docs/verification.md: custom
// inference base URL). Codex has none: it does not read
// OPENAI_BASE_URL, only openai_base_url in its config
// (docs/verification.md), which aggy never writes.
var ProxyVars = map[adapter.Vendor]string{
	adapter.Claude: "ANTHROPIC_BASE_URL",
	adapter.Grok:   "GROK_MODELS_BASE_URL",
}

// ProxyDigest is the sha256, in hex, of what the person confirms when
// turning p on for vendor v: the vendor, the argv, the listen address,
// the health path, and the variable. A confirmation holds only while it
// matches, so a built-in whose proxy changes with a release, or an
// edited user file, asks again.
func ProxyDigest(p Proxy, v adapter.Vendor) string {
	h := sha256.New()
	for _, s := range append([]string{string(v), p.Listen, p.Health, p.Env[v], strconv.Itoa(len(p.Argv))}, p.Argv...) {
		fmt.Fprintf(h, "%d:%s\x00", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Addr is the proxy's listen address, which the validator has checked.
func (p Proxy) Addr() netip.AddrPort { a, _ := ParseListen(p.Listen); return a }

// ParseListen parses a proxy's listen address: 127.0.0.1 or ::1 only,
// with a port from 1024 to 65535.
func ParseListen(s string) (netip.AddrPort, error) {
	a, err := netip.ParseAddrPort(s)
	switch {
	case err != nil:
		return netip.AddrPort{}, errors.New("want 127.0.0.1:<port> or [::1]:<port>")
	case a.Addr() != netip.MustParseAddr("127.0.0.1") && a.Addr() != netip.IPv6Loopback():
		return netip.AddrPort{}, errors.New("a proxy listens on loopback only: 127.0.0.1 or ::1")
	case a.Port() < 1024:
		return netip.AddrPort{}, errors.New("want a port from 1024 to 65535")
	}
	return a, nil
}

// Skill is a skill folder (holding SKILL.md) linked into each listed
// vendor's skill folder. Path starts with "~/" (the user's home).
type Skill struct {
	Vendors []adapter.Vendor `toml:"vendors"`
	Path    string           `toml:"path"`
}

// Hook is a vendor hook: the event it fires on, an optional tool-name
// matcher and timeout, and the command it runs. Every CLI runs a hook
// command through a shell, so its words are limited to characters no
// shell treats specially. Decides must be set, on a built-in only, for
// an event whose hook can allow or deny an action.
type Hook struct {
	Vendors []adapter.Vendor `toml:"vendors"`
	Event   string           `toml:"event"`
	Matcher string           `toml:"matcher"`
	// Timeout is in seconds; zero means the CLI's default.
	Timeout int  `toml:"timeout"`
	Decides bool `toml:"decides"`
	// Approves says the hook is known to answer "allow", approving an
	// action the person would have been asked about. Such a hook is
	// never wired (T6708).
	Approves bool `toml:"approves"`
	// Rewrites says the hook may answer updatedInput, changing the tool
	// call (H78-1); aggy hook-tool drops an updatedInput from any other
	// hook. RewritePrefix is what a rewritten shell command starts with,
	// such as "rtk ": the launch then adds the safety policy's Bash
	// rules again with it in front, in case the CLI matches its rules
	// against the rewritten command. Built-ins only, with decides.
	Rewrites      bool     `toml:"rewrites"`
	RewritePrefix string   `toml:"rewrite_prefix"`
	Argv          []string `toml:"argv"`
}

// Init is a vendor-specific setup step of a built-in. It declares what
// it writes and the command that undoes it.
type Init struct {
	Vendors []adapter.Vendor `toml:"vendors"`
	Argv    []string         `toml:"argv"`
	Undo    []string         `toml:"undo"`
	// Writes names the files or config keys the step changes.
	Writes []string `toml:"writes"`
}

// Cost says how the tool's token cost per vendor is estimated.
type Cost struct {
	// Method is "mcp-schema" (the measured MCP tool schemas), "skill-md"
	// (the size of SKILL.md), "static" (Tokens, or Vendor per vendor),
	// or "none".
	Method string         `toml:"method"`
	Tokens int            `toml:"tokens"`
	Vendor map[string]int `toml:"vendor"`
}

// Parts is what a tool wires into one vendor.
type Parts struct {
	MCP   *MCP
	Skill *Skill
	Hooks []Hook
	Init  []Init
	// Proxy is set when the proxy supports the vendor. The switchboard
	// keeps it only where the person confirmed it (switchboard.State).
	Proxy *Proxy
}

// Empty reports whether the tool has nothing for that vendor.
func (p Parts) Empty() bool {
	return p.MCP == nil && p.Skill == nil && len(p.Hooks) == 0 && len(p.Init) == 0 && p.Proxy == nil
}

// SkillOnly reports whether d is only skills: no MCP server, hook, init
// step, or proxy.
func (d Def) SkillOnly() bool {
	return len(d.MCP) == 0 && len(d.Hooks) == 0 && len(d.Init) == 0 && d.Proxy == nil
}

// PartsFor returns the parts of d that support vendor v. A vendor is in
// at most one [[mcp]] and one [[skill]] entry.
func (d Def) PartsFor(v adapter.Vendor) Parts {
	var p Parts
	for i := range d.MCP {
		if slices.Contains(d.MCP[i].Vendors, v) {
			p.MCP = &d.MCP[i]
		}
	}
	for i := range d.Skills {
		if slices.Contains(d.Skills[i].Vendors, v) {
			p.Skill = &d.Skills[i]
		}
	}
	for _, h := range d.Hooks {
		if slices.Contains(h.Vendors, v) {
			p.Hooks = append(p.Hooks, h)
		}
	}
	for _, i := range d.Init {
		if slices.Contains(i.Vendors, v) {
			p.Init = append(p.Init, i)
		}
	}
	if d.Proxy != nil && slices.Contains(d.Proxy.Vendors, v) {
		p.Proxy = d.Proxy
	}
	return p
}

// Error is a refused definition: the file and the key at fault. Err is
// ErrUntrusted for a file refused for where it is or who can write it.
type Error struct {
	File, Key, Msg string
	Err            error
}

func (e *Error) Error() string {
	msg := e.Msg
	if e.Err != nil {
		msg = e.Err.Error() + ": " + msg
	}
	if e.Key == "" {
		return e.File + ": " + msg
	}
	return e.File + ": " + e.Key + ": " + msg
}

// Unwrap returns Err.
func (e *Error) Unwrap() error { return e.Err }

// ErrUntrusted matches, through errors.Is, a user definition refused for
// where it is or who can write it, rather than for what it says.
var ErrUntrusted = errors.New("untrusted tool definition")

// Limits on a definition. Definitions are untrusted input.
const (
	MaxSize        = 64 << 10
	maxDescription = 200
	maxHomepage    = 512
	maxArgs        = 64
	maxArg         = 4096
	maxEnv         = 16
	maxParts       = 4
	maxHooks       = 8
	maxInit        = 8
	maxPath        = 1024
	maxTokens      = 1_000_000
	maxListed      = 16
	maxPattern     = 256
	maxHint        = 400
	maxMatcher     = 128
	maxTimeout     = 600
	maxHealth      = 128
)

var (
	nameRE        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	versionRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	userVersionRE = regexp.MustCompile(`^[0-9][A-Za-z0-9._+-]{0,63}$`)
	matcherRE     = regexp.MustCompile(`^[A-Za-z0-9_|.*]+$`)
	keyRE         = regexp.MustCompile(`^[a-z_]+$`)
	prefixRE      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31} $`)
	healthRE      = regexp.MustCompile(`^/[A-Za-z0-9/._-]*$`)
)

// Fixed vocabularies.
var (
	scopes     = []string{"global", "repo"}
	conditions = []string{"has-ui"}
	costs      = []string{"mcp-schema", "skill-md", "static", "none"}
	lifecycles = []string{"worktree-copy", "gc", "refresh"}
	topKeys    = []string{"version", "homepage", "install", "mcp", "skill", "hook", "init", "cost", "network", "proxy"}
)

// hookEvents are the hook events each CLI has. Claude: code.claude.com
// docs hooks.md; Codex 0.164.0: codex-rs/config/src/hook_config.rs#L39
// (docs/verification.md, "Codex hooks"); Grok (grok-build 2bdd1d6a):
// 10-hooks.md for SessionStart, Stop, StopFailure, StopCancelled, and
// Notification, and a live check for PreToolUse (verification.md);
// Grok's PostToolUse is from the aggy proposal and unverified.
var hookEvents = map[adapter.Vendor][]string{
	adapter.Claude: {"SessionStart", "SessionEnd", "UserPromptSubmit", "PreToolUse", "PostToolUse",
		"PermissionRequest", "Notification", "Stop", "StopFailure", "SubagentStop", "PreCompact"},
	adapter.Codex: {"PreModelRequest", "PostModelRequest", "PreToolUse", "PermissionRequest", "PostToolUse",
		"PreCompact", "PostCompact", "SessionStart", "SessionEnd", "UserPromptSubmit", "SubagentStart",
		"SubagentStop", "Stop", "Interrupt"},
	adapter.Grok: {"SessionStart", "Stop", "StopFailure", "StopCancelled", "Notification", "PreToolUse", "PostToolUse"},
}

// decidingEvents are events whose hook can allow or deny an action.
// Codex's PreModelRequest and PostModelRequest are in hook_config.rs,
// but what their output can do (block or change a model request) is
// unverified, so they are treated as deciding: no user tool hooks them.
var decidingEvents = []string{"PreToolUse", "PermissionRequest", "PreModelRequest", "PostModelRequest"}

// ValidName reports whether name is a well-formed tool name.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Parse decodes and validates the definition in data. file names it in
// errors. builtIn allows what only aggy's own definitions may have.
func Parse(file string, data []byte, builtIn bool) (Def, error) {
	bad := func(key, f string, a ...any) error { return &Error{File: file, Key: key, Msg: fmt.Sprintf(f, a...)} }
	if len(data) > MaxSize {
		return Def{}, bad("", "over %d bytes", MaxSize)
	}
	if !utf8.Valid(data) {
		return Def{}, bad("", "not UTF-8")
	}
	// Comments included: a bidi or zero-width character can make the
	// file read differently than it parses.
	for i, line := range bytes.Split(data, []byte("\n")) {
		for _, r := range string(bytes.TrimSuffix(line, []byte("\r"))) {
			if r != '\t' && isCtl(r) {
				return Def{}, bad("", "line %d holds a control or format character", i+1)
			}
		}
	}
	var d Def
	md, err := toml.Decode(string(data), &d)
	if err != nil {
		return Def{}, bad("", "%v", err)
	}
	// The decoder matches keys with Unicode case folding, so [MCP], ARGV,
	// or vendorſ (U+017F) would merge with or override the lower-case
	// key: every key is plain a-z and _.
	for _, k := range md.Keys() {
		for _, part := range k {
			if !keyRE.MatchString(part) {
				return Def{}, bad(k.String(), "keys are lower case: a-z and _ only")
			}
		}
	}
	if und := md.Undecoded(); len(und) > 0 {
		return Def{}, bad(und[0].String(), "unknown key")
	}
	if !builtIn && !md.IsDefined("network") {
		d.Network = true
	}
	if err := d.validate(builtIn); err != nil {
		var e *Error
		if errors.As(err, &e) {
			e.File = file
		}
		return Def{}, err
	}
	return d, nil
}

// isCtl reports a control, format, line-separator, or
// paragraph-separator character.
func isCtl(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
}

// validate checks every rule; the error names the first bad key.
func (d Def) validate(builtIn bool) error {
	bad := func(key, f string, a ...any) error { return &Error{Key: key, Msg: fmt.Sprintf(f, a...)} }
	if !nameRE.MatchString(d.Name) {
		return bad("name", "want 1 to 32 of a-z, 0-9, and -, starting with a letter")
	}
	if err := text("description", d.Description, maxDescription, true); err != nil {
		return err
	}
	if d.Homepage != "" {
		if err := text("homepage", d.Homepage, maxHomepage, true); err != nil {
			return err
		}
		u, err := url.Parse(d.Homepage)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return bad("homepage", "want an https URL with no user info")
		}
	}
	if !versionRE.MatchString(d.Version) {
		return bad("version", "want a pinned version: 1 to 64 of A-Z, a-z, 0-9, and . _ + -")
	}
	if !builtIn && !userVersionRE.MatchString(d.Version) {
		return bad("version", "want a pinned version starting with a digit, not a tag such as latest")
	}
	if !builtIn && d.Default {
		return bad("default", "a user tool is always off by default")
	}
	if !slices.Contains(scopes, d.Scope) {
		return bad("scope", "want %q", scopes)
	}
	if d.When != "" && (d.Scope != "repo" || !slices.Contains(conditions, d.When)) {
		return bad("when", "only with scope = \"repo\", one of %q", conditions)
	}
	if len(d.MCP) == 0 && len(d.Skills) == 0 && len(d.Hooks) == 0 && len(d.Init) == 0 && d.Proxy == nil {
		return bad("mcp", "a tool needs at least one part: mcp, skill, hook, init, or proxy")
	}
	if d.SkillOnly() && d.Scope == "repo" {
		// L78-8: a skill is linked for every repo, never for one.
		return bad("scope", "a tool that is only a skill is global: its skill is linked for every repo")
	}
	if err := d.validateMCP(builtIn); err != nil {
		return err
	}
	if err := d.validateSkills(builtIn); err != nil {
		return err
	}
	if err := d.validateHooks(builtIn); err != nil {
		return err
	}
	if err := d.validateInit(builtIn); err != nil {
		return err
	}
	if err := d.validateProxy(); err != nil {
		return err
	}
	if err := d.validateInstall(builtIn); err != nil {
		return err
	}
	if err := d.Cost.validate(d); err != nil {
		return err
	}
	if err := telemetryEnv(d.TelemetryEnv); err != nil {
		return err
	}
	if len(d.Lifecycle) > 0 && !builtIn {
		return bad("lifecycle", "only aggy's built-in tools have lifecycle hooks")
	}
	if err := list("lifecycle", d.Lifecycle, lifecycles); err != nil {
		return err
	}
	return list("unverified", d.Unverified, topKeys)
}

func (d Def) validateMCP(builtIn bool) error {
	if len(d.MCP) > maxParts {
		return &Error{Key: "mcp", Msg: fmt.Sprintf("at most %d entries", maxParts)}
	}
	var seen []adapter.Vendor
	for i, m := range d.MCP {
		key := fmt.Sprintf("mcp[%d]", i)
		if err := partVendors(key+".vendors", m.Vendors, &seen); err != nil {
			return err
		}
		if err := argv(key+".argv", m.Argv, true); err != nil {
			return err
		}
		if len(m.Env) > maxEnv {
			return &Error{Key: key + ".env", Msg: fmt.Sprintf("at most %d names", maxEnv)}
		}
		for j, n := range m.Env {
			ek := fmt.Sprintf("%s.env[%d]", key, j)
			if why := envRefused(d.Name, n, builtIn); why != "" {
				return &Error{Key: ek, Msg: why}
			}
			if slices.Contains(m.Env[:j], n) {
				return &Error{Key: ek, Msg: n + " listed twice"}
			}
		}
	}
	return nil
}

// validateProxy checks [proxy], for built-ins and user tools alike: a
// loopback listen address, a health path, an argv without {repo}, and
// per vendor exactly the variable ProxyVars allows.
func (d Def) validateProxy() error {
	p := d.Proxy
	if p == nil {
		return nil
	}
	bad := func(key, f string, a ...any) error { return &Error{Key: "proxy." + key, Msg: fmt.Sprintf(f, a...)} }
	if err := vendors("proxy.vendors", p.Vendors); err != nil {
		return err
	}
	for _, v := range p.Vendors {
		want, ok := ProxyVars[v]
		if !ok {
			return bad("vendors", "%s has no base-URL variable aggy may set (only %s)", v, proxyVarList())
		}
		if got := p.Env[v]; got != want {
			return bad("env."+string(v), "want %q, the one variable a proxy may set for %s", want, v)
		}
	}
	for v := range p.Env {
		if !slices.Contains(p.Vendors, v) {
			return bad("env", "names a vendor not in proxy.vendors")
		}
	}
	if err := argv("proxy.argv", p.Argv, true); err != nil {
		return err
	}
	if slices.ContainsFunc(p.Argv, func(a string) bool { return strings.Contains(a, Placeholder) }) {
		return bad("argv", "a proxy runs for the session, never with %s", Placeholder)
	}
	if _, err := ParseListen(p.Listen); err != nil {
		return bad("listen", "%v", err)
	}
	if len(p.Health) > maxHealth || !healthRE.MatchString(p.Health) || strings.Contains(p.Health, "..") {
		return bad("health", "want an HTTP path of at most %d of A-Z, a-z, 0-9, and / . _ -, starting with /", maxHealth)
	}
	return nil
}

func proxyVarList() string {
	var out []string
	for _, v := range adapter.Vendors() {
		if n, ok := ProxyVars[v]; ok {
			out = append(out, string(v)+": "+n)
		}
	}
	return strings.Join(out, ", ")
}

func (d Def) validateSkills(builtIn bool) error {
	if len(d.Skills) > maxParts {
		return &Error{Key: "skill", Msg: fmt.Sprintf("at most %d entries", maxParts)}
	}
	var seen []adapter.Vendor
	for i, s := range d.Skills {
		key := fmt.Sprintf("skill[%d]", i)
		if err := partVendors(key+".vendors", s.Vendors, &seen); err != nil {
			return err
		}
		if err := skillPath(key+".path", d.Name, s.Path, builtIn); err != nil {
			return err
		}
	}
	return nil
}

func (d Def) validateHooks(builtIn bool) error {
	if len(d.Hooks) > maxHooks {
		return &Error{Key: "hook", Msg: fmt.Sprintf("at most %d hooks", maxHooks)}
	}
	for i, h := range d.Hooks {
		key := fmt.Sprintf("hook[%d]", i)
		bad := func(sub, f string, a ...any) error {
			return &Error{Key: key + sub, Msg: fmt.Sprintf(f, a...)}
		}
		if err := vendors(key+".vendors", h.Vendors); err != nil {
			return err
		}
		for _, v := range h.Vendors {
			if !slices.Contains(hookEvents[v], h.Event) {
				return bad(".event", "%s has no hook event %q; want one of %q", v, h.Event, hookEvents[v])
			}
		}
		deciding := slices.Contains(decidingEvents, h.Event)
		switch {
		case deciding && !builtIn:
			return bad(".event", "a user tool may not hook %s, whose hook can allow or deny an action", h.Event)
		case deciding && !h.Decides:
			return bad(".decides", "%s can allow or deny an action: set decides = true to say this hook does", h.Event)
		case !deciding && h.Decides:
			return bad(".decides", "only for %q", decidingEvents)
		case h.Approves && !h.Decides:
			return bad(".approves", "only with decides = true")
		case h.Rewrites && (!h.Decides || !builtIn):
			return bad(".rewrites", "only on a built-in, with decides = true")
		case h.Rewrites != (h.RewritePrefix != ""):
			return bad(".rewrite_prefix", "set it exactly when rewrites = true")
		case h.RewritePrefix != "" && !prefixRE.MatchString(h.RewritePrefix):
			return bad(".rewrite_prefix", "want a program name and one space, such as \"rtk \"")
		}
		if h.Matcher != "" {
			if len(h.Matcher) > maxMatcher || !matcherRE.MatchString(h.Matcher) {
				return bad(".matcher", "want a tool-name pattern of at most %d of A-Z, a-z, 0-9, and _ | . *", maxMatcher)
			}
			if _, err := regexp.Compile(h.Matcher); err != nil {
				return bad(".matcher", "not a valid pattern")
			}
		}
		if h.Timeout < 0 || h.Timeout > maxTimeout {
			return bad(".timeout", "want 0 (the CLI's default) to %d seconds", maxTimeout)
		}
		if err := argv(key+".argv", h.Argv, true); err != nil {
			return err
		}
		if err := shellSafe(key+".argv", h.Argv); err != nil {
			return err
		}
	}
	return nil
}

func (d Def) validateInit(builtIn bool) error {
	if len(d.Init) > 0 && !builtIn {
		return &Error{Key: "init", Msg: "only aggy's built-in tools have init steps"}
	}
	if len(d.Init) > maxInit {
		return &Error{Key: "init", Msg: fmt.Sprintf("at most %d steps", maxInit)}
	}
	for i, s := range d.Init {
		key := fmt.Sprintf("init[%d]", i)
		if err := vendors(key+".vendors", s.Vendors); err != nil {
			return err
		}
		if err := argv(key+".argv", s.Argv, true); err != nil {
			return err
		}
		if err := argv(key+".undo", s.Undo, true); err != nil {
			return err
		}
		if len(s.Writes) == 0 || len(s.Writes) > maxListed {
			return &Error{Key: key + ".writes", Msg: fmt.Sprintf("name 1 to %d files or config keys the step changes", maxListed)}
		}
		for j, w := range s.Writes {
			if err := text(fmt.Sprintf("%s.writes[%d]", key, j), w, maxPath, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d Def) validateInstall(builtIn bool) error {
	if err := argv("install.check", d.Install.Check, !d.SkillOnly()); err != nil {
		return err
	}
	if p := d.Install.CheckPattern; p != "" {
		bad := func(m string) error { return &Error{Key: "install.check_pattern", Msg: m} }
		if len(d.Install.Check) == 0 {
			return bad("needs install.check")
		}
		if err := text("install.check_pattern", p, maxPattern, true); err != nil {
			return err
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return bad("not a valid regular expression")
		}
		if re.NumSubexp() != 1 {
			return bad("want exactly one capture group, for the version")
		}
	}
	if err := text("install.hint", d.Install.Hint, maxHint, false); err != nil {
		return err
	}
	if err := argv("install.install", d.Install.Install, false); err != nil {
		return err
	}
	if len(d.Install.Rebuild) > 0 && len(d.Install.Prebuild) == 0 {
		return &Error{Key: "install.rebuild", Msg: "only with install.prebuild"}
	}
	for _, c := range []struct {
		key string
		a   []string
	}{{"install.prebuild", d.Install.Prebuild}, {"install.rebuild", d.Install.Rebuild}} {
		if len(c.a) == 0 {
			continue
		}
		if !builtIn || d.Scope != "repo" || !slices.ContainsFunc(c.a[1:], func(a string) bool { return strings.Contains(a, Placeholder) }) {
			return &Error{Key: c.key, Msg: "only aggy's built-in per-repo tools have a prebuild or rebuild, and it names the repo as " + Placeholder}
		}
		if err := argv(c.key, c.a, true); err != nil {
			return err
		}
	}
	return nil
}

func (c Cost) validate(d Def) error {
	bad := func(f string, a ...any) error { return &Error{Key: "cost", Msg: fmt.Sprintf(f, a...)} }
	static := c.Method == "static"
	switch {
	case !slices.Contains(costs, c.Method):
		return bad("method: want one of %q", costs)
	case c.Tokens < 0 || c.Tokens > maxTokens:
		return bad("tokens: want 0 to %d", maxTokens)
	case !static && (c.Tokens != 0 || c.Vendor != nil):
		return bad("tokens and vendor: only with method = \"static\"")
	case static && c.Tokens == 0 && len(c.Vendor) == 0:
		return bad("method static needs tokens, or tokens per vendor in [cost.vendor]")
	case c.Method == "mcp-schema" && len(d.MCP) == 0:
		return bad("method mcp-schema needs an mcp part")
	case c.Method == "skill-md" && len(d.Skills) == 0:
		return bad("method skill-md needs a skill part")
	}
	for v, n := range c.Vendor {
		if _, err := adapter.ParseVendor(v); err != nil {
			return &Error{Key: "cost.vendor." + v, Msg: err.Error()}
		}
		if n < 1 || n > maxTokens {
			return &Error{Key: "cost.vendor." + v, Msg: fmt.Sprintf("want 1 to %d tokens", maxTokens)}
		}
	}
	return nil
}

// text checks a free-text field: its length, and no control or format
// characters.
func text(key, s string, max int, required bool) error {
	if required && strings.TrimSpace(s) == "" {
		return &Error{Key: key, Msg: "required"}
	}
	if len(s) > max {
		return &Error{Key: key, Msg: fmt.Sprintf("over %d bytes", max)}
	}
	if strings.ContainsFunc(s, isCtl) {
		return &Error{Key: key, Msg: "holds a control or format character"}
	}
	return nil
}

// list checks a list of names from a fixed set, each at most once.
func list(key string, xs, allowed []string) error {
	if len(xs) > maxListed {
		return &Error{Key: key, Msg: fmt.Sprintf("at most %d entries", maxListed)}
	}
	for i, x := range xs {
		if !slices.Contains(allowed, x) {
			return &Error{Key: fmt.Sprintf("%s[%d]", key, i), Msg: fmt.Sprintf("want one of %q", allowed)}
		}
		if slices.Contains(xs[:i], x) {
			return &Error{Key: fmt.Sprintf("%s[%d]", key, i), Msg: x + " listed twice"}
		}
	}
	return nil
}

func vendors(key string, vs []adapter.Vendor) error {
	if len(vs) == 0 {
		return &Error{Key: key, Msg: "name at least one vendor"}
	}
	for i, v := range vs {
		if _, err := adapter.ParseVendor(string(v)); err != nil {
			return &Error{Key: fmt.Sprintf("%s[%d]", key, i), Msg: err.Error()}
		}
		if slices.Contains(vs[:i], v) {
			return &Error{Key: fmt.Sprintf("%s[%d]", key, i), Msg: string(v) + " listed twice"}
		}
	}
	return nil
}

// partVendors is vendors, plus each vendor in at most one entry of a
// part: the MCP server and the skill link are named after the tool.
func partVendors(key string, vs []adapter.Vendor, seen *[]adapter.Vendor) error {
	if err := vendors(key, vs); err != nil {
		return err
	}
	for i, v := range vs {
		if slices.Contains(*seen, v) {
			return &Error{Key: fmt.Sprintf("%s[%d]", key, i), Msg: string(v) + " is in another entry"}
		}
		*seen = append(*seen, v)
	}
	return nil
}
