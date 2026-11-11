package main

// fake-agy: a stand-in for Google's Antigravity CLI (agy), which is closed
// source. It models only what aggy relies on, each from the live checks in
// docs/verification.md ("Antigravity CLI", agy 1.5.3 and 1.5.5) or Google's
// docs where marked:
//
//   - Go flag package parsing: -flag and --flag are one flag, a value as
//     -flag=value or -flag value, a boolean takes =true or =false, parsing
//     stops at the first bare word; an unknown flag prints "flags provided but
//     not defined: -<name>" and the usage, and exits 2 (Go's ExitOnError; the
//     real exit status was not captured).
//   - --version prints the version alone; --help prints "Usage of agy:".
//   - --settings <inline JSON or file>: an unknown top-level key exits 1 with
//     `Error: --settings: unknown setting "<key>"`; permissions.allow, ask,
//     and deny are action(target) rules.
//   - The trust dialog in a folder not yet trusted, "Yes, I trust this
//     folder" highlighted: Enter (or 1, or y) trusts, 2 or n exits 0.
//   - <workspace>/.agents/hooks.json loads only in a trusted folder. Hooks run
//     as children, with agy's environment plus ANTIGRAVITY_CONVERSATION_ID and
//     their cwd <workspace>/.agents; the payload has the keys seen live.
//     SessionStart fires at the first turn of a new conversation only, not on
//     a resume; PreToolUse before each tool call; Stop once per turn with
//     fullyIdle true. A PreToolUse that fails (cannot start, exits non-zero,
//     prints something not JSON, runs past its timeout) blocks the tool.
//     deny blocks; force_ask asks always; ask asks unless an allow rule
//     matches, which runs it.
//   - /exit prints "Resume with -c (or command below):" then
//     "agy --conversation=<id>" and exits 0; -c reopens the folder's last
//     conversation; --conversation=<id> one by id.
//
// Its state, a stand-in, is under $HOME/.gemini/antigravity-cli/fake.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AgyScenario is the scenario's agy part.
type AgyScenario struct {
	// ToolCalls are made in each turn, in order.
	ToolCalls []AgyToolCall `json:"tool_calls"`
	// Approve answers agy's own approval prompt: "y" runs the call.
	Approve string `json:"approve"`
	// Reply is the assistant's text; default "ok: <the line>".
	Reply string `json:"reply"`
}

// AgyToolCall is one tool call.
type AgyToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// agyBools are the switches the fake takes (live help, 1.5.3).
var agyBools = []string{"c", "continue", "dangerously-skip-permissions", "disable-slash-commands", "p", "print", "sandbox", "version", "help", "h"}

// agyValues are the flags that take a value (live help, 1.5.3).
var agyValues = []string{"add-dir", "agent", "conversation", "effort", "i", "prompt-interactive", "input-format", "json-schema", "log-file",
	"mode", "model", "new-project", "output-format", "print-timeout", "project", "prompt", "remote-agent", "remote-control", "settings"}

const agyUsage = `Usage of agy:
  -add-dir value
    	Add a directory to the workspace
  -c	Continue the last conversation in this folder
  -continue
    	Continue the last conversation in this folder
  -conversation string
    	Resume a conversation by id
  -dangerously-skip-permissions
    	Skip permission prompts
  -effort string
    	low|medium|high|xhigh|max
  -i string
    	First prompt of an interactive session
  -mode string
    	accept-edits, auto, plan
  -model string
    	Model
  -p	Print mode
  -sandbox
    	Run commands in a sandbox
  -settings string
    	Settings for this session only, never saved: a JSON object of settings.json keys, inline or as a file path
`

// agyFake is one fake-agy run.
type agyFake struct {
	sc       *Scenario
	flags    map[string][]string
	args     []string
	id       string
	newConv  bool
	ws       string
	settings map[string]any
	hooks    map[string][]agyHandler // by event
	in       *bufio.Scanner
	turns    int
}

// agyHandler is one loaded command hook, with its matcher.
type agyHandler struct {
	matcher string
	command string
	timeout int
}

func runAgy() int {
	sc, err := loadScenario()
	if err != nil {
		return fail(97, "scenario: %v", err)
	}
	a := &agyFake{sc: sc, args: os.Args[1:], flags: map[string][]string{}, in: bufio.NewScanner(os.Stdin)}
	a.ws, _ = os.Getwd()
	if err := a.parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprint(os.Stderr, agyUsage)
		a.record("error")
		return 2
	}
	switch {
	case a.has("version"):
		a.record("version")
		v := sc.Version
		if v == "" {
			v = "1.5.5"
		}
		fmt.Println(v)
		return 0
	case a.has("help") || a.has("h"):
		a.record("help")
		if sc.Help != nil {
			fmt.Print(*sc.Help)
		} else {
			fmt.Print(agyUsage)
		}
		return 0
	}
	if s := a.last("settings"); s != "" {
		if err := a.loadSettings(s); err != nil {
			fmt.Fprintln(os.Stderr, err)
			a.record("error")
			return 1
		}
	}
	if a.has("p") || a.has("print") || a.last("prompt") != "" {
		a.record("print")
		fmt.Println("ok")
		return 0
	}
	if !isTerminal(os.Stdin) && !sc.InteractiveWithoutTTY {
		fmt.Fprintln(os.Stderr, "agy: an interactive session needs a terminal")
		return 1
	}
	fmt.Print("\x1b[?1049h\x1b]7501;?\x07Welcome to the Antigravity CLI\n")
	if !a.trusted() {
		fmt.Printf("Accessing workspace: %s\nDo you trust the contents of this project?\nAntigravity CLI requires permission to read, edit, and execute files here.\n> 1. Yes, I trust this folder\n  2. No, exit\n", a.ws)
		a.record("trust-dialog")
		if !a.in.Scan() {
			return 0
		}
		switch strings.ToLower(strings.TrimSpace(termReply.ReplaceAllString(a.in.Text(), ""))) {
		case "", "1", "y", "yes":
			a.trust()
		default:
			fmt.Print("\x1b[?1049l")
			return 0
		}
	}
	if err := a.loadHooks(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch id := a.last("conversation"); {
	case id != "":
		if !agyUUID.MatchString(id) || !fileExists(a.convPath(id)) {
			fmt.Fprintf(os.Stderr, "Error: conversation %s not found\n", id)
			a.record("error")
			return 1
		}
		a.id = id
	case a.has("c") || a.has("continue"):
		b, err := os.ReadFile(a.lastPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error: no conversation to continue in this folder")
			a.record("error")
			return 1
		}
		a.id = strings.TrimSpace(string(b))
	default:
		a.id, a.newConv = newUUID(), true
	}
	a.record("interactive")
	fmt.Printf("agy (fake) %s\n> ", a.id)
	if p := a.last("i"); p != "" || a.last("prompt-interactive") != "" {
		if p == "" {
			p = a.last("prompt-interactive")
		}
		a.turn(p)
		fmt.Print("> ")
	}
	for a.in.Scan() {
		line := strings.TrimSpace(termReply.ReplaceAllString(a.in.Text(), ""))
		switch line {
		case "/exit":
			fmt.Print("\x1b[?1049l")
			fmt.Printf("Resume with -c (or command below):\nagy --conversation=%s\n", a.id)
			return 0
		case "":
		default:
			a.turn(line)
		}
		fmt.Print("> ")
	}
	return 0
}

var agyUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// parse reads args as Go's flag package does.
func (a *agyFake) parse(args []string) error {
	for i := 0; i < len(args); i++ {
		w := args[i]
		if w == "--" {
			return nil
		}
		if !strings.HasPrefix(w, "-") || w == "-" {
			return nil // Go's flag package stops at the first bare word
		}
		name := strings.TrimPrefix(strings.TrimPrefix(w, "-"), "-")
		if name == "" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "=") {
			return fmt.Errorf("bad flag syntax: %s", w)
		}
		name, val, hasVal := strings.Cut(name, "=")
		switch {
		case contains(agyBools, name):
			if hasVal {
				b, err := strconv.ParseBool(val)
				if err != nil {
					return fmt.Errorf("invalid boolean value %q for -%s: parse error", val, name)
				}
				if !b {
					delete(a.flags, name)
					continue
				}
			}
			a.flags[name] = append(a.flags[name], "true")
		case contains(agyValues, name):
			if !hasVal {
				if i+1 >= len(args) {
					return fmt.Errorf("flag needs an argument: -%s", name)
				}
				i++
				val = args[i]
			}
			a.flags[name] = append(a.flags[name], val)
		default:
			return fmt.Errorf("flags provided but not defined: -%s", name)
		}
	}
	return nil
}

func (a *agyFake) has(n string) bool { return len(a.flags[n]) > 0 }

func (a *agyFake) last(n string) string {
	if v := a.flags[n]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

// agySettingsKeys are the settings.json keys the fake knows (Google's docs).
var agySettingsKeys = []string{"permissions", "toolPermission", "trustedWorkspaces", "model"}

// loadSettings reads --settings, inline or a file path (live: both work).
func (a *agyFake) loadSettings(v string) error {
	b := []byte(v)
	if !strings.HasPrefix(strings.TrimSpace(v), "{") {
		var err error
		if b, err = os.ReadFile(v); err != nil {
			return fmt.Errorf("Error: --settings: %v", err)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("Error: --settings: %v", err)
	}
	for k := range m {
		if !contains(agySettingsKeys, k) {
			return fmt.Errorf("Error: --settings: unknown setting %q", k)
		}
	}
	a.settings = m
	return nil
}

func (a *agyFake) stateDir() string {
	return filepath.Join(os.Getenv("HOME"), ".gemini", "antigravity-cli", "fake")
}

func (a *agyFake) key() string {
	s := sha256.Sum256([]byte(a.ws))
	return hex.EncodeToString(s[:8])
}

func (a *agyFake) trusted() bool { return fileExists(filepath.Join(a.stateDir(), "trusted", a.key())) }

func (a *agyFake) trust() {
	p := filepath.Join(a.stateDir(), "trusted", a.key())
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte(a.ws), 0o600)
}

func (a *agyFake) convPath(id string) string { return filepath.Join(a.stateDir(), "conv", id) }

func (a *agyFake) lastPath() string { return filepath.Join(a.stateDir(), "last", a.key()) }

// loadHooks reads <workspace>/.agents/hooks.json: {name: {event: [...]}},
// PreToolUse holding matcher groups and the others handlers.
func (a *agyFake) loadHooks() error {
	a.hooks = map[string][]agyHandler{}
	b, err := os.ReadFile(filepath.Join(a.ws, ".agents", "hooks.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var file map[string]map[string][]json.RawMessage
	if err := json.Unmarshal(b, &file); err != nil {
		return fmt.Errorf("Error: .agents/hooks.json: %v", err)
	}
	type h struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	for _, events := range file {
		for ev, list := range events {
			for _, raw := range list {
				if ev == "PreToolUse" || ev == "PostToolUse" {
					var g struct {
						Matcher string `json:"matcher"`
						Hooks   []h    `json:"hooks"`
					}
					if json.Unmarshal(raw, &g) != nil {
						continue
					}
					for _, x := range g.Hooks {
						a.hooks[ev] = append(a.hooks[ev], agyHandler{g.Matcher, x.Command, x.Timeout})
					}
					continue
				}
				var x h
				if json.Unmarshal(raw, &x) == nil && x.Type == "command" {
					a.hooks[ev] = append(a.hooks[ev], agyHandler{"", x.Command, x.Timeout})
				}
			}
		}
	}
	return nil
}

// payload is a hook's stdin (live keys, 1.5.5).
func (a *agyFake) payload(event string, extra map[string]any) []byte {
	p := map[string]any{"artifactDirectoryPath": filepath.Join(a.stateDir(), "brain", a.id), "conversationId": a.id, "cwd": a.ws,
		"hookEventName": event, "lastUserInput": "the person's prompt", "modelName": "fake-model",
		"transcriptPath": filepath.Join(a.stateDir(), "brain", a.id, "transcript.jsonl"), "workspacePaths": []string{a.ws}}
	for k, v := range extra {
		p[k] = v
	}
	b, _ := json.Marshal(p)
	return b
}

// runHook runs one handler as agy does: a child with agy's environment and
// ANTIGRAVITY_CONVERSATION_ID, in <workspace>/.agents. ok is false for any
// failure, which blocks a PreToolUse tool.
func (a *agyFake) runHook(hk agyHandler, payload []byte) (out []byte, ok bool) {
	t := hk.timeout
	if t <= 0 {
		t = 30
	}
	cmd := exec.Command("/bin/sh", "-c", hk.command)
	cmd.Dir = filepath.Join(a.ws, ".agents")
	cmd.Env = append(os.Environ(), "ANTIGRAVITY_CONVERSATION_ID="+a.id)
	cmd.Stdin = bytes.NewReader(payload)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, io.Discard
	if err := cmd.Start(); err != nil {
		return nil, false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return nil, false
		}
	case <-time.After(time.Duration(t) * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return nil, false
	}
	return buf.Bytes(), true
}

// turn is one turn: SessionStart for a new conversation's first, each tool
// call, the reply, and Stop.
func (a *agyFake) turn(text string) {
	sc := a.sc.Agy
	if sc == nil {
		sc = &AgyScenario{}
	}
	if a.newConv && a.turns == 0 {
		for _, hk := range a.hooks["SessionStart"] {
			a.runHook(hk, a.payload("SessionStart", nil))
		}
	}
	a.turns++
	_ = os.MkdirAll(filepath.Dir(a.convPath(a.id)), 0o700)
	_ = os.WriteFile(a.convPath(a.id), []byte(a.ws), 0o600)
	_ = os.MkdirAll(filepath.Dir(a.lastPath()), 0o700)
	_ = os.WriteFile(a.lastPath(), []byte(a.id), 0o600)
	for i, c := range sc.ToolCalls {
		outcome, decision := a.toolCall(i, c, sc)
		appendJSON(os.Getenv("AGGY_FAKE_RECORD"), map[string]any{"vendor": "agy", "mode": "tool", "args": []string{c.Name, outcome, decision}, "session_id": a.id})
	}
	reply := sc.Reply
	if reply == "" {
		reply = "ok: " + text
	}
	fmt.Println(reply)
	for _, hk := range a.hooks["Stop"] {
		a.runHook(hk, a.payload("Stop", map[string]any{"error": "", "executionNum": 0, "fullyIdle": true, "terminationReason": "NO_TOOL_CALL"}))
	}
}

// toolCall judges one call: the PreToolUse hooks, then the settings rules,
// then agy's own prompt. outcome is "ran", "denied", "blocked" (a hook
// failed), or "declined"; decision is the hook's.
func (a *agyFake) toolCall(i int, c AgyToolCall, sc *AgyScenario) (outcome, decision string) {
	payload := a.payload("PreToolUse", map[string]any{"stepIdx": i, "toolCall": map[string]any{"id": fmt.Sprintf("call-%d", i), "name": c.Name, "args": json.RawMessage(c.Args)}})
	decision = "none"
	for _, hk := range a.hooks["PreToolUse"] {
		if hk.matcher != "" {
			if ok, _ := regexp.MatchString("^(?:"+hk.matcher+")$", c.Name); !ok {
				continue
			}
		}
		out, ok := a.runHook(hk, payload)
		if !ok {
			return "blocked", "failed"
		}
		if len(bytes.TrimSpace(out)) == 0 {
			continue
		}
		var d struct {
			Decision string `json:"decision"`
		}
		if json.Unmarshal(out, &d) != nil {
			return "blocked", "failed"
		}
		switch d.Decision {
		case "deny":
			return "denied", "deny"
		case "force_ask", "ask", "allow", "":
			if decision == "none" || d.Decision == "force_ask" {
				decision = d.Decision
			}
		default:
			return "blocked", "failed"
		}
	}
	rule := a.ruleFor(c)
	prompt := func() (string, string) {
		fmt.Printf("Approve %s? 1. Yes, run command  2. No, cancel\n", c.Name)
		if sc.Approve == "y" {
			return "ran", decision
		}
		return "declined", decision
	}
	switch {
	case rule == "deny":
		return "denied", decision
	case decision == "force_ask":
		return prompt()
	case decision == "ask" && rule == "allow":
		return "ran", decision
	case decision == "ask" || rule == "ask":
		return prompt()
	case decision == "allow" || rule == "allow" || a.has("dangerously-skip-permissions"):
		return "ran", decision
	}
	return prompt()
}

// ruleFor is the strongest settings rule matching c: deny, ask, allow, or "".
func (a *agyFake) ruleFor(c AgyToolCall) string {
	perms, _ := a.settings["permissions"].(map[string]any)
	var args map[string]any
	_ = json.Unmarshal(c.Args, &args)
	target, action := "", ""
	switch c.Name {
	case "run_command":
		action = "command"
		target, _ = args["CommandLine"].(string)
	case "view_file", "read_file":
		action = "read_file"
		target, _ = args["AbsolutePath"].(string)
	case "write_to_file", "replace_file_content":
		action = "write_file"
		target, _ = args["TargetFile"].(string)
	}
	// The person's own settings.json, where agy persists an "always allow"
	// (Google's docs), adds its rules to --settings's.
	var saved map[string]any
	if b, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".gemini", "antigravity-cli", "settings.json")); err == nil {
		_ = json.Unmarshal(b, &saved)
	}
	savedPerms, _ := saved["permissions"].(map[string]any)
	for _, kind := range []string{"deny", "ask", "allow"} {
		list, _ := perms[kind].([]any)
		more, _ := savedPerms[kind].([]any)
		list = append(list, more...)
		for _, r := range list {
			s, _ := r.(string)
			act, spec, ok := strings.Cut(strings.TrimSuffix(s, ")"), "(")
			if !ok || act != action || target == "" {
				continue
			}
			if spec == target || strings.HasPrefix(target, spec+" ") || fakeGlob(spec, target) ||
				action != "command" && fakeGlob(strings.TrimPrefix(spec, "**/"), filepath.Base(target)) {
				return kind
			}
		}
	}
	return ""
}

func (a *agyFake) record(mode string) {
	p := os.Getenv("AGGY_FAKE_RECORD")
	if p == "" {
		return
	}
	r := Record{Vendor: "agy", Mode: mode, Args: a.args, Cwd: a.ws, Env: map[string]string{}, SessionID: a.id, PID: os.Getpid(),
		StdinTerminal: isTerminal(os.Stdin), Flags: a.flags}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		r.EnvKeys = append(r.EnvKeys, k)
		if strings.HasPrefix(k, "AGGY_") {
			r.Env[k] = v
		}
	}
	appendJSON(p, r)
}
