package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/safefs"
)

const id = "00000000-0000-4000-8000-000000000001"

// The lines below follow the shapes cited in transcript.go; no session
// file has been recorded live yet (see docs/adapters.md).
var (
	claudeLines = strings.Join([]string{
		`{"type":"user","isMeta":true,"message":{"content":"<local-command>"}}`,
		`{"type":"user","cwd":"/w","message":{"role":"user","content":"fix the bug"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"x"},{"type":"text","text":"done"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"out"}]}}`,
		`{"type":"summary","summary":"t"}`,
		`{"type":"user","isSidechain":true,"message":{"content":"side"}}`,
		`{"type":"user","message":{"content":[{"type":"text","text":"now test it"}]}}`,
	}, "\n") + "\n"
	codexLines = strings.Join([]string{
		`{"timestamp":"2026-10-27T10:00:00.000Z","type":"session_meta","payload":{}}`,
		`{"timestamp":"2026-10-27T10:00:01.000Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"sys"}]}}`,
		`{"timestamp":"2026-10-27T10:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]}}`,
		`{"timestamp":"2026-10-27T10:00:03.000Z","type":"response_item","payload":{"type":"function_call","name":"exec"}}`,
		`{"timestamp":"2026-10-27T10:00:04.000Z","type":"event_msg","payload":{"type":"agent_message","message":"dup"}}`,
		`{"timestamp":"2026-10-27T10:00:05.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
		`{"timestamp":"2026-10-27T10:00:06.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"now test it"}]}}`,
	}, "\n") + "\n"
	grokLines = strings.Join([]string{
		`{"type":"system","content":"sys"}`,
		`{"type":"user","content":[{"type":"text","text":"fix the bug"}]}`,
		`{"type":"assistant","content":"","tool_calls":[{"id":"1"}]}`,
		`{"type":"tool_result","tool_call_id":"1","content":"out"}`,
		`{"type":"assistant","content":"done"}`,
		`{"type":"user","content":[{"type":"text","text":"reminder"}],"synthetic_reason":"system_reminder"}`,
		`{"type":"user","content":[{"type":"text","text":"now test it"}]}`,
	}, "\n") + "\n"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func texts(ts []adapter.Turn) string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Role+":"+t.Text)
	}
	return strings.Join(out, "|")
}

func TestTail(t *testing.T) {
	home := t.TempDir()
	cwd := "/work/my repo"
	write(t, filepath.Join(home, "claude", "projects", ClaudeProjectDir(cwd), id+".jsonl"), claudeLines)
	write(t, filepath.Join(home, "codex", "sessions", "2026", "10", "27", "rollout-2026-10-27T10-00-00-"+id+".jsonl"), codexLines)
	write(t, filepath.Join(home, "grok", "sessions", grok.EncodeCwd(cwd), id, "chat_history.jsonl"), grokLines)
	want := "user:fix the bug|assistant:done|user:now test it"
	for _, tc := range []struct {
		v   adapter.Vendor
		cwd string
	}{
		{adapter.Claude, cwd}, {adapter.Codex, cwd}, {adapter.Grok, cwd},
		// Without a cwd, or with one the folder does not match, the scan finds it.
		{adapter.Claude, ""}, {adapter.Grok, "/elsewhere"},
	} {
		tr, err := Tail(tc.v, filepath.Join(home, string(tc.v)), adapter.SessionRef{ID: id, Cwd: tc.cwd}, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.v, err)
		}
		if got := texts(tr.Turns); got != want {
			t.Errorf("%s turns = %q, want %q", tc.v, got, want)
		}
		if !strings.HasPrefix(tr.Path, home) {
			t.Errorf("%s path = %q", tc.v, tr.Path)
		}
		tr, err = Tail(tc.v, filepath.Join(home, string(tc.v)), adapter.SessionRef{ID: id, Cwd: tc.cwd}, 1)
		if err != nil || texts(tr.Turns) != "user:now test it" {
			t.Errorf("%s n=1: %q, %v", tc.v, texts(tr.Turns), err)
		}
	}
}

func TestTailErrors(t *testing.T) {
	home := t.TempDir()
	other := t.TempDir()
	write(t, filepath.Join(other, "secret.jsonl"), claudeLines)
	write(t, filepath.Join(home, "projects", "d", "x"), "")
	// A symlinked session file, and a symlinked project folder.
	if err := os.Symlink(filepath.Join(other, "secret.jsonl"), filepath.Join(home, "projects", "d", id+".jsonl")); err != nil {
		t.Fatal(err)
	}
	gh := t.TempDir()
	write(t, filepath.Join(other, id, "chat_history.jsonl"), grokLines)
	write(t, filepath.Join(gh, "sessions", "x"), "")
	if err := os.Symlink(other, filepath.Join(gh, "sessions", "enc")); err != nil {
		t.Fatal(err)
	}
	// A file in a format no reader knows.
	ch := t.TempDir()
	write(t, filepath.Join(ch, "sessions", "2026", "01", "02", "rollout-x-"+id+".jsonl"), "not json\n")
	// Two files for one id.
	dup := t.TempDir()
	write(t, filepath.Join(dup, "projects", "a", id+".jsonl"), claudeLines)
	write(t, filepath.Join(dup, "projects", "b", id+".jsonl"), claudeLines)

	for _, tc := range []struct {
		name string
		v    adapter.Vendor
		home string
		ref  adapter.SessionRef
		want error
	}{
		{"bad id", adapter.Claude, home, adapter.SessionRef{ID: "--resume"}, nil},
		{"missing", adapter.Codex, t.TempDir(), adapter.SessionRef{ID: id}, adapter.ErrNoTranscript},
		{"symlinked file", adapter.Claude, home, adapter.SessionRef{ID: id, Cwd: "d"}, adapter.ErrNoTranscript},
		{"symlinked folder", adapter.Grok, gh, adapter.SessionRef{ID: id, Cwd: "enc"}, ErrUnsafe},
		{"unknown format", adapter.Codex, ch, adapter.SessionRef{ID: id}, adapter.ErrNoTranscript},
		{"ambiguous", adapter.Claude, dup, adapter.SessionRef{ID: id}, nil},
	} {
		_, err := Tail(tc.v, tc.home, tc.ref, 0)
		if err == nil {
			t.Errorf("%s: no error", tc.name)
			continue
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}

// readTail itself refuses a symlink anywhere under the session folder,
// even when a caller hands it the path directly.
func TestReadTailRefusesLinks(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	write(t, filepath.Join(other, "f.jsonl"), "{}\n")
	if err := os.Symlink(other, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "f.jsonl"), filepath.Join(root, "f.jsonl")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(root, "link", "f.jsonl"), filepath.Join(root, "f.jsonl"), filepath.Join(other, "f.jsonl"), root} {
		if _, err := readTail(root, p); err == nil {
			t.Errorf("readTail(%s) read through a link or outside the root", p)
		}
	}
}

// A credential file is refused by safefs even under a session folder.
func TestReadTailRefusesCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, ".codex", "auth.json")
	write(t, p, `{"tokens":{}}`)
	if _, err := readTail(filepath.Join(home, ".codex"), p); err == nil {
		t.Fatal("read a credential file")
	}
	var ae *safefs.AuthPathError
	_, err := readTail(filepath.Join(home, ".codex"), p)
	if !errors.As(err, &ae) {
		t.Errorf("err = %v, want an AuthPathError", err)
	}
}

func TestReadTailBounded(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "f.jsonl")
	line := `{"type":"assistant","content":"` + strings.Repeat("a", 1000) + `"}` + "\n"
	write(t, p, strings.Repeat(line, MaxTail/len(line)+50))
	data, err := readTail(root, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > MaxTail || !strings.HasPrefix(string(data), `{"type"`) {
		t.Errorf("tail is %d bytes starting %q", len(data), string(data[:10]))
	}
}

func TestHome(t *testing.T) {
	env := map[string]string{"HOME": "/h", "CODEX_HOME": "/c"}
	get := func(k string) string { return env[k] }
	for v, want := range map[adapter.Vendor]string{adapter.Claude: "/h/.claude", adapter.Codex: "/c", adapter.Grok: "/h/.grok"} {
		if got, err := Home(v, get); err != nil || got != want {
			t.Errorf("Home(%s) = %q, %v", v, got, err)
		}
	}
	if _, err := Home(adapter.Grok, func(string) string { return "rel" }); err == nil {
		t.Error("relative home accepted")
	}
}

// C16-2: context the CLI injected as a user message never reaches a
// brief; of a user shell command only the command line is kept.
func TestSkipsInjectedContext(t *testing.T) {
	codex := func(text string) string {
		return `{"timestamp":"2026-10-27T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":` + strconv.Quote(text) + `}]}}`
	}
	claude := func(text string) string {
		return `{"type":"user","message":{"content":` + strconv.Quote(text) + `}}`
	}
	for _, tc := range []struct {
		v    adapter.Vendor
		line string
		want string
	}{
		{adapter.Codex, codex("<user_shell_command>\n<command>\ncat .env\n</command>\n<result>\nExit code: 0\nDuration: 0.1 seconds\nOutput:\nSECRET_OUTPUT\n</result>\n</user_shell_command>"), "! cat .env"},
		{adapter.Codex, codex("# AGENTS.md instructions for /w\n\n<INSTRUCTIONS>\nSECRET_OUTPUT\n</INSTRUCTIONS>"), ""},
		{adapter.Codex, codex("<environment_context>\n  <cwd>/w</cwd>\n</environment_context>"), ""},
		{adapter.Codex, codex("<turn_aborted>\nThe user interrupted\n</turn_aborted>"), ""},
		{adapter.Codex, codex("fix the bug"), "fix the bug"},
		{adapter.Claude, claude("<bash-input>cat .env</bash-input>"), "! cat .env"},
		{adapter.Claude, claude("<bash-stdout>SECRET_OUTPUT</bash-stdout><bash-stderr></bash-stderr>"), ""},
		{adapter.Claude, claude("<local-command-stdout>SECRET_OUTPUT</local-command-stdout>"), ""},
		{adapter.Claude, claude("<command-name>/cost</command-name>\n<command-message>cost</command-message>\n<command-args></command-args>"), ""},
		{adapter.Claude, claude("[Request interrupted by user]"), ""},
		{adapter.Claude, claude("fix the bug"), "fix the bug"},
	} {
		turns, ok := parse(tc.v, []byte(tc.line+"\n"))
		got := texts(turns)
		if tc.want != "" {
			if got != "user:"+tc.want {
				t.Errorf("%s %q: got %q", tc.v, tc.line, got)
			}
		} else if !ok || got != "" {
			t.Errorf("%s %q: got %q, %v", tc.v, tc.line, got, ok)
		}
		if strings.Contains(got, "SECRET_OUTPUT") {
			t.Errorf("%s: output leaked", tc.v)
		}
	}
}

// C16-5: a sidechain line is skipped wherever it is; the project folder
// maps a non-ASCII character to one "-", as grok-build does.
func TestClaudeDetails(t *testing.T) {
	data := `{"type":"user","message":{"content":"main"}}` + "\n" + `{"type":"assistant","isSidechain":true,"message":{"content":"side"}}` + "\n"
	if turns, _ := parse(adapter.Claude, []byte(data)); texts(turns) != "user:main" {
		t.Errorf("got %q", texts(turns))
	}
	if got := ClaudeProjectDir("/Users/é/a.b"); got != "-Users---a-b" {
		t.Errorf("ClaudeProjectDir = %q", got)
	}
}

// C16-6: a last line longer than the read window is an error that says
// so, not an empty session.
func TestTailLineTooLarge(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "sessions", "2026", "01", "02", "rollout-x-"+id+".jsonl")
	write(t, p, codexLines+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"`+strings.Repeat("a", MaxTail+10)+`"}]}}`+"\n")
	_, err := Tail(adapter.Codex, home, adapter.SessionRef{ID: id}, 0)
	if !errors.Is(err, adapter.ErrNoTranscript) || !strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %v", err)
	}
}
