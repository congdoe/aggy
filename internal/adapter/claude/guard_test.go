package claude

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/probe"
)

// TestGuardSettings (T6624): the settings layer runs the guard before
// every tool call, its words quoted so a shell reads them back exactly.
func TestGuardSettings(t *testing.T) {
	guard := []string{"/Applications/My Tools/aggy", "hook", "pretooluse"}
	b, err := GuardSettings(guard)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct{ Type, Command string }
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	pre := s.Hooks["PreToolUse"]
	if len(s.Hooks) != 1 || len(pre) != 1 || pre[0].Matcher != "*" || len(pre[0].Hooks) != 1 || pre[0].Hooks[0].Type != "command" {
		t.Fatalf("settings %s", b)
	}
	// A shell splits the command back into exactly the guard's words.
	out, err := exec.Command("/bin/sh", "-c", `printf '%s\n' `+pre[0].Hooks[0].Command).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"); !slices.Equal(got, guard) {
		t.Errorf("shell words %q, want %q", got, guard)
	}
	for _, bad := range [][]string{nil, {"aggy", "hook"}, {"/x/it's/aggy"}, {"/x/aggy", "a\nb"}, {"/x/aggy", ""}} {
		if _, err := GuardSettings(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestGuardWiring (T6624): --settings is passed, and the settings file
// asked for, only with a guard command and the probed capability.
func TestGuardWiring(t *testing.T) {
	h := fakecli.NewHarness(t, fakecli.Build(t))
	guard := []string{"/usr/local/bin/aggy", "hook", "pretooluse"}
	tests := []struct {
		name  string
		caps  []string
		guard []string
		want  bool
	}{
		{"capability and guard", []string{probe.CapSettings}, guard, true},
		{"no capability", nil, guard, false},
		{"no guard", []string{probe.CapSettings}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := Adapter{Caps: tt.caps}
			req := adapter.HeadlessRequest{Kind: adapter.Review, PromptFile: h.Prompt, Cwd: h.Cwd, RunDir: h.RunDir, GuardCommand: tt.guard}
			c, err := a.Headless(req)
			if err != nil {
				t.Fatal(err)
			}
			files, err := a.ChildFiles(req)
			if err != nil {
				t.Fatal(err)
			}
			hasFile := slices.ContainsFunc(files, func(f adapter.ChildFile) bool { return f.Name == SettingsFile })
			i := slices.Index(c.Args, "--settings")
			if (i >= 0) != tt.want || hasFile != tt.want {
				t.Fatalf("args %q files %v", c.Args, files)
			}
			if tt.want && c.Args[i+1] != filepath.Join(h.RunDir, SettingsFile) {
				t.Errorf("--settings %q", c.Args[i+1])
			}
			out, _ := h.Run(t, c, `{"extra_flags":["--settings"],"json":{"type":"result","result":"ok"}}`)
			if out.ExitCode != 0 {
				t.Errorf("fake: %s", out.Stderr)
			}
		})
	}
}

// TestReviewSettingSources (#10-6): a review loads only the user's
// settings when the CLI takes --setting-sources, so the reviewed repo's
// own hooks do not run.
func TestReviewSettingSources(t *testing.T) {
	tests := []struct {
		name string
		caps []string
		kind adapter.Kind
		want bool
	}{
		{"review with the capability", []string{probe.CapSettingSources}, adapter.Review, true},
		{"review without it", nil, adapter.Review, false},
		{"edit", []string{probe.CapSettingSources}, adapter.Edit, false},
	}
	for _, tt := range tests {
		c, err := Adapter{Caps: tt.caps}.Headless(adapter.HeadlessRequest{Kind: tt.kind, PromptFile: "/p", Cwd: "/w", RunDir: "/r"})
		if err != nil {
			t.Fatal(err)
		}
		i := slices.Index(c.Args, "--setting-sources")
		if (i >= 0) != tt.want || (tt.want && c.Args[i+1] != "user") {
			t.Errorf("%s: %q", tt.name, c.Args)
		}
	}
}
