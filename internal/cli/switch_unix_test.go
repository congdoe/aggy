//go:build unix

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/supervisor"
)

// TestSwitchCommand (T6668): aggy switch sends a switch-request and
// reports the supervisor's answer.
func TestSwitchCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, err := os.MkdirTemp("/tmp", "aggyw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	d, _ = filepath.EvalSymlinks(d)
	t.Setenv("AGGY_HOME", d)
	srv, err := supervisor.Listen(filepath.Join(d, "run"), "11111111-2222-4333-8444-555555555555", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetRoot(os.Getpid()) // the test plays the CLI
	var asked []supervisor.Request
	srv.SetSwitch(func(r supervisor.Request) error {
		if r.Vendor == "codex" {
			return errors.New("codex is not offered here")
		}
		asked = append(asked, r)
		return nil
	})
	env := func(sock string) func(string) string {
		return func(k string) string { return map[string]string{"AGGY_SOCKET": sock}[k] }
	}
	tests := []struct {
		name   string
		args   []string
		sock   string
		code   int
		stdout string
		stderr string
	}{
		{"queued", []string{"grok"}, srv.Path(), 0, "when this turn ends", ""},
		{"with a brief", []string{"--brief", "grok"}, srv.Path(), 0, "when this turn ends", ""},
		{"with tools", []string{"--tool", "context7", "--tool=codegraph", "--tool", "context7", "grok"}, srv.Path(), 0, "when this turn ends", ""},
		{"a bad tool name", []string{"--tool", "../x", "grok"}, srv.Path(), 2, "", "[cli.usage · run "},
		{"--tool with no name", []string{"grok", "--tool"}, srv.Path(), 2, "", "needs a tool name"},
		{"no --new for agents", []string{"--new", "grok"}, srv.Path(), 2, "", "[switch.agent_options · run "},
		{"no --model for agents", []string{"--model", "grok-5", "grok"}, srv.Path(), 2, "", "user's choice"},
		{"unknown flag", []string{"--fork", "grok"}, srv.Path(), 2, "", "usage"},
		{"refused", []string{"codex"}, srv.Path(), 1, "", "not offered here [switch.vendor_unavailable · run "},
		{"unknown vendor", []string{"gpt"}, srv.Path(), 2, "", "unknown vendor"},
		{"no vendor", nil, srv.Path(), 2, "", "usage"},
		{"outside aggy", []string{"grok"}, "", 1, "", "[switch.not_supervised · run "},
		{"socket elsewhere", []string{"grok"}, "/tmp/11111111-2222-4333-8444-555555555555.sock", 1, "", "run directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := switchWith(tt.args, env(tt.sock), &out, &errb)
			if code != tt.code || !strings.Contains(out.String(), tt.stdout) || !strings.Contains(errb.String(), tt.stderr) {
				t.Errorf("code %d stdout %q stderr %q", code, out.String(), errb.String())
			}
		})
	}
	if len(asked) != 3 || asked[0].Vendor != "grok" || asked[0].Brief || !asked[1].Brief ||
		!slices.Equal(asked[2].Tools, []string{"context7", "codegraph"}) {
		t.Errorf("switch requests = %+v, want grok, grok with --brief, grok with two tools", asked)
	}
}
