package brief

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// T6671: a return brief holds only the turns other CLIs added since the
// child was last closed.
func TestSince(t *testing.T) {
	left := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	others := []Other{
		{Vendor: adapter.Codex, Transcript: adapter.Transcript{Path: "/c.jsonl", Turns: []adapter.Turn{
			{Role: "user", Text: "OLD codex ask", Time: left.Add(-time.Minute)},
			{Role: "user", Text: "AT close", Time: left},
			{Role: "assistant", Text: "NEW codex answer", Time: left.Add(time.Minute)},
		}}},
		{Vendor: adapter.Claude, Err: errors.New("format changed")},
		{Vendor: adapter.Grok, Transcript: adapter.Transcript{Path: "/g.jsonl", Turns: []adapter.Turn{
			{Role: "assistant", Text: "undated grok turn"},
		}}},
	}
	tr, err := Since(left, others)
	if err == nil || !strings.Contains(err.Error(), "claude: format changed") {
		t.Errorf("err = %v", err)
	}
	var got []string
	for _, turn := range tr.Turns {
		got = append(got, turn.Role+": "+turn.Text)
	}
	want := "codex assistant: NEW codex answer|grok assistant: undated grok turn"
	if strings.Join(got, "|") != want {
		t.Errorf("turns = %q, want %q", got, want)
	}
	if tr.Path != "/c.jsonl\n/g.jsonl" {
		t.Errorf("path = %q", tr.Path)
	}

	in := Input{From: adapter.Grok, To: adapter.Claude, Since: left, Task: "goal: ship", Transcript: tr, TranscriptErr: err}
	out := Build(in)
	for _, s := range []string{"# Return brief", "## Turns since you left", "NEW codex answer", "### grok assistant",
		"format changed", "goal: ship", "/g.jsonl"} {
		if !strings.Contains(out, s) {
			t.Errorf("return brief lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "OLD codex ask") || strings.Contains(out, "AT close") {
		t.Error("return brief holds turns from before the child was closed")
	}
	in.Transcript, in.TranscriptErr = adapter.Transcript{}, nil
	if out := Build(in); !strings.Contains(out, "No other CLI added a turn") {
		t.Errorf("empty return brief:\n%s", out)
	}
}
