//go:build unix

package e2e

import (
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/fakecli"
)

// TestResumeLatest (T6661): aggy run starts a parent with a pre-assigned
// Claude session; bare aggy then reopens that session with --resume and
// no brief.
func TestResumeLatest(t *testing.T) {
	w := newWorld(t)
	sc := map[string]any{
		"raw_echo": true, "stdout": "READY>",
		"session_files": []map[string]any{{"path": "projects/p/{session_id}.jsonl", "content": "{}"}},
	}
	s := w.start(t, sc, "run", "claude")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy run exit = %d", code)
	}
	first := fakecli.Records(t, w.record)[0]
	id := first.Flags["--session-id"]
	if len(id) != 1 || first.SessionID != id[0] {
		t.Fatalf("first run args = %q, want a pre-assigned --session-id", first.Args)
	}

	s = w.start(t, sc)
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	recs := fakecli.Records(t, w.record)
	second := recs[len(recs)-1]
	if second.Mode != "interactive" || !slices.Equal(second.Args, []string{"--resume", id[0]}) {
		t.Errorf("bare aggy args = %q, want --resume %s and nothing else", second.Args, id[0])
	}
	if second.Env["AGGY_PARENT"] != first.Env["AGGY_PARENT"] || second.Env["AGGY_PARENT"] == "" {
		t.Errorf("AGGY_PARENT %q, then %q: want the same parent", first.Env["AGGY_PARENT"], second.Env["AGGY_PARENT"])
	}
}

// TestResumeGone (T6663): a session the vendor never saved is marked
// gone, and bare aggy says so and starts a new one in the same parent.
func TestResumeGone(t *testing.T) {
	w := newWorld(t)
	sc := map[string]any{"raw_echo": true, "stdout": "READY>"}
	s := w.start(t, sc, "run", "grok")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy run exit = %d", code)
	}
	first := fakecli.Records(t, w.record)[0]

	s = w.start(t, sc)
	s.waitFor([]byte("cannot be resumed"))
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	recs := fakecli.Records(t, w.record)
	second := recs[len(recs)-1]
	if len(second.Flags["--session-id"]) != 1 || second.SessionID == first.SessionID || len(second.Flags["--resume"]) != 0 {
		t.Errorf("second run args = %q, want a new --session-id", second.Args)
	}
	if second.Env["AGGY_PARENT"] != first.Env["AGGY_PARENT"] {
		t.Error("a new child of the same parent was expected")
	}
}
