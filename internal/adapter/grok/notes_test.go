package grok

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestRunNotes (T6922): a Grok review under $HOME says its read tools were
// denied; outside it only a refusal in the session's history says so.
func TestRunNotes(t *testing.T) {
	home := t.TempDir()
	const denied = `{"type":"tool_result","content":"Denied by permission policy: deny rule on read matching \"~*/**\""}` + "\n"
	tests := []struct {
		name    string
		kind    adapter.Kind
		cwd     string
		real    string // what Realpath makes cwd; "" means unchanged
		history string
		want    string
	}{
		{name: "under home", kind: adapter.Review, cwd: filepath.Join(home, "repo"), want: "under your home folder"},
		{name: "home itself", kind: adapter.Review, cwd: home, want: "under your home folder"},
		{name: "through a link", kind: adapter.Review, cwd: "/link/repo", real: filepath.Join(home, "repo"), want: "under your home folder"},
		{name: "outside, nothing refused", kind: adapter.Review, cwd: "/srv/repo", history: `{"type":"tool_result","content":"ok"}` + "\n"},
		{name: "outside, a refusal", kind: adapter.Review, cwd: "/srv/repo", history: denied, want: "refused a read tool"},
		{name: "outside, a refusal in text parts", kind: adapter.Review, cwd: "/srv/repo",
			history: `{"type":"tool_result","content":[{"type":"text","text":"Denied by permission policy: x"}]}` + "\n", want: "refused a read tool"},
		{name: "outside, a file read quotes it", kind: adapter.Review, cwd: "/srv/repo",
			history: `{"type":"tool_result","content":"docs: the tool says Denied by permission policy here"}` + "\n"},
		{name: "outside, only the prompt quotes it", kind: adapter.Review, cwd: "/srv/repo",
			history: `{"type":"user","content":[{"type":"text","text":"Denied by permission policy"}]}` + "\n"},
		{name: "a home with the same prefix", kind: adapter.Review, cwd: home + "-other/repo"},
		{name: "not a review", kind: adapter.Research, cwd: filepath.Join(home, "repo"), history: denied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := t.TempDir()
			a := Adapter{
				Getenv: func(k string) string {
					switch k {
					case "GROK_HOME":
						return gh
					case "HOME":
						return home
					}
					return ""
				},
				Realpath: func(p string) (string, error) {
					if p == tt.cwd && tt.real != "" {
						return tt.real, nil
					}
					return p, nil
				},
			}
			if tt.history != "" {
				dir := filepath.Join(gh, "sessions", EncodeCwd(tt.cwd), sid)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "chat_history.jsonl"), []byte(tt.history), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := a.RunNotes(adapter.HeadlessRequest{Kind: tt.kind, Cwd: tt.cwd}, adapter.HeadlessResult{SessionID: sid})
			if tt.want == "" {
				if len(got) != 0 {
					t.Fatalf("notes = %q", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], tt.want) {
				t.Fatalf("notes = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSelfTestHistory (T6951): the history is found under another working dir's
// name, is no record when missing, and is unreadable when over the cap, never
// silently no evidence.
func TestSelfTestHistory(t *testing.T) {
	const token = "aggy-canary-x"
	tests := []struct {
		name  string
		setup func(t *testing.T, sessions string)
		want  adapter.SelfTestEvidence
	}{
		{"missing", func(*testing.T, string) {}, adapter.SelfTestEvidence{NoRecord: true}},
		{"under the cwd as resolved", func(t *testing.T, s string) {
			writeHistory(t, filepath.Join(s, EncodeCwd("/private/w"), sid), token)
		}, adapter.SelfTestEvidence{Read: true}},
		{"over the cap", func(t *testing.T, s string) {
			p := writeHistory(t, filepath.Join(s, EncodeCwd("/w"), sid), "x")
			if err := os.Truncate(p, maxHistory+1); err != nil {
				t.Fatal(err)
			}
		}, adapter.SelfTestEvidence{Unreadable: true}},
		{"under two working dirs", func(t *testing.T, s string) {
			writeHistory(t, filepath.Join(s, "a", sid), "x")
			writeHistory(t, filepath.Join(s, "b", sid), "x")
		}, adapter.SelfTestEvidence{Unreadable: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := t.TempDir()
			tt.setup(t, filepath.Join(gh, "sessions"))
			a := Adapter{Getenv: func(k string) string {
				if k == "GROK_HOME" {
					return gh
				}
				return ""
			}}
			got := a.SelfTestEvidence(adapter.HeadlessRequest{Cwd: "/w"}, adapter.HeadlessResult{SessionID: sid}, adapter.Output{}, token)
			if got.Read != tt.want.Read || got.Unreadable != tt.want.Unreadable || got.NoRecord != tt.want.NoRecord {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func writeHistory(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "chat_history.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
