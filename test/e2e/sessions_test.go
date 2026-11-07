//go:build unix

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/fakecli"
)

// TestResumeLatest (T6661): aggy run starts a parent with a pre-assigned
// Claude session; aggy --continue then reopens that session with --resume
// and no brief.
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

	s = w.start(t, sc, "--continue")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	recs := fakecli.Records(t, w.record)
	second := recs[len(recs)-1]
	// The safety policy rides along as --settings; nothing else, no brief.
	if second.Mode != "interactive" || len(second.Args) != 4 || !slices.Equal(second.Args[:3], []string{"--resume", id[0], "--settings"}) {
		t.Errorf("aggy --continue args = %q, want --resume %s, the policy, and nothing else", second.Args, id[0])
	}
	if second.Env["AGGY_PARENT"] != first.Env["AGGY_PARENT"] || second.Env["AGGY_PARENT"] == "" {
		t.Errorf("AGGY_PARENT %q, then %q: want the same parent", first.Env["AGGY_PARENT"], second.Env["AGGY_PARENT"])
	}
}

// TestBareStartsFresh (T6903): bare aggy starts a new session with a new
// parent, like claude, even with a session here it could reopen.
func TestBareStartsFresh(t *testing.T) {
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

	s = w.start(t, sc)
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	recs := fakecli.Records(t, w.record)
	second := recs[len(recs)-1]
	if len(second.Flags["--resume"]) != 0 || len(second.Flags["--session-id"]) != 1 || second.SessionID == first.SessionID {
		t.Errorf("bare aggy args = %q, want a new --session-id and no --resume", second.Args)
	}
	if second.Env["AGGY_PARENT"] == first.Env["AGGY_PARENT"] {
		t.Error("bare aggy reused the parent; want a new one")
	}
}

// TestVendorFlags (T6903): aggy claude hands the CLI its own flags, keeps
// aggy's session id and policy, puts the model on the session, and
// --continue takes the session back with the flags given then.
func TestVendorFlags(t *testing.T) {
	w := newWorld(t)
	sc := map[string]any{
		"raw_echo": true, "stdout": "READY>",
		"session_files": []map[string]any{{"path": "projects/p/{session_id}.jsonl", "content": "{}"}},
	}
	s := w.start(t, sc, "claude", "--model", "opus-x", "--permission-mode", "plan")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy claude exit = %d", code)
	}
	first := fakecli.Records(t, w.record)[0]
	if !slices.Equal(first.Flags["--model"], []string{"opus-x"}) || !slices.Equal(first.Flags["--permission-mode"], []string{"plan"}) ||
		len(first.Flags["--session-id"]) != 1 || len(first.Flags["--settings"]) != 1 {
		t.Errorf("args = %q, want the model, the mode, aggy's session id, and the policy", first.Args)
	}
	if first.Env["AGGY_MODEL"] != "opus-x" {
		t.Errorf("AGGY_MODEL = %q", first.Env["AGGY_MODEL"])
	}

	s = w.start(t, sc, "claude", "-c", "--permission-mode", "plan")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy claude -c exit = %d", code)
	}
	recs := fakecli.Records(t, w.record)
	second := recs[len(recs)-1]
	if !slices.Equal(second.Flags["--resume"], first.Flags["--session-id"]) || !slices.Equal(second.Flags["--permission-mode"], []string{"plan"}) {
		t.Errorf("aggy claude -c args = %q, want --resume of the first and the mode", second.Args)
	}
}

// TestResumeGone (T6663): a session the vendor never saved is marked
// gone, and aggy resume says so and starts a new one in the same parent.
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
	// Grok's sessions folder is there and holds nothing for the session.
	if err := os.MkdirAll(filepath.Join(w.home, ".grok", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}

	s = w.start(t, sc, "resume")
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
