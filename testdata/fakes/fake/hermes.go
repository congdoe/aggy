package main

// fake-hermes: a stand-in for Hermes Agent 0.23.1 (hermes-agent at
// 3b42157858), built from its source, never run. It models what aggy uses:
//
//   - -p/--profile <name>, taken out of argv before anything else
//     (hermes_cli/_parser.py:13-16, main.py:508-566): the profile's home is
//     <root>/profiles/<name>, which must exist; "default" is the root.
//   - --version, --help, chat --help.
//   - profile create <name> --no-alias --no-skills: makes the profile folder
//     with a config.yaml seeded from the root's model block
//     (hermes_cli/profiles.py:735-748, 1344-1420).
//   - config get <key> --json, config set <key> <value> (depth two).
//   - sessions export - --session-id <id> --format jsonl [--redact]: one JSON
//     session per line (hermes_cli/sessions_cmd.py:114-118, 468-470).
//   - chat --cli: the classic screen. It registers the profile's hooks at
//     start, asking on its terminal for each (event, command) pair not in
//     shell-hooks-allowlist.json (agent/shell_hooks.py:764-790); each line
//     typed is a turn, which makes the scenario's hermes tool calls, each
//     judged by pre_tool_call (shell_hooks.py:560-658) and then, for terminal,
//     approvals.deny (tools/approval_floors.py:23-42), and ends with
//     on_session_end (agent/turn_finalizer.py:882-896). /exit and /quit end
//     it; /new starts a new session.
//   - chat -q <text> -Q: one turn, then "session_id: <id>".
//
// Hooks run as Hermes' daemon runs them: no AGGY_* variable, and the
// install folder (this binary's) as their folder, with the payload's cwd that
// folder too (shell_hooks.py:87-107, Path.cwd()).

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HermesScenario is the scenario's hermes part.
type HermesScenario struct {
	// ToolCalls are made in each turn, in order.
	ToolCalls []HermesToolCall `json:"tool_calls"`
	// TurnEnd is on_session_end's outcome: "" (completed), "failed", or
	// "interrupted".
	TurnEnd string `json:"turn_end"`
	// Reply is the assistant's text for a turn; default "ok: <the line>".
	Reply string `json:"reply"`
	// Approve answers an escalated call ({"action":"approve"}): "y" runs
	// it; anything else refuses it.
	Approve string `json:"approve"`
}

// HermesToolCall is one tool call.
type HermesToolCall struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
}

// hermesFake is one fake-hermes run.
type hermesFake struct {
	sc      *Scenario
	root    string // Hermes' root home
	home    string // the profile's home
	profile string
	in      *bufio.Scanner
	id      string
	args    []string
}

// hermesSessionRE is the two shapes Hermes 0.23.1 mints: local-<sha256 hex>,
// seen live, and the older <date>_<time>_<hex> (internal/adapter.ValidSessionID).
var hermesSessionRE = regexp.MustCompile(`^(local-[0-9a-f]{64}|[0-9]{8}_[0-9]{6}_[0-9a-f]{6,12})$`)

func runHermes() int {
	if code, ok := hermesSpawned(); ok {
		return code
	}
	sc, err := loadScenario()
	if err != nil {
		return fail(97, "scenario: %v", err)
	}
	h := &hermesFake{sc: sc, in: bufio.NewScanner(os.Stdin), args: os.Args[1:]}
	if h.root, err = hermesRoot(); err != nil {
		return fail(97, "hermes: %v", err)
	}
	args, profile, err := takeProfile(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	h.profile, h.home = "default", h.root
	if profile != "" && profile != "default" {
		h.profile, h.home = profile, filepath.Join(h.root, "profiles", profile)
		if !isDir(h.home) {
			fmt.Fprintf(os.Stderr, "Error: Profile '%s' does not exist. Create it with: hermes profile create %s\n", profile, profile)
			h.record("error", nil)
			return 1
		}
	}
	return h.main(args)
}

// hermesRoot is $HERMES_HOME (its root when it names a profile), else
// ~/.hermes.
func hermesRoot() (string, error) {
	if v := os.Getenv("HERMES_HOME"); v != "" {
		if !filepath.IsAbs(v) {
			return "", errors.New("HERMES_HOME must be absolute")
		}
		if filepath.Base(filepath.Dir(v)) == "profiles" {
			return filepath.Dir(filepath.Dir(v)), nil
		}
		return filepath.Clean(v), nil
	}
	return filepath.Join(os.Getenv("HOME"), ".hermes"), nil
}

// takeProfile takes -p/--profile out of args.
func takeProfile(args []string) (rest []string, profile string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(rest, args[i:]...), profile, nil
		case a == "-p" || a == "--profile":
			if i+1 >= len(args) {
				return nil, "", errors.New("hermes: error: argument -p/--profile: expected one argument")
			}
			profile = args[i+1]
			i++
		case strings.HasPrefix(a, "--profile="):
			profile = strings.TrimPrefix(a, "--profile=")
		default:
			rest = append(rest, a)
		}
	}
	return rest, profile, nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func (h *hermesFake) version() string {
	if h.sc.VersionOutput != "" {
		return h.sc.VersionOutput
	}
	v := h.sc.Version
	if v == "" {
		v = "0.23.1"
	}
	// The real first line's shape (the fact sheet's live --version, scrubbed).
	return "Hermes Agent v" + v + " (2026.9.24) · stable · local fake\n"
}

const hermesHelp = `usage: hermes [-h] [--version] [-p PROFILE] [-m MODEL] [--resume SESSION]
              [--continue [CONTINUE]] [--yolo] [--accept-hooks] [--cli] [--tui]
              {chat,model,auth,config,profile,sessions,hooks,gateway} ...

options:
  -h, --help            show this help message and exit
  --version, -V         Show version and exit
  --resume, -r SESSION  Resume a previous session by ID or title
  --no-restore-cwd      Do not restore the session's working directory
  --continue, -c [CONTINUE]
  --cli                 Force the classic prompt_toolkit CLI
  --tui                 Launch the modern TUI
`

const hermesChatHelp = `usage: hermes chat [-h] [-q QUERY | --query-file PATH] [-m MODEL] [-Q]
                   [--resume SESSION_ID] [--no-restore-cwd] [--cli]

options:
  -q, --query QUERY     Single query (non-interactive mode)
  --query-file PATH     Read the query from a file
  -Q, --quiet           Quiet mode
  --resume, -r SESSION_ID
  --no-restore-cwd
  --cli
`

// chatValues and chatSwitches are the chat flags the fake takes
// (hermes_cli/_parser.py:147-330).
var (
	chatValues   = []string{"-m", "--model", "--provider", "--reasoning", "-t", "--toolsets", "-s", "--skills", "--resume", "-r", "--max-turns", "-q", "--query", "--query-file", "--format", "--source"}
	chatSwitches = []string{"--cli", "--tui", "-Q", "--quiet", "--no-restore-cwd", "--yolo", "--accept-hooks", "--ignore-user-config", "--ignore-rules", "--safe-mode", "--checkpoints", "-v", "--verbose", "--pass-session-id"}
)

func (h *hermesFake) main(args []string) int {
	if r, ok := (&fake{sc: h.sc}).reply(h.args); ok {
		h.record("reply", nil)
		fmt.Print(r.Stdout)
		fmt.Fprint(os.Stderr, r.Stderr)
		return r.ExitCode
	}
	switch {
	case len(args) == 1 && (args[0] == "--version" || args[0] == "-V"):
		h.record("version", nil)
		fmt.Print(h.version())
		return 0
	case len(args) == 1 && (args[0] == "--help" || args[0] == "-h"):
		h.record("help", nil)
		fmt.Print(hermesHelp)
		return 0
	case len(args) == 2 && args[0] == "chat" && (args[1] == "--help" || args[1] == "-h"):
		h.record("help", nil)
		fmt.Print(hermesChatHelp)
		return 0
	case len(args) >= 2 && args[0] == "profile" && args[1] == "create":
		return h.profileCreate(args[2:])
	case len(args) >= 2 && args[0] == "profile" && args[1] == "delete":
		return h.profileDelete(args[2:])
	case len(args) >= 2 && args[0] == "config" && args[1] == "get":
		return h.configGet(args[2:])
	case len(args) >= 2 && args[0] == "config" && args[1] == "set":
		return h.configSet(args[2:])
	case len(args) >= 2 && args[0] == "sessions" && args[1] == "export":
		return h.export(args[2:])
	case len(args) >= 1 && args[0] == "chat":
		return h.chat(args[1:])
	}
	h.record("error", nil)
	fmt.Fprintf(os.Stderr, "hermes: error: the fake does not model %q\n", strings.Join(args, " "))
	return 2
}

func (h *hermesFake) profileCreate(args []string) int {
	if len(args) != 3 || args[1] != "--no-alias" || args[2] != "--no-skills" || !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`).MatchString(args[0]) {
		h.record("error", nil)
		fmt.Fprintln(os.Stderr, "hermes: error: profile create <name> --no-alias --no-skills")
		return 2
	}
	dir := filepath.Join(h.root, "profiles", args[0])
	if isDir(dir) {
		fmt.Fprintf(os.Stderr, "Error: Profile '%s' already exists\n", args[0])
		return 1
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(1, "hermes: %v", err)
	}
	// The model block of the active profile, as _seed_model_config copies it.
	seed := "model:\n  default: fake-model\n"
	if b, err := os.ReadFile(filepath.Join(h.home, "config.yaml")); err == nil {
		if m, ok := parseYAML(string(b))["model"].(map[string]any); ok {
			seed = "model:\n"
			for _, k := range sortedKeys(m) {
				seed += fmt.Sprintf("  %s: %v\n", k, m[k])
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(seed), 0o600); err != nil {
		return fail(1, "hermes: %v", err)
	}
	h.profile, h.home = args[0], dir
	h.record("profile-create", nil)
	fmt.Printf("Profile '%s' created at %s\n", args[0], dir)
	return 0
}

// profileDelete is `hermes profile delete <name> --yes` (LIVE: it removed the
// profile). The default is never deleted.
func (h *hermesFake) profileDelete(args []string) int {
	if len(args) != 2 || args[1] != "--yes" || args[0] == "default" || !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`).MatchString(args[0]) {
		h.record("error", nil)
		fmt.Fprintln(os.Stderr, "hermes: error: profile delete <name> --yes")
		return 2
	}
	dir := filepath.Join(h.root, "profiles", args[0])
	if !isDir(dir) {
		fmt.Fprintf(os.Stderr, "Error: Profile '%s' does not exist\n", args[0])
		return 1
	}
	if err := os.RemoveAll(dir); err != nil {
		return fail(1, "hermes: %v", err)
	}
	h.record("profile-delete", nil)
	fmt.Printf("Profile '%s' deleted\n", args[0])
	return 0
}

// hermesDefaults are the defaults config get merges under the file, for the
// keys aggy reads (hermes_cli/config_defaults.py:1646-1739).
func hermesDefaults() map[string]any {
	return map[string]any{
		"approvals":         map[string]any{"mode": "smart", "deny": []any{}},
		"hooks":             map[string]any{},
		"hooks_auto_accept": false,
		"auth":              map[string]any{"adopt_external_logins": true},
		"updates":           map[string]any{"check": true},
	}
}

func (h *hermesFake) config() (map[string]any, error) {
	cfg := hermesDefaults()
	b, err := os.ReadFile(filepath.Join(h.home, "config.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	merge(cfg, parseYAML(string(b)))
	return cfg, nil
}

// metricsAnswered says the profile's config answers the shared-metrics offer
// at offer version 2 (shared_metrics_consent.py:57-72).
func (h *hermesFake) metricsAnswered() bool {
	cfg, err := h.config()
	if err != nil {
		return false
	}
	t, _ := cfg["telemetry"].(map[string]any)
	sm, _ := t["shared_metrics"].(map[string]any)
	_, enabled := sm["enabled"]
	return enabled && sm["offer_version"] == 2
}

func merge(dst, src map[string]any) {
	for k, v := range src {
		if dm, ok := dst[k].(map[string]any); ok {
			if sm, ok := v.(map[string]any); ok {
				merge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

func (h *hermesFake) configGet(args []string) int {
	if len(args) != 2 || args[1] != "--json" {
		h.record("error", nil)
		fmt.Fprintln(os.Stderr, "hermes: error: config get <key> --json")
		return 2
	}
	h.record("config-get", nil)
	cfg, err := h.config()
	if err != nil {
		return fail(1, "hermes: %v", err)
	}
	var v any = cfg
	for _, k := range strings.Split(args[0], ".") {
		m, ok := v.(map[string]any)
		if !ok {
			v = nil
			break
		}
		v = m[k]
	}
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
	return 0
}

// configSet sets a.b: value by its lines, keeping every other line, as
// Hermes keeps comments (hermes_cli/config.py:1957-1967). Depth two only.
func (h *hermesFake) configSet(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "hermes: error: config set <key> <value>")
		return 2
	}
	h.record("config-set", nil)
	parts := strings.Split(args[0], ".")
	if len(parts) > 2 {
		return fail(2, "hermes: config set: the fake sets two levels at most")
	}
	file := filepath.Join(h.home, "config.yaml")
	b, _ := os.ReadFile(file)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(b) == 0 {
		lines = nil
	}
	if len(parts) == 1 {
		done := false
		for i, l := range lines {
			if strings.HasPrefix(l, parts[0]+":") {
				lines[i], done = parts[0]+": "+args[1], true
			}
		}
		if !done {
			lines = append(lines, parts[0]+": "+args[1])
		}
	} else {
		top := -1
		for i, l := range lines {
			if l == parts[0]+":" {
				top = i
			}
		}
		if top < 0 {
			lines = append(lines, parts[0]+":", "  "+parts[1]+": "+args[1])
		} else {
			at := top + 1
			done := false
			for i := top + 1; i < len(lines) && strings.HasPrefix(lines[i], " "); i++ {
				if strings.HasPrefix(lines[i], "  "+parts[1]+":") {
					lines[i], done = "  "+parts[1]+": "+args[1], true
				}
				at = i + 1
			}
			if !done {
				lines = append(lines[:at], append([]string{"  " + parts[1] + ": " + args[1]}, lines[at:]...)...)
			}
		}
	}
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return fail(1, "hermes: %v", err)
	}
	return 0
}

// The fake's sessions: one JSON file per session under the profile's home,
// in the export's shape (hermes_state_portability.py:347-355).
type hermesSession struct {
	ID       string           `json:"id"`
	Source   string           `json:"source"`
	Messages []map[string]any `json:"messages"`
}

func (h *hermesFake) sessionPath(id string) string {
	return filepath.Join(h.home, "fake-sessions", id+".json")
}

func (h *hermesFake) export(args []string) int {
	if len(args) < 6 || args[0] != "-" || args[1] != "--session-id" || args[3] != "--format" || args[4] != "jsonl" {
		h.record("error", nil)
		fmt.Fprintln(os.Stderr, "hermes: error: sessions export - --session-id <id> --format jsonl [--redact]")
		return 2
	}
	for _, a := range args[5:] {
		if a != "--redact" {
			fmt.Fprintf(os.Stderr, "hermes: error: unrecognized arguments: %s\n", a)
			return 2
		}
	}
	h.id = args[2]
	h.record("export", nil)
	b, err := os.ReadFile(h.sessionPath(args[2]))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Session '%s' not found.\n", args[2])
		return 1
	}
	var s hermesSession
	if json.Unmarshal(b, &s) != nil {
		return fail(1, "hermes: a bad session file")
	}
	out, _ := json.Marshal(s)
	fmt.Println(string(out))
	return 0
}

// newHermesID mints the shape every live session showed: local-<64 hex>.
func newHermesID() string {
	return "local-" + strings.ReplaceAll(newUUID()+newUUID(), "-", "")
}

func (h *hermesFake) chat(args []string) int {
	flags := map[string][]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		switch {
		case contains(chatSwitches, name) && !hasVal:
			flags[name] = append(flags[name], "")
		case contains(chatValues, name):
			if !hasVal {
				if i+1 >= len(args) {
					fmt.Fprintf(os.Stderr, "hermes chat: error: argument %s: expected one argument\n", name)
					return 2
				}
				i++
				val = args[i]
			}
			flags[name] = append(flags[name], val)
		default:
			h.record("error", nil)
			fmt.Fprintf(os.Stderr, "hermes: error: unrecognized arguments: %s\n", a)
			return 2
		}
	}
	last := func(names ...string) string {
		for _, n := range names {
			if v := flags[n]; len(v) > 0 {
				return v[len(v)-1]
			}
		}
		return ""
	}
	h.id = last("--resume", "-r")
	if h.id != "" {
		if !hermesSessionRE.MatchString(h.id) || !fileExists(h.sessionPath(h.id)) {
			fmt.Fprintf(os.Stderr, "Session not found: %s\n", h.id)
			h.record("error", nil)
			return 1
		}
	} else {
		h.id = newHermesID()
	}
	if q := last("-q", "--query"); q != "" {
		h.record("headless", nil)
		if !h.registerHooks(false) {
			return 1
		}
		h.turn(q)
		if _, quiet := flags["-Q"]; quiet || flags["--quiet"] != nil {
			fmt.Printf("\nsession_id: %s\n", h.id)
		}
		return 0
	}
	if !isTerminal(os.Stdin) && !h.sc.InteractiveWithoutTTY {
		fmt.Fprintln(os.Stderr, "hermes: the classic CLI needs a terminal")
		return 1
	}
	h.record("interactive", nil)
	if !h.registerHooks(true) {
		return 1
	}
	// The first-run "Help improve Hermes?" screen (LIVE, 2026-11-11): shown
	// before the prompt on a profile with no shared-metrics answer at offer
	// version 2, unless --query-file is given (shared_metrics_consent.py:
	// 167-189); it takes the first line typed.
	if last("--query-file") == "" && !h.metricsAnswered() {
		h.record("metrics-offer", nil)
		fmt.Print("Help improve Hermes?\n  Share with Nous\n> No thanks\n")
		if h.in.Scan() {
			_ = h.in.Text()
		}
	}
	fmt.Printf("Hermes Agent (fake) %s\n> ", h.id)
	if qf := last("--query-file"); qf != "" {
		b, err := os.ReadFile(qf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hermes: cannot read %s\n", qf)
			return 1
		}
		h.turn(string(b))
		fmt.Print("> ")
	}
	for h.in.Scan() {
		line := strings.TrimSpace(termReply.ReplaceAllString(h.in.Text(), ""))
		switch line {
		case "/exit", "/quit":
			fmt.Println("Goodbye!")
			return 0
		case "":
		case "/new":
			h.id = newHermesID()
			fmt.Printf("new session %s\n", h.id)
		default:
			h.turn(line)
		}
		fmt.Print("> ")
	}
	return 0
}

// scriptMtime is the consent record's script_mtime_at_approval: the mtime of
// the command's first word (shell_hooks.py:833-840), or nil.
func scriptMtime(cmd string) any {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return nil
	}
	st, err := os.Stat(f[0])
	if err != nil {
		return nil
	}
	return st.ModTime().UTC().Format(time.RFC3339Nano)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// hermesHook is one configured hook, consented to or not.
type hermesHook struct {
	event, command string
	timeout        int
	failClosed     bool
}

// hooks are the profile's configured hooks this run registered.
var hooksOn []hermesHook

// registerHooks reads the profile's hooks and asks for each unconsented
// pair on the terminal when prompt is set; one refused is not registered.
// false: the config could not be read.
func (h *hermesFake) registerHooks(prompt bool) bool {
	cfg, err := h.config()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes: config.yaml: %v\n", err)
		return false
	}
	allow := filepath.Join(h.home, "shell-hooks-allowlist.json")
	var al struct {
		Approvals []map[string]any `json:"approvals"`
	}
	if b, err := os.ReadFile(allow); err == nil {
		_ = json.Unmarshal(b, &al)
	}
	has := func(ev, cmd string) bool {
		for _, a := range al.Approvals {
			if a["event"] == ev && a["command"] == cmd {
				return true
			}
		}
		return false
	}
	auto, _ := cfg["hooks_auto_accept"].(bool)
	hooks, _ := cfg["hooks"].(map[string]any)
	for _, ev := range sortedKeys(hooks) {
		list, _ := hooks[ev].([]any)
		for _, e := range list {
			m, _ := e.(map[string]any)
			cmd, _ := m["command"].(string)
			if cmd == "" {
				continue
			}
			hk := hermesHook{event: ev, command: cmd, timeout: 60}
			if t, ok := m["timeout"].(int); ok {
				hk.timeout = t
			}
			hk.failClosed, _ = m["fail_closed"].(bool)
			if !has(ev, cmd) {
				ok := auto
				if !ok && prompt {
					fmt.Printf("\n⚠ Hermes is about to register a shell hook that will run a\n  command on your behalf.\n\n    Event:   %s\n    Command: %s\n\nAllow this hook to run? [y/N]: ", ev, cmd)
					if h.in.Scan() {
						a := strings.ToLower(strings.TrimSpace(termReply.ReplaceAllString(h.in.Text(), "")))
						ok = a == "y" || a == "yes"
					}
				}
				if !ok {
					appendJSON(os.Getenv("AGGY_FAKE_RECORD"), map[string]any{"vendor": "hermes", "mode": "hook-refused", "args": []string{ev, cmd}})
					continue
				}
				al.Approvals = append(al.Approvals, map[string]any{"event": ev, "command": cmd, "approved_at": time.Now().UTC().Format(time.RFC3339), "script_mtime_at_approval": scriptMtime(cmd)})
				b, _ := json.Marshal(al)
				_ = os.WriteFile(allow, b, 0o600)
			}
			hooksOn = append(hooksOn, hk)
		}
	}
	return true
}

// turn is one turn: the scenario's tool calls, the reply, and the turn end.
func (h *hermesFake) turn(text string) {
	hs := h.sc.Hermes
	if hs == nil {
		hs = &HermesScenario{}
	}
	for _, c := range hs.ToolCalls {
		outcome := h.toolCall(c, hs)
		appendJSON(os.Getenv("AGGY_FAKE_RECORD"), map[string]any{"vendor": "hermes", "mode": "tool", "args": []string{c.Tool, outcome}, "session_id": h.id})
	}
	reply := hs.Reply
	if reply == "" {
		reply = "ok: " + text
	}
	fmt.Println(reply)
	h.saveTurn(text, reply)
	// extra's keys as seen live (2026-11-11); aggy reads only failed and
	// interrupted.
	end := map[string]any{"completed": hs.TurnEnd == "", "failed": hs.TurnEnd == "failed", "interrupted": hs.TurnEnd == "interrupted",
		"kanban_task_id": nil, "model": "fake-model", "platform": "cli", "task_id": "", "telemetry_schema_version": 1, "turn_exit_reason": "", "turn_id": newUUID()}
	for _, hk := range hooksOn {
		if hk.event == "on_session_end" {
			h.runHook(hk, h.payload(hk.event, "", nil, end))
		}
	}
}

func (h *hermesFake) saveTurn(user, reply string) {
	var s hermesSession
	if b, err := os.ReadFile(h.sessionPath(h.id)); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	s.ID, s.Source = h.id, "cli"
	now := float64(time.Now().Unix())
	s.Messages = append(s.Messages, map[string]any{"role": "user", "content": user, "timestamp": now},
		map[string]any{"role": "assistant", "content": reply, "timestamp": now})
	_ = os.MkdirAll(filepath.Dir(h.sessionPath(h.id)), 0o700)
	b, _ := json.Marshal(s)
	_ = os.WriteFile(h.sessionPath(h.id), b, 0o600)
}

// payload is the hook's stdin (shell_hooks.py:87-107, 626-628).
func (h *hermesFake) payload(event, tool string, input json.RawMessage, extra map[string]any) []byte {
	cwd := installDir()
	if extra == nil {
		extra = map[string]any{}
	}
	extra["kanban_task_id"] = nil
	p := map[string]any{"hook_event_name": event, "tool_name": nil, "tool_input": nil, "session_id": h.id, "cwd": cwd, "profile": h.profile, "extra": extra}
	if tool != "" {
		p["tool_name"] = tool
		var in map[string]any
		if json.Unmarshal(input, &in) == nil {
			p["tool_input"] = in
		}
	}
	b, _ := json.Marshal(p)
	return b
}

func installDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "/"
	}
	return filepath.Dir(exe)
}

// toolCall runs one tool call's pre_tool_call hooks and, for terminal,
// approvals.deny, and says what happened: "ran", "blocked: <message>",
// "approved", "refused", or "denied".
func (h *hermesFake) toolCall(c HermesToolCall, hs *HermesScenario) string {
	escalated := false
	for _, hk := range hooksOn {
		if hk.event != "pre_tool_call" {
			continue
		}
		d := h.runHook(hk, h.payload(hk.event, c.Tool, c.Input, map[string]any{"task_id": "", "tool_call_id": "call_1"}))
		switch d["action"] {
		case "block":
			msg, _ := d["message"].(string)
			return "blocked: " + msg
		case "approve":
			escalated = true
		}
	}
	if c.Tool == "terminal" {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(c.Input, &in)
		cfg, _ := h.config()
		ap, _ := cfg["approvals"].(map[string]any)
		deny, _ := ap["deny"].([]any)
		for _, g := range deny {
			if s, ok := g.(string); ok && fnmatch(strings.ToLower(s), strings.ToLower(in.Command)) {
				return "denied"
			}
		}
	}
	if escalated {
		if hs.Approve == "y" {
			return "approved"
		}
		return "refused"
	}
	return "ran"
}

// runHook runs one hook as the daemon does and returns its directive for
// pre_tool_call (shell_hooks.py:560-658), nil for none.
func (h *hermesFake) runHook(hk hermesHook, payload []byte) map[string]any {
	words := strings.Fields(hk.command)
	block := func(why string) map[string]any {
		if hk.event == "pre_tool_call" && hk.failClosed {
			return map[string]any{"action": "block", "message": "hook " + hk.command + " failed closed: " + why}
		}
		return nil
	}
	if len(words) == 0 {
		return block("empty command")
	}
	// The daemon's environment: no AGGY_* variable (the fakes' own aside), and
	// none of the CLI's HERMES_* either; the install folder for a cwd.
	var env []string
	for _, kv := range os.Environ() {
		if (!strings.HasPrefix(kv, "AGGY_") || strings.HasPrefix(kv, "AGGY_FAKE_") || strings.HasPrefix(kv, "AGGY_TEST_")) && !strings.HasPrefix(kv, "HERMES_") {
			env = append(env, kv)
		}
	}
	stdoutS, stderrS, code, failure := runDetached(words, hk.timeout, env, installDir(), payload)
	appendJSON(os.Getenv("AGGY_FAKE_RECORD"), map[string]any{"vendor": "hermes", "mode": "hook", "args": []string{hk.event, strings.TrimSpace(stdoutS), strings.TrimSpace(stderrS)}, "session_id": h.id})
	if failure != "" {
		return block(failure)
	}
	var stdout, stderr strings.Builder
	stdout.WriteString(stdoutS)
	stderr.WriteString(stderrS)
	if hk.event != "pre_tool_call" {
		return nil
	}
	out := strings.TrimSpace(stdout.String())
	var d map[string]any
	parsed := out != "" && json.Unmarshal([]byte(out), &d) == nil
	if code == 2 {
		if parsed && d["action"] == "block" {
			return d
		}
		return map[string]any{"action": "block", "message": strings.TrimSpace(stderr.String())}
	}
	var dir map[string]any
	if parsed {
		switch {
		case d["action"] == "block" || d["decision"] == "block":
			m, _ := d["message"].(string)
			if m == "" {
				m, _ = d["reason"].(string)
			}
			dir = map[string]any{"action": "block", "message": m}
		case d["action"] == "approve":
			dir = map[string]any{"action": "approve", "rule_key": d["rule_key"]}
		}
	}
	if dir == nil && code != 0 {
		return block("hook exited " + strconv.Itoa(code) + " with no directive")
	}
	if dir == nil && out != "" && !parsed {
		return block("unparseable stdout (expected a JSON object)")
	}
	return dir
}

// fnmatch is Python's fnmatch.fnmatchcase: * and ? match any character,
// / included, and [...] a set.
func fnmatch(pat, s string) bool {
	var re strings.Builder
	re.WriteString("^")
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; c {
		case '*':
			re.WriteString(".*")
		case '?':
			re.WriteString(".")
		case '[':
			j := strings.IndexByte(pat[i+1:], ']')
			if j < 0 {
				re.WriteString(`\[`)
				continue
			}
			set := pat[i+1 : i+1+j]
			if strings.HasPrefix(set, "!") {
				set = "^" + set[1:]
			}
			re.WriteString("[" + strings.ReplaceAll(set, `\`, `\\`) + "]")
			i += j + 1
		default:
			re.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	re.WriteString("$")
	r, err := regexp.Compile("(?s)" + re.String())
	return err == nil && r.MatchString(s)
}

// record appends this run to $AGGY_FAKE_RECORD, with HERMES_* values.
func (h *hermesFake) record(mode string, _ []string) {
	p := os.Getenv("AGGY_FAKE_RECORD")
	if p == "" {
		return
	}
	cwd, _ := os.Getwd()
	r := Record{Vendor: "hermes", Mode: mode, Args: h.args, Cwd: cwd, Env: map[string]string{}, SessionID: h.id, PID: os.Getpid(),
		StdinTerminal: isTerminal(os.Stdin)}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		r.EnvKeys = append(r.EnvKeys, k)
		if strings.HasPrefix(k, "AGGY_") || strings.HasPrefix(k, "HERMES_") {
			r.Env[k] = v
		}
	}
	appendJSON(p, r)
}

// parseYAML reads the YAML subset config.yaml uses here: maps by
// indentation, lists of scalars or of maps, quoted and plain scalars, []
// and {}, and comments. Anything else is skipped.
func parseYAML(src string) map[string]any {
	type line struct {
		indent int
		text   string
	}
	var ls []line
	for _, l := range strings.Split(src, "\n") {
		t := strings.TrimRight(l, " \r")
		if s := strings.TrimSpace(t); s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		ls = append(ls, line{len(t) - len(strings.TrimLeft(t, " ")), strings.TrimSpace(t)})
	}
	var block func(i, indent int) (any, int)
	block = func(i, indent int) (any, int) {
		if i < len(ls) && ls[i].indent == indent && strings.HasPrefix(ls[i].text, "- ") || i < len(ls) && ls[i].indent == indent && ls[i].text == "-" {
			var list []any
			for i < len(ls) && ls[i].indent == indent && strings.HasPrefix(ls[i].text+" ", "- ") {
				item := strings.TrimSpace(strings.TrimPrefix(ls[i].text, "-"))
				if k, v, ok := cutKey(item); ok {
					// A map item: its first key on the dash line, the rest
					// indented to that key.
					m := map[string]any{}
					sub := indent + 2
					if v == "" && i+1 < len(ls) && ls[i+1].indent > sub {
						var val any
						val, i = block(i+1, ls[i+1].indent)
						m[k] = val
					} else {
						m[k] = scalar(v)
						i++
					}
					if i < len(ls) && ls[i].indent == sub {
						var rest any
						rest, i = block(i, sub)
						if rm, ok := rest.(map[string]any); ok {
							for kk, vv := range rm {
								m[kk] = vv
							}
						}
					}
					list = append(list, m)
					continue
				}
				list = append(list, scalar(item))
				i++
			}
			return list, i
		}
		m := map[string]any{}
		for i < len(ls) && ls[i].indent == indent {
			k, v, ok := cutKey(ls[i].text)
			if !ok {
				i++
				continue
			}
			if v == "" && i+1 < len(ls) && ls[i+1].indent > indent {
				m[k], i = block(i+1, ls[i+1].indent)
				continue
			}
			if v == "" && i+1 < len(ls) && ls[i+1].indent == indent && strings.HasPrefix(ls[i+1].text, "- ") {
				m[k], i = block(i+1, indent)
				continue
			}
			m[k] = scalar(v)
			i++
		}
		return m, i
	}
	if len(ls) == 0 {
		return map[string]any{}
	}
	v, _ := block(0, ls[0].indent)
	m, _ := v.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// cutKey splits "key: value" (or "key:"), the key unquoted.
func cutKey(s string) (k, v string, ok bool) {
	if strings.HasPrefix(s, `"`) {
		return "", "", false
	}
	k, v, ok = strings.Cut(s, ":")
	if !ok || strings.ContainsAny(k, " \t") {
		return "", "", false
	}
	return k, strings.TrimSpace(v), true
}

func scalar(s string) any {
	switch {
	case s == "[]":
		return []any{}
	case s == "{}":
		return map[string]any{}
	case s == "true" || s == "True":
		return true
	case s == "false" || s == "False":
		return false
	case s == "null" || s == "~":
		return nil
	case strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) && len(s) >= 2:
		return s[1 : len(s)-1]
	case strings.HasPrefix(s, `'`) && strings.HasSuffix(s, `'`) && len(s) >= 2:
		return s[1 : len(s)-1]
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if i := strings.Index(s, " #"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func sortedKeys(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
