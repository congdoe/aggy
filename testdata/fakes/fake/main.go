// Command fake is fake-claude, fake-codex, fake-grok, and fake-hook: a
// stand-in for the vendor CLIs that follows a scenario file, so tests never
// run a real vendor CLI or need a login. The vendor is the binary's name
// without the "fake-" prefix, or $AGGY_FAKE_VENDOR. See ../README.md for
// the scenario format.
//
// It honors only the flags recorded in docs/adapters.md, and fails like a
// real CLI on any other, so an adapter that invents a flag fails its test.
// It writes only under $HOME (or the vendor's relocated home) and refuses
// to run when $HOME is the real user's home.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Scenario drives one run. Every field is optional.
type Scenario struct {
	Version       string            `json:"version"`
	VersionOutput string            `json:"version_output"`
	Help          *string           `json:"help"`
	ExtraFlags    []string          `json:"extra_flags"`
	ExtraSwitches []string          `json:"extra_switches"`
	SessionID     string            `json:"session_id"`
	Stdout        string            `json:"stdout"`
	Stderr        string            `json:"stderr"`
	JSON          map[string]any    `json:"json"`
	Events        []json.RawMessage `json:"events"`
	Error         string            `json:"error"`
	ErrorText     *string           `json:"error_text"`
	ErrorEvent    json.RawMessage   `json:"error_event"`
	ExitCode      int               `json:"exit_code"`
	HookCommand   []string          `json:"hook_command"`
	HookPayloadIn string            `json:"hook_payload_in"`
	Hooks         []Hook            `json:"hooks"`
	Images        int               `json:"images"`
	SessionFiles  []SessionFile     `json:"session_files"`
	SleepMS       int               `json:"sleep_ms"`
	// ExtraValues adds accepted values to a flag with a recorded value
	// set, e.g. {"--sandbox": ["workspace-write"]}.
	ExtraValues map[string][]string `json:"extra_values"`
	// Strict turns on rules docs/adapters.md does not record, by name:
	// "resume-with-session-id" refuses --resume with --session-id unless
	// --fork-session is given.
	Strict []string `json:"strict"`
	// InteractiveWithoutTTY lets an interactive run start with a stdin
	// that is not a terminal. The real CLIs need a terminal.
	InteractiveWithoutTTY bool `json:"interactive_without_tty"`
	// Replies answer an exact argument list that docs/adapters.md does
	// not record, such as a sign-in status command a test supplies,
	// before any flag is parsed.
	Replies []Reply `json:"replies"`
	// RawEcho makes an interactive run behave like a full-screen CLI: it
	// puts its terminal in raw mode, prints stdout, echoes every byte it
	// reads, reports each resize as "[winch <rows>x<cols>]", stops itself
	// on Ctrl+Z (0x1a) and prints "[continued]" when resumed, and exits on
	// Ctrl+D (0x04).
	RawEcho bool `json:"raw_echo"`
}

// Reply is the fixed answer to one exact argument list.
type Reply struct {
	Args     []string `json:"args"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr"`
	ExitCode int      `json:"exit_code"`
}

// reply returns the scenario's reply to args, if any.
func (f *fake) reply(args []string) (Reply, bool) {
	for _, r := range f.sc.Replies {
		if len(r.Args) == len(args) {
			same := true
			for i := range args {
				if args[i] != r.Args[i] {
					same = false
					break
				}
			}
			if same {
				return r, true
			}
		}
	}
	return Reply{}, false
}

// recordedValues are the flag values docs/adapters.md records. Others are
// refused unless the scenario adds them through extra_values.
var recordedValues = map[string][]string{
	"--output-format":   {"json"},
	"--permission-mode": {"plan"},
	"--sandbox":         {"read-only"},
}

// codexFeatures are the codex exec features the fake knows: the ones the
// borrow lockdown disables, and others live checks accepted (codex-cli
// 0.164.0, 2026-10-28). Like the real CLI, an unknown --disable feature
// is ignored, unless --strict-config makes it an error.
var codexFeatures = []string{"shell_tool", "unified_exec", "view_image", "apps", "plugins", "goals",
	"image_generation", "sleep_tool", "code_mode", "standalone_web_search", "multi_agent"}

// grokTools are the tool names grok 1.0.70 knows (live-verified
// 2026-10-28). A --tools list naming any other makes the real CLI warn
// "name no tool" on stderr and ignore the whole allowlist; the fake
// prints the same warning.
var grokTools = []string{
	"read_file", "grep", "list_dir", "write", "search_replace", "run_terminal_cmd", "web_fetch",
	"Agent", "search_tool", "use_tool", "image_gen", "image_edit", "image_to_video",
	"reference_to_video", "send_feedback", "monitor", "workflow", "todo_write",
	"ask_user_question", "enter_plan_mode", "exit_plan_mode",
	"web_search", "open_page", "open_page_with_find",
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validate checks flag values and the rules docs/adapters.md records, and
// the opt-in strict rules.
func (f *fake) validate(headless bool) error {
	for flag, allowed := range recordedValues {
		allowed = append(append([]string{}, allowed...), f.sc.ExtraValues[flag]...)
		for _, v := range f.flags[flag] {
			if !contains(allowed, v) {
				return fmt.Errorf("invalid value %q for %s", v, flag)
			}
		}
	}
	if f.has("--strict-config") {
		for _, v := range f.flags["--disable"] {
			if !contains(codexFeatures, v) {
				// codex's own message (config/src/loader/mod.rs at
				// 6aaac69), printed bare.
				return verbatimError(fmt.Sprintf("unknown configuration field `features.%s` in -c/--config override", v))
			}
		}
	}
	// Recorded: --session-id takes a UUID (Claude and Grok).
	for _, v := range f.flags["--session-id"] {
		if !uuidRE.MatchString(v) {
			return fmt.Errorf("--session-id must be a UUID, got %q", v)
		}
	}
	if contains(f.sc.Strict, "resume-with-session-id") && f.has("--resume") && f.has("--session-id") && !f.has("--fork-session") {
		return errors.New("--session-id with --resume requires --fork-session")
	}
	// A headless run needs a prompt: an argument or stdin. A prompt
	// argument of "-" means "read it from stdin", so stdin must then hold
	// one.
	if headless {
		args := f.positional
		if f.sub == "exec resume" && len(args) > 0 {
			args = args[1:] // the session id
		}
		fromStdin := len(args) == 0 || (len(args) == 1 && args[0] == "-")
		if fromStdin && strings.TrimSpace(f.stdin) == "" {
			return errors.New("no prompt given (pass it as an argument or on stdin)")
		}
	}
	if f.sub == "exec resume" && len(f.positional) == 0 {
		return errors.New("exec resume needs a session id")
	}
	if !headless && !f.terminal && !f.sc.InteractiveWithoutTTY {
		return errors.New("an interactive session needs a terminal")
	}
	return nil
}

// Hook is one hook call: the event name and its recorded payload.
type Hook struct {
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
}

// SessionFile is a file the run writes, relative to the vendor's home.
type SessionFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Record is one line appended to $AGGY_FAKE_RECORD per run.
type Record struct {
	Vendor        string              `json:"vendor"`
	Mode          string              `json:"mode"`
	Args          []string            `json:"args"`
	Cwd           string              `json:"cwd"`
	Env           map[string]string   `json:"env"`
	EnvKeys       []string            `json:"env_keys"`
	StdinTerminal bool                `json:"stdin_terminal"`
	Stdin         string              `json:"stdin"`
	SessionID     string              `json:"session_id,omitempty"`
	Flags         map[string][]string `json:"flags,omitempty"`
	Positional    []string            `json:"positional,omitempty"`
	PID           int                 `json:"pid"`
}

// Versions the vendor facts were recorded against (docs/adapters.md).
var defaultVersion = map[string]string{"claude": "2.1.311", "codex": "0.164.0", "grok": "1.0.70"}

// flagSpec lists the flags a vendor command accepts: value flags take an
// argument, switches do not.
type flagSpec struct {
	values   []string
	switches []string
	// optional take a value only when the next argument does not start
	// with "-": Claude's --resume [value] opens a picker without one, and
	// never takes a following flag as its value.
	optional []string
	// multi take one or more values, each split on commas, up to the next argument that starts
	// with "-" (a lone "-" is a value), as clap's num_args = 1.. does.
	multi []string
}

// Flags recorded in docs/adapters.md and docs/design.md, per vendor and
// codex subcommand.
var specs = map[string]flagSpec{
	// --strict-mcp-config and --mcp-config: the borrow lockdown,
	// live-verified on claude 2.1.312 (2026-10-28). --tools narrows the
	// built-ins (tool names unverified).
	"claude": {
		// --append-system-prompt-file: the shared memory, live-verified on
		// 2.1.312 although the real --help omits it (T6619).
		values:   []string{"--output-format", "--permission-mode", "--session-id", "--mcp-config", "--tools", "--append-system-prompt-file", "--settings"},
		switches: []string{"-p", "--fork-session", "--strict-mcp-config"},
		optional: []string{"--resume"},
	},
	"grok": {
		values: []string{"--output-format", "--permission-mode", "--session-id", "--resume",
			"--tools", "--disallowed-tools", "--max-turns", "--rules", "--deny"},
		// --continue: proposal §3. --no-subagents: the borrow lockdown,
		// live-verified on grok 1.0.70 (2026-10-28).
		switches: []string{"-p", "--fork-session", "--no-alt-screen", "--continue", "--no-subagents"},
	},
	"codex": {
		values:   []string{"-c"},
		switches: []string{"--no-alt-screen"},
	},
	// codex exec flags go before the resume subcommand; -c is global.
	// --ignore-user-config and --skip-git-repo-check: the borrow
	// lockdown, live-verified on codex-cli 0.164.0 (2026-10-28).
	"codex exec": {
		values:   []string{"-c", "--disable", "--sandbox"},
		switches: []string{"--json", "--ignore-user-config", "--skip-git-repo-check", "--strict-config"},
		multi:    []string{"-i"},
	},
	"codex exec resume": {
		values:   []string{"-c"},
		switches: []string{"--strict-config"},
	},
	"codex resume": {
		values:   []string{"-c"},
		switches: []string{"--no-alt-screen"},
	},
}

// Limit text per vendor, from docs/adapters.md and docs/design.md. Claude's
// is a recorded fragment; Grok's is not recorded, so it is the class name.
var limitText = map[string]string{
	"claude": "You've hit your limit · resets in 3h",
	"codex":  "You've hit your usage limit",
	"grok":   "rate_limit",
}

func main() {
	os.Exit(run())
}

// verbatimError is a validation error the fake prints exactly as the
// real CLI does, with no "fake: <vendor>:" prefix.
type verbatimError string

func (e verbatimError) Error() string { return string(e) }

func fail(code int, format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "fake: "+format+"\n", args...)
	return code
}

func run() int {
	vendor := os.Getenv("AGGY_FAKE_VENDOR")
	if vendor == "" {
		vendor = strings.TrimPrefix(filepath.Base(os.Args[0]), "fake-")
	}
	if err := checkHome(vendor); err != nil {
		return fail(97, "%v", err)
	}
	if vendor == "hook" {
		return runHook()
	}
	if _, ok := specs[vendor]; !ok {
		return fail(97, "unknown vendor %q", vendor)
	}
	sc, err := loadScenario()
	if err != nil {
		return fail(97, "scenario: %v", err)
	}
	f := &fake{vendor: vendor, sc: sc}
	return f.main(os.Args[1:])
}

// checkHome refuses to run against the real home directory, so a fake can
// never write into the real ~/.claude, ~/.codex, or ~/.grok.
//
// HOME must not be the real home, and no vendor home the fake could write
// (CLAUDE_CONFIG_DIR, CODEX_HOME, GROK_HOME, or HOME/.<vendor>) may be
// relative or inside the real home. Paths are compared twice: by name,
// after resolving symlinks and folding case on darwin, and by file
// identity, walking up from each candidate and comparing every existing
// ancestor with os.SameFile against the real home and the real vendor
// homes. Identity catches the spellings a name check misses, such as the
// /System/Volumes/Data firmlink on macOS or a case-folded "ſ" for "s".
func checkHome(string) error {
	h := os.Getenv("HOME")
	if h == "" || !filepath.IsAbs(h) {
		return errors.New("HOME must be an absolute temp directory")
	}
	for _, env := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		if v := os.Getenv(env); v != "" && !filepath.IsAbs(v) {
			return fmt.Errorf("%s must be absolute, got %q", env, v)
		}
	}
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return errors.New("cannot find the real home directory to stay out of it")
	}
	realHome := canon(u.HomeDir)
	if canon(h) == realHome {
		return errors.New("refusing to run with HOME set to the real home directory")
	}
	for _, v := range []string{"claude", "codex", "grok"} {
		vh := canon(vendorHome(v))
		if vh == realHome || strings.HasPrefix(vh, realHome+"/") {
			return fmt.Errorf("refusing to run with the %s home %s inside the real home directory", v, vendorHome(v))
		}
	}
	var real []os.FileInfo
	for _, p := range []string{u.HomeDir, filepath.Join(u.HomeDir, ".claude"), filepath.Join(u.HomeDir, ".codex"), filepath.Join(u.HomeDir, ".grok")} {
		if fi, err := os.Stat(p); err == nil {
			real = append(real, fi)
		}
	}
	candidates := []string{h}
	for _, v := range []string{"claude", "codex", "grok"} {
		candidates = append(candidates, vendorHome(v))
	}
	for _, c := range candidates {
		if within(c, real) {
			return fmt.Errorf("refusing to run: %s is the real home directory or inside it", c)
		}
	}
	return nil
}

// within reports whether p or any of its existing ancestors is the same
// file as one of real.
func within(p string, real []os.FileInfo) bool {
	for p = filepath.Clean(p); ; p = filepath.Dir(p) {
		if fi, err := os.Stat(p); err == nil {
			for _, r := range real {
				if os.SameFile(fi, r) {
					return true
				}
			}
		}
		if filepath.Dir(p) == p {
			return false
		}
	}
}

// canon resolves symlinks in the longest existing prefix of p, and folds
// case on darwin.
func canon(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = filepath.Join(r, rest)
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			p = filepath.Join(p, rest)
			break
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
	if runtime.GOOS == "darwin" {
		p = strings.ToLower(p)
	}
	return p
}

func loadScenario() (*Scenario, error) {
	sc := &Scenario{}
	p := os.Getenv("AGGY_FAKE_SCENARIO")
	if p == "" {
		return sc, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(sc); err != nil {
		return nil, err
	}
	return sc, nil
}

// vendorHome is the vendor's config directory: the relocation variable when
// set, else ~/.<vendor>.
func vendorHome(vendor string) string {
	env := map[string]string{"claude": "CLAUDE_CONFIG_DIR", "codex": "CODEX_HOME", "grok": "GROK_HOME"}[vendor]
	if v := os.Getenv(env); v != "" {
		return filepath.Clean(v)
	}
	return filepath.Join(os.Getenv("HOME"), "."+vendor)
}

type fake struct {
	vendor     string
	sc         *Scenario
	flags      map[string][]string
	positional []string
	sub        string // codex subcommand: "", "exec", "resume", "exec resume"
	id         string
	stdin      string
	terminal   bool
}

func (f *fake) has(flag string) bool { _, ok := f.flags[flag]; return ok }

func (f *fake) value(flag string) string {
	if v := f.flags[flag]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

// parse reads args against the spec for the vendor and subcommand. A flag
// it does not know is an error, as in the real CLIs.
func (f *fake) parse(args []string) error {
	f.flags = map[string][]string{}
	key := f.vendor
	if f.vendor == "codex" && len(args) > 0 && (args[0] == "exec" || args[0] == "resume") {
		f.sub = args[0]
		args = args[1:]
		key = "codex " + f.sub
	}
	var spec flagSpec
	var values, switches []string
	use := func(k string) {
		spec = specs[k]
		values = append(append([]string{}, spec.values...), f.sc.ExtraFlags...)
		switches = append(append([]string{"--version", "--help", "-h"}, spec.switches...), f.sc.ExtraSwitches...)
	}
	use(key)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			f.positional = append(f.positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			// The first positional of codex exec may name its resume
			// subcommand, which then takes only its own flags.
			if f.sub == "exec" && len(f.positional) == 0 && a == "resume" {
				f.sub = "exec resume"
				use("codex exec resume")
				continue
			}
			f.positional = append(f.positional, a)
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		switch {
		case contains(switches, name) && !hasVal:
			f.flags[name] = append(f.flags[name], "")
		case contains(spec.optional, name):
			if !hasVal && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				val = args[i]
			}
			f.flags[name] = append(f.flags[name], val)
		case contains(spec.multi, name):
			// Values split on commas too, as codex's value_delimiter = ','.
			if hasVal {
				f.flags[name] = append(f.flags[name], strings.Split(val, ",")...)
				continue
			}
			n := 0
			for i+1 < len(args) && args[i+1] != "--" && (args[i+1] == "-" || !strings.HasPrefix(args[i+1], "-")) {
				i++
				n++
				f.flags[name] = append(f.flags[name], strings.Split(args[i], ",")...)
			}
			if n == 0 {
				return fmt.Errorf("option %s needs a value", name)
			}
		case contains(values, name):
			if !hasVal {
				if i+1 >= len(args) {
					return fmt.Errorf("option %s needs a value", name)
				}
				i++
				val = args[i]
			}
			f.flags[name] = append(f.flags[name], val)
		default:
			return fmt.Errorf("unknown option '%s'", a)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (f *fake) main(args []string) int {
	if r, ok := f.reply(args); ok {
		f.record("reply", args)
		fmt.Print(f.expand(r.Stdout))
		fmt.Fprint(os.Stderr, f.expand(r.Stderr))
		return r.ExitCode
	}
	if err := f.parse(args); err != nil {
		f.record("error", args)
		return fail(2, "%s: %v", f.vendor, err)
	}
	if f.has("--version") {
		f.record("version", args)
		fmt.Print(f.versionText())
		return 0
	}
	if f.has("--help") || f.has("-h") {
		f.record("help", args)
		fmt.Print(f.helpText())
		return 0
	}
	headless := (f.vendor != "codex" && f.has("-p")) || f.sub == "exec" || f.sub == "exec resume"
	f.terminal = isTerminal(os.Stdin)
	if headless && !f.terminal {
		// codex exec waits on a stdin that is not a terminal; the others
		// read the prompt from it. Either way, read to EOF.
		b, _ := io.ReadAll(os.Stdin)
		f.stdin = string(b)
	}
	if err := f.validate(headless); err != nil {
		f.record("error", args)
		var v verbatimError
		if errors.As(err, &v) {
			fmt.Fprintln(os.Stderr, string(v))
			return 2
		}
		return fail(2, "%s: %v", f.vendor, err)
	}
	f.id = f.sessionID()
	if f.vendor == "grok" {
		for _, list := range f.flags["--tools"] {
			for _, name := range strings.Split(list, ",") {
				if !contains(grokTools, strings.TrimSpace(name)) {
					fmt.Fprintf(os.Stderr, "warning: --tools: name no tool %q; ignoring the allowlist\n", name)
				}
			}
		}
	}
	mode := "interactive"
	if headless {
		mode = "headless"
	}
	f.record(mode, args)
	if f.sc.SleepMS > 0 {
		time.Sleep(time.Duration(f.sc.SleepMS) * time.Millisecond)
	}
	if err := f.writeFiles(); err != nil {
		return fail(3, "%v", err)
	}
	if headless {
		f.headlessOutput()
	} else if !f.sc.RawEcho {
		fmt.Print(f.expand(f.sc.Stdout))
	}
	if f.sc.Stderr != "" {
		fmt.Fprint(os.Stderr, f.expand(f.sc.Stderr))
	}
	if err := f.runHooks(); err != nil {
		return fail(3, "hook: %v", err)
	}
	if f.sc.Error != "" {
		text := limitText[f.vendor]
		if f.sc.ErrorText != nil {
			text = *f.sc.ErrorText
		}
		if text != "" {
			fmt.Fprintln(os.Stderr, text)
		}
	}
	switch {
	case !headless && f.sc.RawEcho:
		if err := rawEcho(f.expand(f.sc.Stdout)); err != nil {
			return fail(3, "raw echo: %v", err)
		}
	case !headless:
		waitForExit()
	}
	if f.sc.ExitCode == 0 && f.sc.Error != "" {
		return 1
	}
	return f.sc.ExitCode
}

func (f *fake) versionText() string {
	if f.sc.VersionOutput != "" {
		return f.expand(f.sc.VersionOutput)
	}
	v := f.sc.Version
	if v == "" {
		v = defaultVersion[f.vendor]
	}
	// The real output formats are not recorded; this one is a placeholder
	// that holds the version number.
	return fmt.Sprintf("%s %s (fake)\n", f.vendor, v)
}

// helpText lists the flags the fake honors, unless the scenario gives the
// text. The real help formats are not recorded.
func (f *fake) helpText() string {
	if f.sc.Help != nil {
		return *f.sc.Help
	}
	key := f.vendor
	if f.sub != "" {
		key = "codex " + f.sub
	}
	spec := specs[key]
	var b strings.Builder
	if f.sub == "exec resume" {
		b.WriteString("Usage: codex exec resume [options] [SESSION_ID] [PROMPT]\n\nOptions:\n")
	} else {
		fmt.Fprintf(&b, "Usage: %s [options]\n\nOptions:\n", key)
	}
	for _, v := range spec.optional {
		fmt.Fprintf(&b, "  %s [value]\n", v)
	}
	for _, v := range spec.multi {
		fmt.Fprintf(&b, "  %s <value>...\n", v)
	}
	for _, v := range spec.values {
		fmt.Fprintf(&b, "  %s <value>\n", v)
	}
	for _, s := range spec.switches {
		fmt.Fprintf(&b, "  %s\n", s)
	}
	switch {
	case f.vendor == "codex" && f.sub == "":
		b.WriteString("\nCommands:\n  exec\n  resume\n")
	case f.sub == "exec":
		b.WriteString("\nCommands:\n  resume\n")
	}
	return b.String()
}

// sessionID follows each CLI's recorded rules: Claude and Grok take
// --session-id, resume keeps the id unless --fork-session, and Codex never
// takes an id in advance.
func (f *fake) sessionID() string {
	fresh := f.sc.SessionID
	if fresh == "" {
		fresh = newUUID()
	}
	switch f.vendor {
	case "codex":
		if (f.sub == "exec resume" || f.sub == "resume") && len(f.positional) > 0 {
			return f.positional[0]
		}
		return fresh
	default:
		if r := f.value("--resume"); r != "" && !f.has("--fork-session") {
			return r
		}
		if s := f.value("--session-id"); s != "" {
			return s
		}
		return fresh
	}
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// grokSessionDir is ~/.grok/sessions/<url-encoded cwd>/<sessionId>. The
// exact encoding is not recorded; this uses url.QueryEscape, which encodes
// "/" as %2F.
func (f *fake) grokSessionDir() string {
	cwd, _ := os.Getwd()
	return filepath.Join(vendorHome("grok"), "sessions", url.QueryEscape(cwd), f.id)
}

// expand fills {session_id}, {cwd}, {home}, {vendor_home}, and, for Grok,
// {session_dir}.
func (f *fake) expand(s string) string {
	cwd, _ := os.Getwd()
	pairs := []string{
		"{session_id}", f.id,
		"{cwd}", cwd,
		"{home}", os.Getenv("HOME"),
		"{vendor_home}", vendorHome(f.vendor),
	}
	if f.vendor == "grok" {
		pairs = append(pairs, "{session_dir}", f.grokSessionDir())
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

func (f *fake) expandJSON(raw json.RawMessage) string {
	return f.expand(string(raw))
}

func (f *fake) headlessOutput() {
	jsonOut := f.value("--output-format") == "json" || f.has("--json")
	if !jsonOut {
		fmt.Print(f.expand(f.sc.Stdout))
		return
	}
	switch f.vendor {
	case "codex":
		// Recorded: the first event is thread.started with thread_id.
		// Later events are not recorded, so they come from the scenario.
		line, _ := json.Marshal(map[string]string{"type": "thread.started", "thread_id": f.id})
		fmt.Println(string(line))
		for _, e := range f.sc.Events {
			fmt.Println(compact(f.expandJSON(e)))
		}
		if len(f.sc.ErrorEvent) > 0 {
			fmt.Println(compact(f.expandJSON(f.sc.ErrorEvent)))
		}
	default:
		obj := map[string]any{}
		if f.sc.JSON != nil {
			b, _ := json.Marshal(f.sc.JSON)
			_ = json.Unmarshal([]byte(f.expand(string(b))), &obj)
		}
		if f.vendor == "grok" {
			// Recorded: Grok's JSON includes sessionId.
			obj["sessionId"] = f.id
		}
		b, _ := json.Marshal(obj)
		fmt.Println(string(b))
	}
}

func compact(s string) string {
	var buf bytes.Buffer
	if json.Compact(&buf, []byte(s)) != nil {
		return s
	}
	return buf.String()
}

// Minimal valid image bytes.
var (
	png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")
	jpg = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00\xff\xd9")
)

// writeFiles writes images where each CLI writes them, and the scenario's
// session files under the vendor home.
func (f *fake) writeFiles() error {
	if f.sc.Images > 0 {
		var dir, ext string
		var data []byte
		switch f.vendor {
		case "codex":
			dir, ext, data = filepath.Join(vendorHome("codex"), "generated_images", f.id), ".png", png
		case "grok":
			dir, ext, data = filepath.Join(f.grokSessionDir(), "images"), ".jpg", jpg
		default:
			return fmt.Errorf("%s image paths are not recorded", f.vendor)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		for i := 1; i <= f.sc.Images; i++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d%s", i, ext)), data, 0o600); err != nil {
				return err
			}
		}
	}
	base := vendorHome(f.vendor)
	for _, sf := range f.sc.SessionFiles {
		rel := filepath.Clean(f.expand(sf.Path))
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("session file %q must stay inside the vendor home", sf.Path)
		}
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(f.expand(sf.Content)), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// runHooks calls the hook command once per hook, with the event name as
// the last argument and the payload on stdin (or as an argument when
// hook_payload_in is "arg"). A limit error with no hooks listed raises
// StopFailure with error rate_limit on Claude and Grok; Codex has no
// failure hook.
func (f *fake) runHooks() error {
	hooks := f.sc.Hooks
	if len(hooks) == 0 && f.sc.Error == "rate_limit" && f.vendor != "codex" {
		hooks = []Hook{{Event: "StopFailure", Payload: json.RawMessage(`{"error":"rate_limit"}`)}}
	}
	if len(hooks) == 0 {
		return nil
	}
	if len(f.sc.HookCommand) == 0 {
		return errors.New("hooks listed but no hook_command")
	}
	for _, h := range hooks {
		payload := f.expandJSON(h.Payload)
		args := append(append([]string{}, f.sc.HookCommand[1:]...), h.Event)
		if f.sc.HookPayloadIn == "arg" {
			args = append(args, payload)
		}
		cmd := exec.Command(f.sc.HookCommand[0], args...)
		if f.sc.HookPayloadIn != "arg" {
			cmd.Stdin = strings.NewReader(payload)
		}
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", h.Event, err)
		}
	}
	return nil
}

// rawEcho is the RawEcho session: raw mode, banner, byte echo, resize
// reports, Ctrl+Z suspend, and Ctrl+D exit.
func rawEcho(banner string) error {
	st, err := makeRaw(0)
	if err != nil {
		return err
	}
	defer func() { _ = restoreTerm(0, st) }()
	var mu sync.Mutex
	write := func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = os.Stdout.Write(b)
	}
	winch := make(chan os.Signal, 4)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			h, w := termSize(0)
			write([]byte(fmt.Sprintf("[winch %dx%d]", h, w)))
		}
	}()
	write([]byte(banner))
	buf := make([]byte, 4096)
	for {
		n, rerr := os.Stdin.Read(buf)
		var out []byte
		for _, c := range buf[:n] {
			switch c {
			case 0x04:
				write(out)
				return nil
			case 0x1a:
				write(out)
				out = nil
				_ = restoreTerm(0, st)
				// kill can return before the stop takes hold on Linux.
				cont := make(chan os.Signal, 1)
				signal.Notify(cont, syscall.SIGCONT)
				_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
				<-cont
				signal.Stop(cont)
				if _, err := makeRaw(0); err != nil {
					return err
				}
				write([]byte("[continued]"))
			default:
				out = append(out, c)
			}
		}
		write(out)
		if rerr != nil {
			return nil
		}
	}
}

// waitForExit reads lines until "/exit" or EOF, like a session waiting at
// its prompt.
func waitForExit() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "/exit" {
			return
		}
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0 && fi.Mode()&os.ModeDevice != 0 && !isDevNull(fi)
}

// isDevNull reports whether fi is /dev/null, which is a character device
// but not a terminal.
func isDevNull(fi os.FileInfo) bool {
	null, err := os.Stat(os.DevNull)
	return err == nil && os.SameFile(fi, null)
}

// record appends this invocation to $AGGY_FAKE_RECORD. Values are kept
// only for AGGY_* variables; every other variable is recorded by name.
func (f *fake) record(mode string, args []string) {
	p := os.Getenv("AGGY_FAKE_RECORD")
	if p == "" {
		return
	}
	cwd, _ := os.Getwd()
	r := Record{
		Vendor: f.vendor, Mode: mode, Args: args, Cwd: cwd, Env: map[string]string{},
		StdinTerminal: f.terminal, Stdin: f.stdin, SessionID: f.id,
		Flags: f.flags, Positional: f.positional, PID: os.Getpid(),
	}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		r.EnvKeys = append(r.EnvKeys, k)
		if strings.HasPrefix(k, "AGGY_") {
			r.Env[k] = v
		}
	}
	sort.Strings(r.EnvKeys)
	appendJSON(p, r)
}

func appendJSON(path string, v any) {
	b, _ := json.Marshal(v)
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake: record: %v\n", err)
		return
	}
	defer fh.Close()
	_, _ = fh.Write(append(b, '\n'))
}

// runHook is fake-hook: it records its arguments and stdin.
func runHook() int {
	b, _ := io.ReadAll(os.Stdin)
	if p := os.Getenv("AGGY_FAKE_RECORD"); p != "" {
		appendJSON(p, map[string]any{"vendor": "hook", "args": os.Args[1:], "stdin": string(b)})
	}
	return 0
}
