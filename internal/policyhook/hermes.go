package policyhook

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/parity"
)

// Hermes Agent's pre_tool_call, at v0.23.1. The payload is
// {hook_event_name, tool_name, tool_input, session_id, cwd, profile, extra}
// (agent/shell_hooks.py:87-107, :626-628; live, docs/verification.md). cwd
// is the gateway daemon's folder, never the session's, and extra may hold the
// conversation: neither is read here.
//
// Every tool goes through the hook, not only terminal: live (T6933,
// 2026-11-11) read_file, write_file, and search_files did, and write_file
// wrote with no approval prompt under approvals.mode manual. Hermes'
// approvals cover terminal commands (and execute_code, below) only, so for
// the file tools this hook is the only check. The tools judged by name, with
// the input each takes (tools/terminal_tool.py:1750-1758,
// tools/file_tools.py:1152-1435; the shapes live): terminal {command};
// read_file {path}; write_file {path, content}; patch {path, ...};
// search_files {path, pattern}. The rest of the registry, at v0.23.1, decided
// from its source and not checked live:
//   - execute_code {code} runs a script that can do anything a shell can
//     (tools/code_execution_tool.py:955-968; Hermes' own approval guards it,
//     tools/approval.py:1345-1370): its code is judged as a command's text.
//   - process_manage {action, data}: write and submit type data into a
//     running process's stdin (tools/process_registry.py:2586-2616), judged
//     as a command; its other actions get no opinion.
//   - cronjob_manage schedules work that runs later, outside this session
//     (tools/cronjob_tools.py:1260), and computer_use drives the desktop
//     (tools/computer_use_tool.py:13): every call asks.
//   - delegate_task starts a subagent (tools/delegate_tool.py:934); whether
//     the subagent's own tool calls reach this hook is unverified. No opinion.
//   - web, browser, image, video, speech, memory, skills, todo, clarify, and
//     the screen and terminal-pane tools: no opinion, unless their input has
//     a command or a file field, below.
//   - Any other name, an MCP tool among them, is judged by its input as in
//     ClaudeCalls and never pre-approved: a command as a command's text, a
//     path, file_path, or notebook_path as a file it may write.

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

// hermesAskTools ask on every call: what they do cannot be judged from
// their input.
var hermesAskTools = []string{"cronjob_manage", "computer_use"}

// hermesCall is one payload read for judging: the calls, the files the tool
// reads or writes (absolute, from the repo root), and whether it asks
// whatever the calls say. ok is false for input aggy cannot read.
type hermesCall struct {
	calls []parity.Call
	files []string
	ask   bool
	ok    bool
}

// HermesCalls reads one pre_tool_call payload into the calls to judge. ok is
// false for a payload that is not a JSON object, or a known tool whose field
// is missing or mistyped: such a call is asked about, never allowed. A
// relative path is taken from root, the launch's repo root, never from the
// payload's cwd. A tool that asks on every call (hermesAskTools) reads as
// not ok, so a caller that only counts calls asks too.
func HermesCalls(payload []byte, root string) (calls []parity.Call, ok bool) {
	h := readHermes(payload, root)
	return h.calls, h.ok && !h.ask
}

func readHermes(payload []byte, root string) hermesCall {
	p, ok := ParseHermes(payload)
	if !ok || p.ToolName == "" {
		return hermesCall{}
	}
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
	one := func(kind parity.CallKind, key string, other bool) hermesCall {
		s, has, typed := field(key)
		if !has || !typed || s == "" {
			return hermesCall{}
		}
		h := hermesCall{ok: true}
		if kind != parity.CallShell {
			s = path(s)
			h.files = []string{s}
		}
		h.calls = []parity.Call{{Kind: kind, Text: s, Other: other}}
		return h
	}
	switch {
	case p.ToolName == "terminal":
		return one(parity.CallShell, "command", false)
	case p.ToolName == "read_file":
		return one(parity.CallRead, "path", false)
	case p.ToolName == "write_file" || p.ToolName == "patch":
		// patch's V4A mode names its files inside the patch text, with no
		// path (file_tools.py:1415-1433): unreadable, so it asks.
		return one(parity.CallWrite, "path", false)
	case p.ToolName == "search_files":
		h := hermesCall{ok: true}
		for _, k := range []string{"path", "pattern"} {
			s, has, typed := field(k)
			if has && !typed {
				return hermesCall{}
			}
			if s == "" {
				continue
			}
			if k == "path" {
				s = path(s)
				h.files = append(h.files, s)
			}
			h.calls = append(h.calls, parity.Call{Kind: parity.CallSearch, Text: s})
		}
		return h
	case p.ToolName == "execute_code":
		return one(parity.CallShell, "code", true)
	case p.ToolName == "process_manage":
		action, has, typed := field("action")
		if !has || !typed {
			return hermesCall{}
		}
		if action != "write" && action != "submit" {
			return hermesCall{ok: true}
		}
		return one(parity.CallShell, "data", true)
	case slices.Contains(hermesAskTools, p.ToolName):
		return hermesCall{ok: true, ask: true}
	}
	if _, has := p.ToolInput["command"]; has {
		return one(parity.CallShell, "command", true)
	}
	for _, k := range fileKeys {
		if _, has := p.ToolInput[k]; has {
			return one(parity.CallWrite, k, true)
		}
	}
	return hermesCall{ok: true}
}

// JudgeHermes is Judge for one Hermes pre_tool_call payload. Hermes has no
// hook answer that allows a call past its own approvals, so an exact
// pre-approval gives no opinion, and Hermes' own manual approvals decide. A
// file tool's path outside the plan's root, or one that starts with ~, asks
// (ReasonOutside), as does one whose folder's real path leads out of the
// root's: Hermes asks nothing before its file tools, so this is the only ask
// a read of ~/.ssh or a write to another repo meets. aggy's state, the trust
// files, and the profile's own folder are protected paths in the plan, and a
// write there asks (ReasonProtected).
func JudgeHermes(plan DecidePlan, payload []byte) parity.CallDecision {
	h := readHermes(payload, plan.Root)
	if h.ok && h.ask {
		return parity.CallDecision{Verdict: parity.VerdictAsk, Reason: parity.ReasonUnknownInput}
	}
	d := Judge(plan, h.calls, "", h.ok)
	if d.Verdict == parity.VerdictAllow {
		d.Verdict = parity.VerdictNone
	}
	if h.ok && d.Verdict == parity.VerdictNone && slices.ContainsFunc(h.files, func(f string) bool { return hermesOutside(f, plan.Root) }) {
		d = parity.CallDecision{Verdict: parity.VerdictAsk, Group: d.Group, Strict: d.Strict, Reason: parity.ReasonOutside}
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

// HermesOutput is the hook's stdout for d (agent/shell_hooks.py:637-658,
// _parse_pre_tool_call): {"action":"block","message":...} refuses the call
// (live: the model is told the message); {"action":"approve","message":...,
// "rule_key":...} sends it to Hermes' human approval gate, the one its
// dangerous commands use (tools/approval.py:1246-1270; live: titled
// "<tool> (plugin approval rule)", with Allow once, Allow for this session,
// Add to permanent allowlist, and Deny); nothing leaves it to Hermes. The
// rule_key is new for every call, so a session or permanent answer there
// should cover that one call only and never stop a later policy match from
// asking; what Hermes does with those two answers for a per-call key is
// unverified. The message is aggy's fixed text and a group name, never the
// tool input.
func HermesOutput(d parity.CallDecision) []byte {
	if d.Verdict == parity.VerdictNone || d.Verdict == parity.VerdictAllow {
		return nil
	}
	b := ClaudeOutput(d)
	var o struct {
		H struct {
			Reason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	_ = json.Unmarshal(b, &o)
	out := map[string]string{"action": "block", "message": o.H.Reason}
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
