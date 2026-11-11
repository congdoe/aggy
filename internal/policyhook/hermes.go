package policyhook

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/safefs"
)

// Hermes Agent's pre_tool_call, at v0.23.1. The payload is
// {hook_event_name, tool_name, tool_input, session_id, cwd, profile, extra}
// (agent/shell_hooks.py:87-107, :626-628; LIVE). cwd is the gateway daemon's
// folder, never the session's, and extra may hold the conversation: neither is
// read here.
//
// Every tool goes through the hook, and Hermes' own approvals cover terminal
// commands only: LIVE (2026-11-11), write_file and execute_code ran with no
// prompt under approvals.mode manual, and pre_tool_call fired for each. So on
// Hermes the hook is the guard, and no opinion is safe only for a tool known
// to be read-only (#134 review). Each tool is in one of four classes, from the
// tool registry at v0.23.1 (tools/*.py, registry.register):
//
//   - Code: judged by its text as a command, and asked even when no rule
//     matches, since code can do what no rule sees. execute_code {code}
//     (tools/code_execution_tool.py:955-968; LIVE, unasked by Hermes),
//     browser_exec {code} (tools/browser_use_cli.py:990-1003), and
//     process_manage's write and submit, which type {data} into a background
//     process's stdin (tools/process_registry.py:2586-2616).
//   - terminal {command, workdir, background, ...} (tools/terminal_tool.py:
//     1617-1670): its command is judged as a shell command, its workdir as a
//     folder; no match is no opinion, since Hermes' manual approvals see it.
//   - Files, by path: read_file {path}, write_file {path, content}, patch
//     {path, ...}, search_files {path, pattern} (tools/file_tools.py:
//     1152-1435; LIVE shapes), and vision_analyze's image_url and video_url
//     when they are local (tools/vision_tools.py:1031). A path outside the
//     repo asks; a write to aggy's state, the trust files, a Claude settings
//     file, or the profile's folder asks; a key-named read asks; a vendor
//     credential file is refused.
//   - Read-only and local, or a search of the web with no file: no opinion.
//     hermesNoOpinion lists them.
//
// Every other tool asks, the tool's name in the message: cronjob_manage,
// delegate_task, skill_manage, skill_view, memory, session_search (which may
// name another profile), computer_use, process_manage's other actions but
// list, poll, log, and wait, any MCP tool, and any name a later Hermes adds.
// A command or file field in its input is judged too, so a deny rule still
// denies. A pre-approval never applies to a code or other tool (Call.Other).

// hermesNoOpinion are the tools known to be read-only, from their source at
// v0.23.1: web_search and web_extract (tools/web_tools.py:560-570), x_search
// (tools/x_search_tool.py:339), clarify, which asks the person
// (tools/clarify_tool.py:218), todo_list, the session's own to-do list
// (tools/todo_tool.py:333), read_terminal and read_window_below, which read
// Hermes' own panes (tools/read_terminal_tool.py:76,
// tools/read_window_tool.py:42), and skills_list (tools/skills_tool.py:753).
var hermesNoOpinion = []string{"web_search", "web_extract", "x_search", "clarify", "todo_list", "read_terminal", "read_window_below", "skills_list"}

// hermesClass is how a tool is judged.
type hermesClass int

const (
	classNone  hermesClass = iota // read-only: no opinion
	classShell                    // terminal: judged; no match, no opinion
	classFile                     // a file tool: judged by path
	classCode                     // runs code: judged, and asks with no match
	classOther                    // not classified: judged, and asks with no match
)

// HermesPayload is the part of a pre_tool_call payload aggy reads.
type HermesPayload struct {
	Event     string                     `json:"hook_event_name"`
	ToolName  string                     `json:"tool_name"`
	ToolInput map[string]json.RawMessage `json:"tool_input"`
	SessionID string                     `json:"session_id"`
	Profile   string                     `json:"profile"`
}

// ParseHermes reads a payload's event, tool, session, and profile; ok is
// false for one that is not a JSON object.
func ParseHermes(payload []byte) (p HermesPayload, ok bool) {
	var probe map[string]json.RawMessage
	if json.Unmarshal(payload, &probe) != nil || probe == nil || json.Unmarshal(payload, &p) != nil {
		return HermesPayload{}, false
	}
	return p, true
}

// hermesCall is one payload read for judging: the tool, its class, the calls,
// the files it reads or writes (absolute, from the repo root), and ok, false
// for input aggy cannot read.
type hermesCall struct {
	tool  string
	class hermesClass
	calls []parity.Call
	files []string
	ok    bool
}

// HermesCalls reads one pre_tool_call payload into the calls to judge. judged
// is false only for a tool aggy has no opinion on (hermesNoOpinion): a
// caller whose plan failed asks for every other one.
func HermesCalls(payload []byte, root string) (calls []parity.Call, judged bool) {
	h := readHermes(payload, root)
	return h.calls, !h.ok || h.class != classNone
}

func readHermes(payload []byte, root string) hermesCall {
	p, ok := ParseHermes(payload)
	if !ok || p.ToolName == "" {
		return hermesCall{class: classOther}
	}
	h := hermesCall{tool: p.ToolName, class: classOther, ok: true}
	field := func(k string) (string, bool, bool) {
		raw, has := p.ToolInput[k]
		if !has {
			return "", false, true
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", true, false
		}
		return s, true, true
	}
	path := func(s string) string {
		if !filepath.IsAbs(s) && !strings.HasPrefix(s, "~") && root != "" {
			return filepath.Join(root, s)
		}
		return s
	}
	// add judges key's text as kind; required says a missing or empty field
	// is unreadable.
	add := func(kind parity.CallKind, key string, required bool) {
		s, has, typed := field(key)
		if !typed || required && (!has || s == "") {
			h.ok = false
			return
		}
		if s == "" {
			return
		}
		if kind != parity.CallShell {
			s = path(s)
			h.files = append(h.files, s)
		}
		h.calls = append(h.calls, parity.Call{Kind: kind, Text: s, Other: h.class == classCode || h.class == classOther})
	}
	switch p.ToolName {
	case "terminal":
		h.class = classShell
		add(parity.CallShell, "command", true)
		if s, has, typed := field("workdir"); !typed {
			h.ok = false
		} else if has && s != "" {
			h.files = append(h.files, path(s))
		}
	case "read_file":
		h.class = classFile
		add(parity.CallRead, "path", true)
	case "write_file", "patch":
		// patch's V4A mode names its files inside the patch text, with no
		// path (file_tools.py:1415-1433): unreadable, so it asks.
		h.class = classFile
		add(parity.CallWrite, "path", true)
	case "search_files":
		h.class = classFile
		if s, has, typed := field("path"); !typed {
			h.ok = false
		} else if has && s != "" {
			h.files = append(h.files, path(s))
			h.calls = append(h.calls, parity.Call{Kind: parity.CallSearch, Text: path(s)})
		}
		if s, _, typed := field("pattern"); !typed {
			h.ok = false
		} else if s != "" {
			h.calls = append(h.calls, parity.Call{Kind: parity.CallSearch, Text: s})
		}
	case "vision_analyze":
		h.class = classFile
		for _, k := range []string{"image_url", "video_url"} {
			s, _, typed := field(k)
			if !typed {
				h.ok = false
			} else if s != "" && !webURL.MatchString(s) {
				add(parity.CallRead, k, false)
			}
		}
	case "execute_code", "browser_exec":
		h.class = classCode
		add(parity.CallShell, "code", true)
	case "process_manage":
		action, _, typed := field("action")
		switch {
		case !typed:
			h.ok = false
		case action == "list" || action == "poll" || action == "log" || action == "wait":
			h.class = classNone
		case action == "write" || action == "submit":
			h.class = classCode
			add(parity.CallShell, "data", false)
		}
	default:
		if slices.Contains(hermesNoOpinion, p.ToolName) {
			h.class = classNone
			break
		}
		// Not classified: a command or a file field is judged too, so a
		// deny rule still denies.
		if _, has := p.ToolInput["command"]; has {
			add(parity.CallShell, "command", false)
		}
		for _, k := range fileKeys {
			if _, has := p.ToolInput[k]; has {
				add(parity.CallWrite, k, false)
			}
		}
	}
	return h
}

// webURL is an http or https URL, which vision_analyze fetches rather than
// reads from disk.
var webURL = regexp.MustCompile(`(?i)^https?://`)

// JudgeHermes is Judge for one Hermes pre_tool_call payload, by the tool's
// class (above). Hermes has no hook answer that allows a call past its own
// approvals, so an exact pre-approval gives no opinion. A file path outside
// the plan's root, or one that starts with ~, asks (ReasonOutside), as does
// one whose folder's real path leads out of the root's. A vendor credential
// file is refused. A code tool, or one not classified, asks when nothing else
// decided (ReasonUnjudged); input aggy cannot read asks (ReasonUnknownInput).
func JudgeHermes(plan DecidePlan, payload []byte) parity.CallDecision {
	h := readHermes(payload, plan.Root)
	if h.ok && h.class == classNone {
		return parity.CallDecision{Verdict: parity.VerdictNone, Reason: parity.ReasonNoMatch}
	}
	d := Judge(plan, h.calls, "", h.ok)
	if !h.ok {
		return d
	}
	if d.Verdict == parity.VerdictAllow {
		d.Verdict = parity.VerdictNone
	}
	if slices.ContainsFunc(h.files, func(f string) bool { return errors.Is(safefs.Check(f), safefs.ErrAuthPath) }) {
		return parity.CallDecision{Verdict: parity.VerdictDeny, Group: d.Group, Reason: parity.ReasonProtected}
	}
	if d.Verdict == parity.VerdictNone && slices.ContainsFunc(h.files, func(f string) bool { return hermesOutside(f, plan.Root) }) {
		d = parity.CallDecision{Verdict: parity.VerdictAsk, Group: d.Group, Strict: d.Strict, Reason: parity.ReasonOutside}
	}
	if d.Verdict == parity.VerdictNone && (h.class == classCode || h.class == classOther) {
		d = parity.CallDecision{Verdict: parity.VerdictAsk, Group: d.Group, Strict: d.Strict, Reason: parity.ReasonUnjudged}
	}
	return d
}

// hermesOutside says a file tool's path is not under root: as written, or
// by the real path of its nearest folder that exists. A path aggy cannot
// place (relative with no root, a leading ~, a folder it cannot resolve)
// counts as outside.
func hermesOutside(p, root string) bool {
	if root == "" || !filepath.IsAbs(p) {
		return true
	}
	if outsideRoot(p, root) {
		return true
	}
	realRoot, err := realPath(root)
	if err != nil {
		return true
	}
	dir := filepath.Dir(filepath.Clean(p))
	for {
		r, err := realPath(dir)
		switch {
		case err == nil:
			return outsideRoot(r, realRoot)
		case !errors.Is(err, fs.ErrNotExist):
			return true
		}
		up := filepath.Dir(dir)
		if up == dir {
			return true
		}
		dir = up
	}
}

// HermesTool is the payload's tool name as aggy may show it: letters, digits,
// and _ . - only, at most 64, else "this tool".
func HermesTool(payload []byte) string {
	p, _ := ParseHermes(payload)
	if p.ToolName == "" || len(p.ToolName) > 64 || !toolName.MatchString(p.ToolName) {
		return "this tool"
	}
	return p.ToolName
}

var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// HermesOutput is the hook's stdout for d on the call to tool
// (agent/shell_hooks.py:637-658, _parse_pre_tool_call): {"action":"block",
// "message":...} refuses the call (LIVE: the model is told the message);
// {"action":"approve","message":...,"rule_key":...} sends it to Hermes' human
// approval gate (tools/approval.py:1246-1270; LIVE: titled "<tool> (plugin
// approval rule)", with Allow once, Allow for this session, Add to permanent
// allowlist, and Deny); nothing leaves it to Hermes. The rule_key is new for
// every call, so a session or permanent answer there should cover that one
// call only; what Hermes does with those two answers for a per-call key is
// unverified. The message is aggy's fixed text, a group name, and the tool's
// name (HermesTool), never the tool input.
func HermesOutput(d parity.CallDecision, tool string) []byte {
	if d.Verdict == parity.VerdictNone || d.Verdict == parity.VerdictAllow {
		return nil
	}
	var msg string
	switch d.Reason {
	case parity.ReasonUnjudged:
		msg = fmt.Sprintf("aggy cannot judge what Hermes' %s tool will do, and Hermes asks nothing before it, so aggy asks: approve this yourself", tool)
	case parity.ReasonOutside:
		msg = fmt.Sprintf("Hermes' %s tool names a file outside this repo, so aggy asks: approve this yourself", tool)
	default:
		b := ClaudeOutput(d)
		var o struct {
			H struct {
				Reason string `json:"permissionDecisionReason"`
			} `json:"hookSpecificOutput"`
		}
		_ = json.Unmarshal(b, &o)
		msg = o.H.Reason
	}
	out := map[string]string{"action": "block", "message": msg}
	if d.Verdict == parity.VerdictAsk {
		out["action"] = "approve"
		out["rule_key"] = "aggy-" + nonce()
	}
	j, _ := json.Marshal(out)
	return append(j, '\n')
}

// HermesBlock is the hook's stdout refusing a call for a reason of aggy's
// own, such as a launch it cannot find.
func HermesBlock(why string) []byte {
	j, _ := json.Marshal(map[string]string{"action": "block", "message": why})
	return append(j, '\n')
}

func nonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("policyhook: no randomness: %v", err))
	}
	return hex.EncodeToString(b[:])
}
