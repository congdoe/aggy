package hook

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/congdoe/aggy/internal/adapter"
)

// Hook payload field names are vendor facts. Sources:
//
//   - Claude: code.claude.com/docs/en/hooks.md ("Common input fields";
//     SessionStart, Stop, StopFailure, and Notification input).
//   - Codex 0.164.0: hooks/schema/generated/session-start.command.input.schema.json
//     and stop.command.input.schema.json; the legacy notify payload,
//     hooks/src/legacy_notify.rs#L16 (kebab-case keys, "type").
//   - Grok: grok-build 2bdd1d6a, crates/codegen/xai-grok-pager/docs/user-guide/10-hooks.md
//     (camelCase keys: sessionId, hookEventName, error, errorDetails,
//     reason, notificationType).
//
// docs/verification.md records them.

// keep lists, per vendor, the payload keys aggy forwards to the
// supervisor. Everything else (prompts, assistant text, tool input, and
// the transcript itself) stays in the hook: aggy needs the session, the
// event, and its classification, not the conversation.
var keep = map[adapter.Vendor][]string{
	adapter.Claude: {"session_id", "hook_event_name", "cwd", "transcript_path", "source", "model",
		"permission_mode", "stop_hook_active", "error", "error_details", "notification_type"},
	adapter.Codex: {"session_id", "hook_event_name", "cwd", "transcript_path", "source", "model",
		"permission_mode", "turn_id", "stop_hook_active",
		"type", "thread-id", "turn-id"}, // notify
	adapter.Grok: {"sessionId", "hookEventName", "cwd", "workspaceRoot", "timestamp", "permissionMode",
		"stopHookActive", "error", "errorDetails", "reason", "notificationType"},
}

// maxField drops a kept value longer than this once encoded (an
// error_details string can be long; ids and classes are short).
const maxField = 2 << 10

// Project returns the kept keys of payload, a JSON object, as one
// compact JSON object. A payload that is not a JSON object is an error.
func Project(v adapter.Vendor, payload []byte) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if p := bytes.TrimSpace(payload); len(p) == 0 || p[0] != '{' || json.Unmarshal(p, &m) != nil {
		return nil, errors.New("the hook payload is not a JSON object")
	}
	out := map[string]json.RawMessage{}
	for _, k := range keep[v] {
		if raw, ok := m[k]; ok && len(raw) <= maxField {
			out[k] = raw
		}
	}
	return json.Marshal(out)
}

// Event is what aggy reads from a forwarded hook.
type Event struct {
	// SessionID is the vendor's session id (Codex: the thread id), or "".
	SessionID string
	// Source is how the session started (startup, resume, clear, compact,
	// fork), from Claude's and Codex's SessionStart; "" otherwise.
	Source string
	// Error is StopFailure's classified error, such as rate_limit.
	Error string
	// Reason is Grok's StopCancelled cause, such as user_interrupt.
	Reason string
}

// Parse reads the fields aggy uses from a (projected) payload of v.
// Values that are not strings are ignored.
func Parse(v adapter.Vendor, payload []byte) Event {
	var m map[string]any
	_ = json.Unmarshal(payload, &m)
	s := func(k string) string { x, _ := m[k].(string); return x }
	switch v {
	case adapter.Grok:
		return Event{SessionID: s("sessionId"), Error: s("error"), Reason: s("reason")}
	case adapter.Codex:
		id := s("session_id")
		if id == "" && s("type") == NotifyType {
			id = s("thread-id")
		}
		return Event{SessionID: id, Source: s("source")}
	}
	return Event{SessionID: s("session_id"), Source: s("source"), Error: s("error")}
}

// NotifyType is the type of the Codex legacy notify payload aggy
// handles (legacy_notify.rs#L15).
const NotifyType = "agent-turn-complete"

// Forward reports whether a hook installed for vendor label should be
// forwarded, given the hook's environment.
//
// Only a CLI aggy runs has AGGY_SOCKET and AGGY_VENDOR. Grok also runs
// the hooks in Claude's settings.json (10-hooks.md, "Claude
// compatibility"), so a Claude-labeled hook can fire inside Grok; Grok
// sets GROK_HOOK_EVENT and GROK_SESSION_ID for every hook it runs. Such a
// call is dropped: aggy installs Grok's own hooks for Grok, so each event
// is forwarded once, by the hook installed for the vendor on screen.
func Forward(label adapter.Vendor, getenv func(string) string) bool {
	if getenv("AGGY_SOCKET") == "" {
		return false
	}
	if label != adapter.Grok && (getenv("GROK_HOOK_EVENT") != "" || getenv("GROK_SESSION_ID") != "") {
		return false
	}
	return getenv("AGGY_VENDOR") == string(label)
}
