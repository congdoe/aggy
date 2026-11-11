package policyhook

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/congdoe/aggy/internal/parity"
)

// Hermes Agent's pre_tool_call, at v0.23.1. The payload is
// {hook_event_name, tool_name, tool_input, session_id, cwd, profile, extra}
// (agent/shell_hooks.py:87-107, :626-628; live, docs/verification.md). cwd
// is the gateway daemon's folder, never the session's, and extra may hold the
// conversation: neither is read here. The tools judged by name, with the
// input field each takes (tools/terminal_tool.py:1750-1758 and
// tools/file_tools.py:1152-1435): terminal's command; read_file's path;
// write_file's and patch's path; search_files' path and pattern.

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

// HermesCalls reads one pre_tool_call payload into the calls to judge. ok is
// false for a payload that is not a JSON object, or a known tool whose field
// is missing or mistyped: such a call is asked about, never allowed. A tool
// not known by name is judged by its input as in ClaudeCalls, and never
// pre-approved: a command as a shell command, a path or file_path as a file
// it may write; with neither, no call and no opinion. A relative path is
// taken from root, the launch's repo root, never from the payload's cwd.
func HermesCalls(payload []byte, root string) (calls []parity.Call, ok bool) {
	p, ok := ParseHermes(payload)
	if !ok || p.ToolName == "" {
		return nil, false
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
		if !filepath.IsAbs(s) && root != "" {
			return filepath.Join(root, s)
		}
		return s
	}
	one := func(kind parity.CallKind, key string) ([]parity.Call, bool) {
		s, has, typed := field(key)
		if !has || !typed || s == "" {
			return nil, false
		}
		if kind != parity.CallShell {
			s = path(s)
		}
		return []parity.Call{{Kind: kind, Text: s}}, true
	}
	switch p.ToolName {
	case "terminal":
		return one(parity.CallShell, "command")
	case "read_file":
		return one(parity.CallRead, "path")
	case "write_file", "patch":
		// patch's V4A mode names its files inside the patch text, with no
		// path (file_tools.py:1415-1433): unreadable, so it asks.
		return one(parity.CallWrite, "path")
	case "search_files":
		for _, k := range []string{"path", "pattern"} {
			s, has, typed := field(k)
			if has && !typed {
				return nil, false
			}
			if s != "" {
				if k == "path" {
					s = path(s)
				}
				calls = append(calls, parity.Call{Kind: parity.CallSearch, Text: s})
			}
		}
		return calls, true
	}
	other := func(calls []parity.Call, ok bool) ([]parity.Call, bool) {
		for i := range calls {
			calls[i].Other = true
		}
		return calls, ok
	}
	if _, has := p.ToolInput["command"]; has {
		return other(one(parity.CallShell, "command"))
	}
	for _, k := range fileKeys {
		if _, has := p.ToolInput[k]; has {
			return other(one(parity.CallWrite, k))
		}
	}
	return nil, true
}

// JudgeHermes is Judge for Hermes. Hermes has no hook answer that allows a
// call past its own approvals, so an exact pre-approval gives no opinion,
// and Hermes' own manual approvals decide.
func JudgeHermes(plan DecidePlan, calls []parity.Call, ok bool) parity.CallDecision {
	d := Judge(plan, calls, "", ok)
	if d.Verdict == parity.VerdictAllow {
		d.Verdict = parity.VerdictNone
	}
	return d
}

// HermesOutput is the hook's stdout for d (agent/shell_hooks.py:637-658,
// _parse_pre_tool_call): {"action":"block","message":...} refuses the call;
// {"action":"approve","message":...,"rule_key":...} sends it to Hermes'
// human approval gate, the one its dangerous commands use
// (tools/approval.py:1246-1270); nothing leaves it to Hermes. The rule_key
// is new for every call, so an "always" or "session" answer there covers
// that one call: it never stops a later policy match from asking. The
// message is aggy's fixed text and a group name, never the tool input.
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
