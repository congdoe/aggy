// Package tooldef is the one format every managed tool is described in:
// the built-ins embedded in aggy and the user's own definitions in
// ~/.aggy/tools/*.toml. The switchboard (aggy tool on|off, launch
// wiring, doctor) works only from these definitions.
//
// A definition is untrusted input, built-ins included: each passes the
// same validator, which bounds sizes and counts, refuses control and
// format characters, takes commands only as argv (never a shell string),
// takes a program only as a bare PATH name or an absolute path, and
// refuses base-URL, proxy, and secret environment names. Definitions are
// never read from a repo.
package tooldef

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
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

	Install Install `toml:"install"`
	MCP     *MCP    `toml:"mcp"`
	Skill   *Skill  `toml:"skill"`
	Hooks   []Hook  `toml:"hook"`
	Init    []Init  `toml:"init"`
	Cost    Cost    `toml:"cost"`

	// Lifecycle names the Go hooks a built-in has: worktree-copy, gc,
	// refresh. A user tool has none.
	Lifecycle []string `toml:"lifecycle"`
	// Unverified lists the top-level keys whose vendor facts are not yet
	// recorded in docs/verification.md.
	Unverified []string `toml:"unverified"`
}

// Install says how to find, install, and prepare the tool. Install and
// Prebuild run only on an explicit aggy tool install, after asking.
type Install struct {
	// Check confirms presence and version. It is required unless the
	// tool is only a skill.
	Check    []string `toml:"check"`
	Install  []string `toml:"install"`
	Prebuild []string `toml:"prebuild"`
}

// MCP is a stdio MCP server: Argv is the program and its arguments, and
// Env names the variables passed through from the user's environment.
// A definition never holds an environment value.
type MCP struct {
	Vendors []adapter.Vendor `toml:"vendors"`
	Argv    []string         `toml:"argv"`
	Env     []string         `toml:"env"`
}

// Skill is a skill folder (holding SKILL.md) linked into each vendor's
// skill folder. Path is absolute or starts with "~/" (the user's home).
type Skill struct {
	Vendors []adapter.Vendor `toml:"vendors"`
	Path    string           `toml:"path"`
}

// Hook is a vendor hook: the event it fires on and the command it runs.
type Hook struct {
	Vendors []adapter.Vendor `toml:"vendors"`
	Event   string           `toml:"event"`
	Argv    []string         `toml:"argv"`
}

// Init is a vendor-specific setup step, such as a tool's own init
// command for one CLI.
type Init struct {
	Vendors []adapter.Vendor `toml:"vendors"`
	Argv    []string         `toml:"argv"`
}

// Cost says how the tool's token cost per vendor is estimated.
type Cost struct {
	// Method is "mcp-schema" (the measured MCP tool schemas), "skill-md"
	// (the size of SKILL.md), "static" (Tokens), or "none".
	Method string `toml:"method"`
	Tokens int    `toml:"tokens"`
}

// Parts is what a tool wires into one vendor.
type Parts struct {
	MCP   *MCP
	Skill *Skill
	Hooks []Hook
	Init  []Init
}

// Empty reports whether the tool has nothing for that vendor.
func (p Parts) Empty() bool {
	return p.MCP == nil && p.Skill == nil && len(p.Hooks) == 0 && len(p.Init) == 0
}

// PartsFor returns the parts of d that support vendor v.
func (d Def) PartsFor(v adapter.Vendor) Parts {
	var p Parts
	if d.MCP != nil && slices.Contains(d.MCP.Vendors, v) {
		p.MCP = d.MCP
	}
	if d.Skill != nil && slices.Contains(d.Skill.Vendors, v) {
		p.Skill = d.Skill
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
	maxHooks       = 8
	maxInit        = 8
	maxPath        = 1024
	maxTokens      = 1_000_000
	maxListed      = 16
)

var (
	nameRE    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	versionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	envRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	eventRE   = regexp.MustCompile(`^[A-Z][A-Za-z]{1,39}$`)
)

// Fixed vocabularies.
var (
	scopes     = []string{"global", "repo"}
	conditions = []string{"has-ui"}
	costs      = []string{"mcp-schema", "skill-md", "static", "none"}
	lifecycles = []string{"worktree-copy", "gc", "refresh"}
	topKeys    = []string{"version", "homepage", "install", "mcp", "skill", "hook", "init", "cost"}
	// shells would turn an argv back into a shell string.
	shells = []string{"sh", "bash", "zsh", "dash", "ksh", "mksh", "ash", "fish", "csh", "tcsh", "busybox", "env"}
)

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
	if und := md.Undecoded(); len(und) > 0 {
		return Def{}, bad(und[0].String(), "unknown key")
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

func isCtl(r rune) bool { return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) }

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
	if !builtIn && d.Default {
		return bad("default", "a user tool is always off by default")
	}
	if !slices.Contains(scopes, d.Scope) {
		return bad("scope", "want %q", scopes)
	}
	if d.When != "" && (d.Scope != "repo" || !slices.Contains(conditions, d.When)) {
		return bad("when", "only with scope = \"repo\", one of %q", conditions)
	}
	if d.MCP == nil && d.Skill == nil && len(d.Hooks) == 0 && len(d.Init) == 0 {
		return bad("mcp", "a tool needs at least one part: mcp, skill, hook, or init")
	}
	if d.MCP != nil {
		if err := vendors("mcp.vendors", d.MCP.Vendors); err != nil {
			return err
		}
		if err := argv("mcp.argv", d.MCP.Argv, true); err != nil {
			return err
		}
		if len(d.MCP.Env) > maxEnv {
			return bad("mcp.env", "at most %d names", maxEnv)
		}
		for i, n := range d.MCP.Env {
			key := fmt.Sprintf("mcp.env[%d]", i)
			if !envRE.MatchString(n) {
				return bad(key, "want a variable name")
			}
			if why := forbiddenEnv(n); why != "" {
				return bad(key, "%s may not be passed: %s", n, why)
			}
			if slices.Contains(d.MCP.Env[:i], n) {
				return bad(key, "%s listed twice", n)
			}
		}
	}
	if d.Skill != nil {
		if err := vendors("skill.vendors", d.Skill.Vendors); err != nil {
			return err
		}
		if err := skillPath(d.Skill.Path); err != nil {
			return err
		}
	}
	if len(d.Hooks) > maxHooks {
		return bad("hook", "at most %d hooks", maxHooks)
	}
	for i, h := range d.Hooks {
		key := fmt.Sprintf("hook[%d]", i)
		if err := vendors(key+".vendors", h.Vendors); err != nil {
			return err
		}
		if !eventRE.MatchString(h.Event) {
			return bad(key+".event", "want a hook event name, such as PostToolUse")
		}
		if err := argv(key+".argv", h.Argv, true); err != nil {
			return err
		}
	}
	if len(d.Init) > maxInit {
		return bad("init", "at most %d steps", maxInit)
	}
	for i, s := range d.Init {
		key := fmt.Sprintf("init[%d]", i)
		if err := vendors(key+".vendors", s.Vendors); err != nil {
			return err
		}
		if err := argv(key+".argv", s.Argv, true); err != nil {
			return err
		}
	}
	skillOnly := d.MCP == nil && len(d.Hooks) == 0 && len(d.Init) == 0
	if err := argv("install.check", d.Install.Check, !skillOnly); err != nil {
		return err
	}
	if err := argv("install.install", d.Install.Install, false); err != nil {
		return err
	}
	if err := argv("install.prebuild", d.Install.Prebuild, false); err != nil {
		return err
	}
	if err := d.Cost.validate(d); err != nil {
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

func (c Cost) validate(d Def) error {
	bad := func(f string, a ...any) error { return &Error{Key: "cost", Msg: fmt.Sprintf(f, a...)} }
	switch {
	case !slices.Contains(costs, c.Method):
		return bad("method: want one of %q", costs)
	case c.Tokens < 0 || c.Tokens > maxTokens:
		return bad("tokens: want 0 to %d", maxTokens)
	case (c.Method == "static") != (c.Tokens > 0):
		return bad("tokens: set with method = \"static\" only, and then above zero")
	case c.Method == "mcp-schema" && d.MCP == nil:
		return bad("method mcp-schema needs an mcp part")
	case c.Method == "skill-md" && d.Skill == nil:
		return bad("method skill-md needs a skill part")
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

// argv checks a command: a program and its arguments, run directly. The
// program is a bare name looked up on PATH or a clean absolute path, and
// never a shell or env, which would make the arguments a shell string.
func argv(key string, a []string, required bool) error {
	bad := func(f string, x ...any) error { return &Error{Key: key, Msg: fmt.Sprintf(f, x...)} }
	if len(a) == 0 {
		if required {
			return bad("want a program and its arguments")
		}
		return nil
	}
	if len(a) > maxArgs {
		return bad("at most %d arguments", maxArgs)
	}
	for _, s := range a {
		if len(s) > maxArg {
			return bad("an argument is over %d bytes", maxArg)
		}
		if strings.ContainsFunc(s, isCtl) {
			return bad("an argument holds a control or format character")
		}
	}
	p := a[0]
	switch {
	case p == "" || strings.ContainsFunc(p, unicode.IsSpace):
		return bad("the program %q must be one word: a command is argv, never a shell string", p)
	case strings.HasPrefix(p, "/"):
		if path.Clean(p) != p {
			return bad("the program %q must be a clean absolute path", p)
		}
	case strings.Contains(p, "/") || p == "." || p == "..":
		return bad("the program %q must be a bare name found on PATH or an absolute path", p)
	}
	if slices.Contains(shells, path.Base(p)) {
		return bad("the program %q is a shell or env, which would run a shell string", p)
	}
	return nil
}

// skillPath accepts a clean absolute path or one under "~/".
func skillPath(p string) error {
	bad := func(m string) error { return &Error{Key: "skill.path", Msg: m} }
	if err := text("skill.path", p, maxPath, true); err != nil {
		return err
	}
	rest, home := strings.CutPrefix(p, "~/")
	switch {
	case !home && !strings.HasPrefix(p, "/"):
		return bad("want an absolute path or one starting with ~/")
	case home && (rest == "" || path.Clean(rest) != rest || rest == ".." || strings.HasPrefix(rest, "../") || strings.HasPrefix(rest, "/")):
		return bad("want a clean path under ~/")
	case !home && path.Clean(p) != p:
		return bad("want a clean absolute path")
	}
	return nil
}

// forbiddenEnv says why a variable may not be passed to a tool, or "".
// aggy never sets or forwards a base URL, a proxy, a secret, or a vendor
// CLI's own routing settings.
func forbiddenEnv(n string) string {
	u := strings.ToUpper(n)
	switch {
	case strings.HasSuffix(u, "BASE_URL"):
		return "a base-URL variable can send traffic to another endpoint"
	case strings.Contains(u, "PROXY"):
		return "aggy sets up no proxy"
	case strings.Contains(u, "API_KEY"), strings.Contains(u, "APIKEY"), strings.HasSuffix(u, "_KEY"),
		strings.Contains(u, "TOKEN"), strings.Contains(u, "SECRET"), strings.Contains(u, "PASSWORD"),
		strings.Contains(u, "CREDENTIAL"), strings.Contains(u, "AUTH"):
		return "it names a key or secret"
	}
	for _, p := range []string{"ANTHROPIC_", "OPENAI_", "XAI_", "GROK_", "CODEX_", "CLAUDE_", "AGGY_"} {
		if strings.HasPrefix(u, p) {
			return "it is a vendor CLI's or aggy's own setting"
		}
	}
	return ""
}
