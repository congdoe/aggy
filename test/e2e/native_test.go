//go:build unix

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/transcript"
)

// Native session ids: sessions the vendor CLIs started by themselves.
const (
	nativeA = "aaaaaaaa-1111-4111-8111-111111111111"
	nativeB = "bbbbbbbb-2222-4222-8222-222222222222"
	nativeC = "cccccccc-3333-4333-8333-333333333333"
)

// nativeClaude writes a Claude session of dir under the world's HOME, at the
// project folder Claude's docs give, with prompt as its first line, changed at.
func nativeClaude(t *testing.T, w *world, dir, id, prompt string, at time.Time) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": prompt}})
	p := filepath.Join(w.home, ".claude", "projects", transcript.ClaudeProjectDir(dir), id+".jsonl")
	writeFileAt(t, p, string(b)+"\n", at)
}

// nativeCodex writes an interactive Codex rollout of dir, changed at.
func nativeCodex(t *testing.T, w *world, dir, id, prompt string, at time.Time) {
	t.Helper()
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "source": "cli", "cwd": dir}})
	msg, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user",
		"content": []map[string]any{{"type": "input_text", "text": prompt}}}})
	p := filepath.Join(w.home, ".codex", "sessions", at.UTC().Format("2006/01/02"),
		"rollout-"+at.UTC().Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	writeFileAt(t, p, string(meta)+"\n"+string(msg)+"\n", at)
}

func writeFileAt(t *testing.T, p, content string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

// lastInteractive is the fake's last on-screen run.
func lastInteractive(t *testing.T, w *world) fakecli.Record {
	t.Helper()
	recs := fakecli.Records(t, w.record)
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Mode == "interactive" {
			return recs[i]
		}
	}
	t.Fatalf("no interactive run: %+v", recs)
	return fakecli.Record{}
}

// runToReady starts aggy with args, waits for the fake, and ends it.
func runToReady(t *testing.T, w *world, sc map[string]any, args ...string) *session {
	t.Helper()
	s := w.start(t, sc, args...)
	s.waitWithin([]byte("READY>"), offerTimeout) // the launch probes the CLI first, slow under load
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy %q exit = %d; screen %q", args, code, s.screen())
	}
	return s
}

// childrenWith counts the index's children that hold vendor session id.
func childrenWith(t *testing.T, w *world, id string) int {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(w.home, ".aggy", "aggy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM children WHERE vendor_session_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestContinueNativeSession (T6965): aggy claude -c continues the Claude
// session started here without aggy, adopts it once, and a second -c reopens
// the same child; a session of another folder is never picked.
func TestContinueNativeSession(t *testing.T) {
	w := newWorld(t)
	nativeClaude(t, w, w.cwd, nativeA, "started with plain claude", time.Now().Add(-time.Hour))
	nativeClaude(t, w, filepath.Join(w.home, "elsewhere"), nativeB, "another folder", time.Now())
	sc := map[string]any{"raw_echo": true, "stdout": "READY>"}
	for i := range 2 {
		s := runToReady(t, w, sc, "claude", "-c")
		r := lastInteractive(t, w)
		if !slices.Equal(r.Flags["--resume"], []string{nativeA}) || len(r.Flags["--session-id"]) != 0 {
			t.Fatalf("run %d args = %q, want --resume %s", i, r.Args, nativeA)
		}
		if adopted := strings.Contains(s.screen(), "is aggy's session"); adopted != (i == 0) {
			t.Errorf("run %d: adoption note %v, want %v", i, adopted, i == 0)
		}
	}
	if n := childrenWith(t, w, nativeA); n != 1 {
		t.Errorf("%d children hold the session, want 1", n)
	}
	if n := childrenWith(t, w, nativeB); n != 0 {
		t.Errorf("another folder's session was adopted")
	}
}

// TestContinueNativeCodex (T6965): aggy codex --continue finds Codex's own rollout of
// this folder by its session_meta.
func TestContinueNativeCodex(t *testing.T) {
	w := newWorld(t)
	nativeCodex(t, w, w.cwd, nativeA, "codex by itself", time.Now().Add(-time.Minute))
	nativeCodex(t, w, filepath.Join(w.home, "elsewhere"), nativeB, "elsewhere", time.Now())
	runToReady(t, w, map[string]any{"raw_echo": true, "stdout": "READY>"}, "codex", "--continue")
	r := lastInteractive(t, w)
	if i := slices.Index(r.Args, "resume"); i < 0 || !slices.Contains(r.Args[i:], nativeA) || slices.Contains(r.Args, nativeB) {
		t.Errorf("args = %q, want resume %s", r.Args, nativeA)
	}
}

// TestPickSession (T6965): aggy -r lists aggy's session and the vendors' own
// here, newest first, escapes a first prompt's control characters, leaves out
// another folder's, and opens the one picked.
func TestPickSession(t *testing.T) {
	w := newWorld(t)
	sc := map[string]any{"raw_echo": true, "stdout": "READY>",
		"session_files": []map[string]any{{"path": "projects/" + transcript.ClaudeProjectDir(w.cwd) + "/{session_id}.jsonl",
			"content": `{"type":"user","message":{"content":"aggy started this"}}` + "\n"}}}
	runToReady(t, w, sc, "run", "claude")
	aggyID := lastInteractive(t, w).SessionID
	nativeClaude(t, w, w.cwd, nativeA, "\x1b[2Jcleared\x07 and more", time.Now().Add(-2*time.Hour))
	nativeCodex(t, w, w.cwd, nativeB, "codex on its own", time.Now().Add(-3*time.Hour))
	nativeClaude(t, w, filepath.Join(w.home, "elsewhere"), nativeC, "another folder", time.Now())

	s := w.start(t, sc, "-r")
	s.waitWithin([]byte("Pick 1-3"), offerTimeout)
	list := s.screen()
	for _, want := range []string{"aggy started this", `\x1b[2Jcleared\x07 and more`, "codex on its own", "claude's own", "codex's own"} {
		if !strings.Contains(list, want) {
			t.Errorf("list %q lacks %q", list, want)
		}
	}
	if strings.Contains(list, "another folder") || strings.Contains(list, "\x1b[2J") {
		t.Errorf("list %q shows another folder's session or a raw escape", list)
	}
	if i, j := strings.Index(list, "aggy started this"), strings.Index(list, "codex on its own"); j < i {
		t.Errorf("list %q is not newest first", list)
	}
	s.send([]byte("3\r"))
	s.waitWithin([]byte("READY>"), offerTimeout) // the launch probes the CLI first, slow under load
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	r := lastInteractive(t, w)
	if r.Vendor != "codex" || !slices.Contains(r.Args, nativeB) {
		t.Errorf("picked run = %s %q, want codex resume %s", r.Vendor, r.Args, nativeB)
	}
	if aggyID == "" || childrenWith(t, w, aggyID) != 1 {
		t.Errorf("aggy's own session %q is not recorded once", aggyID)
	}
}

// TestResumeNativeIDAndName (T6965): --resume takes a vendor session id found
// here, and, for Claude, a name, which Claude resolves; aggy records the session
// that changed in the run, so -c reopens it.
func TestResumeNativeIDAndName(t *testing.T) {
	w := newWorld(t)
	nativeClaude(t, w, w.cwd, nativeA, "by id", time.Now().Add(-time.Hour))
	sc := map[string]any{"raw_echo": true, "stdout": "READY>"}
	runToReady(t, w, sc, "claude", "--resume", nativeA)
	if r := lastInteractive(t, w); !slices.Equal(r.Flags["--resume"], []string{nativeA}) {
		t.Fatalf("args = %q, want --resume %s", r.Args, nativeA)
	}

	// The fake, given a name, "finds" nativeB and writes to its file.
	named := map[string]any{"raw_echo": true, "stdout": "READY>",
		"session_files": []map[string]any{{"path": "projects/" + transcript.ClaudeProjectDir(w.cwd) + "/" + nativeB + ".jsonl",
			"content": `{"type":"user","message":{"content":"named"}}` + "\n"}}}
	s := runToReady(t, w, named, "claude", "--resume", "auth-refactor")
	if r := lastInteractive(t, w); !slices.Equal(r.Flags["--resume"], []string{"auth-refactor"}) || len(r.Flags["--session-id"]) != 0 {
		t.Fatalf("args = %q, want --resume auth-refactor and no session id", r.Args)
	}
	if !strings.Contains(s.screen(), "records which one when it exits") {
		t.Errorf("no note about the name: %q", s.screen())
	}
	if n := childrenWith(t, w, nativeB); n != 1 {
		t.Fatalf("%d children hold the named session, want 1", n)
	}
	runToReady(t, w, sc, "claude", "-c")
	if r := lastInteractive(t, w); !slices.Equal(r.Flags["--resume"], []string{nativeB}) {
		t.Errorf("-c args = %q, want --resume %s", r.Args, nativeB)
	}
}

// TestResumeNativeRefused (T6965): a value that could be a path or a flag never
// reaches the CLI, a Codex name is not passed, and an id of no session here is
// not found.
func TestResumeNativeRefused(t *testing.T) {
	tests := []struct {
		args []string
		code string
	}{
		{[]string{"claude", "--resume=/tmp/x.jsonl"}, "cli.usage"},
		{[]string{"claude", "--resume", "a\\b"}, "cli.usage"},
		{[]string{"codex", "--resume", "auth-refactor"}, "cli.not_found"},
		{[]string{"claude", "--resume", nativeC}, "cli.not_found"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			w := newWorld(t)
			s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, tt.args...)
			s.waitWithin([]byte(tt.code), offerTimeout)
			if code := s.exit(); code == 0 {
				t.Errorf("exit 0, want a refusal")
			}
			for _, r := range fakecli.Records(t, w.record) {
				if r.Mode == "interactive" {
					t.Errorf("the CLI started: %q", r.Args)
				}
			}
		})
	}
}
