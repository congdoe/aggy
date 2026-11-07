package codex

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/congdoe/aggy/internal/adapter"
)

// selfTestQuiet are the exec --json item types (exec/src/exec_events.rs#L107
// at 6aaac69) that do not read a file under the review lockdown: the model's
// own words, and web search. Any other type, one new in a later Codex included,
// is a tool the lockdown should have left the child without. file_change, from
// apply_patch, which cannot be turned off (T6638), is judged by its status
// (patchWrote).
var selfTestQuiet = []string{"", "agent_message", "reasoning", "todo_list", "error", "web_search"}

// patchWrote reports whether a file_change event says the patch went through,
// under a read-only sandbox that should have refused it. Statuses are
// PatchApplyStatus (exec_events.rs#L178 at 6aaac69): in_progress, completed,
// failed. Only completed counts, so a status renamed in a later Codex is not a
// final failure for that version. At rust-v0.164.0 a patch the review lockdown
// refuses emits no item at all (verification.md, T6929, question 5). Source only.
func patchWrote(eventType string, status statusField) bool {
	return eventType == "item.completed" && status == "completed"
}

// SelfTestEvidence reads the exec --json stream, which shows each tool call.
// A shell's output is in that stream too, so the borrow's own scan of stdout
// sees a token it printed. Source only; no live run has shown such an item
// under the lockdown.
func (Adapter) SelfTestEvidence(_ adapter.HeadlessRequest, _ adapter.HeadlessResult, out adapter.Output, _ string) adapter.SelfTestEvidence {
	var ev adapter.SelfTestEvidence
	for _, line := range bytes.Split(out.Stdout, []byte("\n")) {
		var e event
		if json.Unmarshal(bytes.TrimSpace(line), &e) != nil || (e.Type != "item.started" && e.Type != "item.completed") {
			continue
		}
		if e.Item.Type == "file_change" && !patchWrote(e.Type, e.Item.Status) {
			continue
		}
		if !slices.Contains(selfTestQuiet, e.Item.Type) && !slices.Contains(ev.Tools, e.Item.Type) {
			ev.Tools = append(ev.Tools, e.Item.Type)
		}
	}
	return ev
}
