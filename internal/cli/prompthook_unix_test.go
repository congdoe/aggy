//go:build unix

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/congdoe/aggy/internal/supervisor"
)

// TestPromptSubmitBlocks (T6910): aggy hook UserPromptSubmit hands a command
// with no model turn to the supervisor and, when taken, blocks the prompt with a
// JSON decision; anything else prints nothing, so the prompt goes on.
func TestPromptSubmitBlocks(t *testing.T) {
	d, err := os.MkdirTemp("/tmp", "aggyp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	d, _ = filepath.EvalSymlinks(d)
	t.Setenv("AGGY_HOME", d)
	var mu sync.Mutex
	var got []string
	srv, err := supervisor.Listen(filepath.Join(d, "run"), "11111111-2222-4333-8444-555555555555", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetRoot(os.Getpid())
	srv.SetSwitch(func(r supervisor.Request) error {
		mu.Lock()
		defer mu.Unlock()
		if r.Type != supervisor.ReqPromptCommand {
			return errors.New("not a prompt command")
		}
		got = append(got, r.Vendor+" "+r.Command)
		if strings.Contains(r.Command, "refuse") {
			return errors.New("refused")
		}
		return nil
	})
	env := func(v string) func(string) string {
		m := map[string]string{"AGGY_SOCKET": srv.Path(), "AGGY_VENDOR": v}
		return func(k string) string { return m[k] }
	}
	none := func(string) string { return "" }
	for _, c := range []struct {
		name    string
		vendor  string
		payload string
		env     func(string) string
		sent    string
		blocks  bool
	}{
		{"a switch", "claude", `{"prompt":"/switch grok\n"}`, env("claude"), "claude /switch grok", true},
		{"grok's inbox", "grok", `{"prompt":"/inbox"}`, env("grok"), "grok /inbox", true},
		{"a question for the helper", "claude", `{"prompt":"/aggy why is it slow"}`, env("claude"), "", false},
		{"a borrow with a task", "claude", `{"prompt":"/borrow research what changed"}`, env("claude"), "", false},
		{"keyboard only", "claude", `{"prompt":"/aggy accept 9f2"}`, env("claude"), "", false},
		{"a message", "claude", `{"prompt":"hello"}`, env("claude"), "", false},
		{"outside aggy", "claude", `{"prompt":"/switch grok"}`, none, "", false},
		{"another vendor on screen", "claude", `{"prompt":"/switch grok"}`, env("grok"), "", false},
		{"not JSON", "claude", `/switch grok`, env("claude"), "", false},
		{"the supervisor refuses", "claude", `{"prompt":"/inbox refuse"}`, env("claude"), "claude /inbox refuse", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			mu.Lock()
			got = nil
			mu.Unlock()
			var out, errOut bytes.Buffer
			code := hookWith([]string{"--vendor", c.vendor, "UserPromptSubmit"}, strings.NewReader(c.payload), c.env, &out, &errOut)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut.String())
			}
			mu.Lock()
			sent := strings.Join(got, ",")
			mu.Unlock()
			if sent != c.sent {
				t.Errorf("sent %q, want %q", sent, c.sent)
			}
			if !c.blocks {
				if out.Len() > 0 {
					t.Errorf("stdout %q, want nothing", out.String())
				}
				return
			}
			var dec map[string]any
			if err := json.Unmarshal(out.Bytes(), &dec); err != nil || dec["decision"] != "block" || dec["reason"] == "" {
				t.Fatalf("stdout %q: want a block decision", out.String())
			}
			if _, ok := dec["suppressOriginalPrompt"]; ok != (c.vendor == "claude") {
				t.Errorf("suppressOriginalPrompt on %s: %v", c.vendor, ok)
			}
		})
	}
}
