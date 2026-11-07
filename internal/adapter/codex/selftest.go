package codex

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/congdoe/aggy/internal/adapter"
)

// selfTestQuiet are the exec --json item types (exec/src/exec_events.rs#L107
// at 6aaac69) that do not read a file under the review lockdown: the model's
// own words, a failed apply_patch (which cannot be turned off, T6638), and web
// search. Any other type, one new in a later Codex included, is a tool the
// lockdown should have left the child without.
var selfTestQuiet = []string{"", "agent_message", "reasoning", "todo_list", "error", "file_change", "web_search"}

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
		if !slices.Contains(selfTestQuiet, e.Item.Type) && !slices.Contains(ev.Tools, e.Item.Type) {
			ev.Tools = append(ev.Tools, e.Item.Type)
		}
	}
	return ev
}
