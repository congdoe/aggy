package grok

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
// named in tool_calls was tried. Source only. A missing file shows nothing, and
// the reply then decides (NoRecord); one that cannot be read whole, over the
// cap for a one-turn run or otherwise, is Unreadable, which is not a pass.
// tool_calls' inner shape is not recorded, so a name counts wherever it appears
// as a JSON string there.
func (a Adapter) SelfTestEvidence(req adapter.HeadlessRequest, res adapter.HeadlessResult, _ adapter.Output, token string) adapter.SelfTestEvidence {
	var ev adapter.SelfTestEvidence
	if adapter.ValidSessionID(adapter.Grok, res.SessionID) != nil || !filepath.IsAbs(a.Home()) {
		ev.NoRecord = true
		return ev
	}
	data, err := a.history(req.Cwd, res.SessionID)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		ev.NoRecord = true
		return ev
	case err != nil:
		ev.Unreadable = true
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

// history reads the session's chat_history.jsonl: under the cwd's encoded name,
// else the one session of that id under any other, since the encoding is
// unconfirmed and the CLI may see the cwd with symlinks resolved (a macOS temp
// dir), as for Artifacts. It matches fs.ErrNotExist when there is none. Several
// are an error, not a guess.
func (a Adapter) history(cwd, id string) ([]byte, error) {
	sessions := filepath.Join(a.Home(), "sessions")
	file := func(dir string) string { return filepath.Join(sessions, dir, id, "chat_history.jsonl") }
	data, err := safefs.ReadFileLimit(file(EncodeCwd(cwd)), maxHistory)
	if !errors.Is(err, fs.ErrNotExist) {
		return data, err
	}
	dirs, err := adapter.SubDirs(sessions)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, d := range dirs {
		if _, err := safefs.Lstat(file(d)); err == nil {
			found = append(found, file(d))
		}
	}
	switch len(found) {
	case 0:
		return nil, fs.ErrNotExist
	case 1:
		return safefs.ReadFileLimit(found[0], maxHistory)
	}
	return nil, fmt.Errorf("grok: session %s has a history under %d working dirs", id, len(found))
}
