package hook

import (
	"bytes"
	"encoding/json"
	"errors"
)

// MaxToolOutput caps what a managed tool's hook may print. More is
// refused whole (FilterToolOutput).
const MaxToolOutput = 64 << 10

// ErrToolOutput is a tool hook's output that FilterToolOutput refuses:
// too big, not one JSON object, or not for a PreToolUse hook.
var ErrToolOutput = errors.New("tool hook output refused")

// FilterToolOutput returns the part of a managed tool's Claude hook
// output aggy passes on, for event (aggy hook-tool, T6708). Only a
// PreToolUse hook's answer passes, and only these keys of its
// hookSpecificOutput: hookEventName, permissionDecision when it is
// "deny" or "ask", permissionDecisionReason, and updatedInput. A
// permissionDecision of "allow" is dropped, so a tool's hook can never
// approve an action the person would have been asked about; every other
// key (continue, stopReason, systemMessage, additionalContext, decision,
// suppressOutput, ...) is dropped too, so a hook cannot stop the session
// or add text to the model's context. Claude's hook output format:
// code.claude.com/docs hooks.md, "PreToolUse decision control".
//
// Empty output is nil and no error. Output over MaxToolOutput, output
// that is not one JSON object, and output of another event are refused
// (nil and ErrToolOutput): the caller prints nothing, which leaves the
// tool call as the CLI would have handled it without the hook. dropped
// names the keys left out.
func FilterToolOutput(event string, out []byte) (filtered []byte, dropped []string, err error) {
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil, nil
	}
	if len(out) > MaxToolOutput || event != "PreToolUse" {
		return nil, nil, ErrToolOutput
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	var top map[string]json.RawMessage
	if err := dec.Decode(&top); err != nil || top == nil || dec.More() {
		return nil, nil, ErrToolOutput
	}
	var hso map[string]json.RawMessage
	for k, v := range top {
		if k != "hookSpecificOutput" {
			dropped = append(dropped, k)
			continue
		}
		if err := json.Unmarshal(v, &hso); err != nil || hso == nil {
			return nil, nil, ErrToolOutput
		}
	}
	keep := map[string]json.RawMessage{}
	for k, v := range hso {
		switch k {
		case "hookEventName":
			var s string
			if json.Unmarshal(v, &s) != nil || s != "PreToolUse" {
				return nil, nil, ErrToolOutput
			}
			keep[k] = v
		case "permissionDecision":
			var s string
			if json.Unmarshal(v, &s) != nil {
				return nil, nil, ErrToolOutput
			}
			if s != "deny" && s != "ask" {
				dropped = append(dropped, k+"="+s)
				continue
			}
			keep[k] = v
		case "permissionDecisionReason", "updatedInput":
			keep[k] = v
		default:
			dropped = append(dropped, "hookSpecificOutput."+k)
		}
	}
	if len(keep) == 0 || len(keep) == 1 && keep["hookEventName"] != nil {
		return nil, dropped, nil
	}
	keep["hookEventName"] = json.RawMessage(`"PreToolUse"`)
	b, err := json.Marshal(map[string]any{"hookSpecificOutput": keep})
	return b, dropped, err
}
