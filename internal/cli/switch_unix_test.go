//go:build unix

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
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
	var asked []string
	srv.SetSwitch(func(r supervisor.Request) error {
		if r.Vendor == "codex" {
			return errors.New("codex is not offered here")
		}
		asked = append(asked, r.Vendor)
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
		{"refused", []string{"codex"}, srv.Path(), 1, "", "not offered here"},
		{"unknown vendor", []string{"gpt"}, srv.Path(), 2, "", "unknown vendor"},
		{"no vendor", nil, srv.Path(), 2, "", "usage"},
		{"outside aggy", []string{"grok"}, "", 1, "", "not inside a CLI aggy runs"},
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
	if len(asked) != 1 || asked[0] != "grok" {
		t.Errorf("switch requests = %v, want one for grok", asked)
	}
}
