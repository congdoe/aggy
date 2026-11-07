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
