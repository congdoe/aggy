package grok

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// maxHistory caps the session history a self-test reads: its run is one turn.
const maxHistory = 16 << 20

// SelfTestEvidence reads the session's chat_history.jsonl: --output-format
// json has no tool calls (verification.md G6), but this file holds the
// messages sent to the model, a tool's result among them, as assistant lines
// with tool_calls and tool_result lines (grok-build 2bdd1d6a, verification.md).
// So a token in it was read, whatever the reply says, and a locked-down tool
// named in tool_calls was tried. Source only, and a missing file shows nothing:
// the reply then decides. tool_calls' inner shape is not recorded, so a name
// counts wherever it appears as a JSON string there.
func (a Adapter) SelfTestEvidence(req adapter.HeadlessRequest, res adapter.HeadlessResult, _ adapter.Output, token string) adapter.SelfTestEvidence {
	var ev adapter.SelfTestEvidence
	if adapter.ValidSessionID(adapter.Grok, res.SessionID) != nil || !filepath.IsAbs(a.Home()) {
		return ev
	}
	data, err := safefs.ReadFileLimit(filepath.Join(a.Home(), "sessions", EncodeCwd(req.Cwd), res.SessionID, "chat_history.jsonl"), maxHistory)
	if err != nil {
		return ev
	}
	ev.Read = bytes.Contains(data, []byte(token))
	for _, line := range bytes.Split(data, []byte("\n")) {
		var e struct {
			Type      string          `json:"type"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" {
			continue
		}
		for _, t := range Lockdown {
			if !slices.Contains(SearchTools, t) && !slices.Contains(ev.Tools, t) && bytes.Contains(e.ToolCalls, []byte(`"`+t+`"`)) {
				ev.Tools = append(ev.Tools, t)
			}
		}
	}
	return ev
}
