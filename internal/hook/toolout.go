package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxToolOutput caps what a managed tool's hook may print. More is
// refused whole (FilterToolOutput).
const MaxToolOutput = 64 << 10

// ErrToolOutput is a tool hook's output that FilterToolOutput refuses:
// too big, not one JSON object, or not for a PreToolUse hook.
var ErrToolOutput = errors.New("tool hook output refused")

// MaxToolReason caps a hook's permissionDecisionReason (L78-6).
const MaxToolReason = 1 << 10

// Names in FilterToolOutput's dropped list: fixed text only, never a key
// or value the hook wrote (L78-5).
const (
	DroppedAllow       = "permissionDecision_allow"
	DroppedDecision    = "permissionDecision_other"
	DroppedReason      = "permissionDecisionReason_invalid"
	DroppedRewrite     = "updatedInput_undeclared"
	DroppedInputShape  = "updatedInput_not_object"
	DroppedRewriteForm = "updatedInput_unexpected"
	DroppedOther       = "other"
	droppedInnerPrefix = "hookSpecificOutput."
)

// knownTop and knownInner are Claude's hook output keys
// (code.claude.com/docs hooks.md, "JSON output" and "PreToolUse decision
// control"): a dropped one is recorded by its name, any other as
// DroppedOther.
var (
	knownTop   = []string{"continue", "stopReason", "suppressOutput", "systemMessage", "decision", "reason"}
	knownInner = []string{"additionalContext"}
)

// FilterToolOutput returns the part of a managed tool's Claude hook
// output aggy passes on, for event (aggy hook-tool, T6708). Only a
// PreToolUse hook's answer passes, and only these keys of its
// hookSpecificOutput:
//
//   - hookEventName;
//   - permissionDecision when it is "deny" or "ask": an "allow" is
//     dropped, so a tool's hook can never approve an action the person
//     would have been asked about;
//   - permissionDecisionReason, a string of at most MaxToolReason bytes.
//     Claude shows a deny's reason to the model, so this text does reach
//     the model's context (L78-6);
//   - updatedInput, an object, and only when prefix is set (the hook's
//     definition declares rewrites = true and its rewrite_prefix,
//     H78-1), and only in the one form rewritten checks against
//     payload, the hook's own input: a Bash call whose command is prefix
//     plus the original command and whose other keys are the original's
//     (R80-1). Any other rewrite could step around both the deny rules
//     matched against the original and their prefixed copies
//     (parity.RewriteDeny), which match only prefix + a denied command.
//
// Every other key (continue, stopReason, systemMessage,
// additionalContext, decision, suppressOutput, ...) is dropped, so a
// hook cannot stop the session or add other text to the model's context.
// Claude's hook output format: code.claude.com/docs hooks.md,
// "PreToolUse decision control".
//
// Empty output is nil and no error. Output over MaxToolOutput, output
// that is not one JSON object, and output of another event are refused
// (nil and ErrToolOutput): the caller prints nothing, which leaves the
// tool call as the CLI would have handled it without the hook. dropped
// names what was left out, in the fixed names above, each once.
func FilterToolOutput(event string, out, payload []byte, prefix string) (filtered []byte, dropped []string, err error) {
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
	drop := func(name string) {
		if !slices.Contains(dropped, name) {
			dropped = append(dropped, name)
		}
	}
	named := func(prefix, k string, known []string) string {
		if slices.Contains(known, k) {
			return prefix + k
		}
		return DroppedOther
	}
	var hso map[string]json.RawMessage
	for k, v := range top {
		if k != "hookSpecificOutput" {
			drop(named("", k, knownTop))
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
			switch s {
			case "deny", "ask":
				keep[k] = v
			case "allow":
				drop(DroppedAllow)
			default:
				drop(DroppedDecision)
			}
		case "permissionDecisionReason":
			var s string
			if json.Unmarshal(v, &s) != nil || len(s) > MaxToolReason {
				drop(DroppedReason)
				continue
			}
			keep[k] = v
		case "updatedInput":
			var obj map[string]json.RawMessage
			switch {
			case prefix == "":
				drop(DroppedRewrite)
			case json.Unmarshal(v, &obj) != nil || obj == nil:
				drop(DroppedInputShape)
			default:
				if in, ok := rewritten(obj, payload, prefix); ok {
					keep[k] = in
				} else {
					drop(DroppedRewriteForm)
				}
			}
		default:
			drop(named(droppedInnerPrefix, k, knownInner))
		}
	}
	if len(keep) == 0 || len(keep) == 1 && keep["hookEventName"] != nil {
		return nil, dropped, nil
	}
	keep["hookEventName"] = json.RawMessage(`"PreToolUse"`)
	b, err := json.Marshal(map[string]any{"hookSpecificOutput": keep})
	return b, dropped, err
}

// rewritten checks a rewriting hook's updatedInput, upd, against the
// call it rewrites, as Claude's hook input payload gives it (tool_name
// and tool_input; code.claude.com/docs hooks.md, "PreToolUse input"),
// and returns the input to pass on, built from the original's own values
// (R80-1). It is accepted only when the call is Bash, every key of upd
// is a key of the original tool_input, each one but command is
// deep-equal to the original's, and command is exactly prefix + the
// original command. Then whichever input Claude matches its rules
// against, the policy's rules or their prefixed copies
// (parity.RewriteDeny) cover it, and no other key
// (dangerouslyDisableSandbox, ...) changes.
//
// TODO(T6813): a per-segment rewrite ("a && b" to "rtk a && rtk b") is
// refused: accepting one needs a tokenizer that splits a command as
// Claude does, and deny rules shown to cover each segment.
func rewritten(upd map[string]json.RawMessage, payload []byte, prefix string) (json.RawMessage, bool) {
	var top, orig map[string]json.RawMessage
	var tool, cmd, newCmd string
	if json.Unmarshal(payload, &top) != nil || json.Unmarshal(top["tool_name"], &tool) != nil || tool != "Bash" ||
		json.Unmarshal(top["tool_input"], &orig) != nil ||
		json.Unmarshal(orig["command"], &cmd) != nil || json.Unmarshal(upd["command"], &newCmd) != nil {
		return nil, false
	}
	// Invalid UTF-8 or a lone surrogate decodes to U+FFFD, so the command
	// passed on would not be the one Claude's rules saw. Leading space
	// would sit between prefix and command, where a prefixed rule such as
	// Bash(rtk git push --force*) may not match it (M82-1).
	if strings.TrimSpace(cmd) == "" || cmd != strings.TrimLeftFunc(cmd, unicode.IsSpace) ||
		strings.ContainsRune(cmd, utf8.RuneError) || newCmd != prefix+cmd {
		return nil, false
	}
	out := make(map[string]json.RawMessage, len(upd))
	for k, v := range upd {
		o, ok := orig[k]
		if !ok {
			return nil, false
		}
		if k != "command" {
			var a, b any
			if json.Unmarshal(v, &a) != nil || json.Unmarshal(o, &b) != nil || !reflect.DeepEqual(a, b) {
				return nil, false
			}
		}
		out[k] = o
	}
	c, err := json.Marshal(newCmd)
	if err != nil {
		return nil, false
	}
	out["command"] = c
	b, err := json.Marshal(out)
	return b, err == nil
}
