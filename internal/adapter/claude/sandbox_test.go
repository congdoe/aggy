package claude

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden settings layers in testdata/settings")

// TestSandboxSettingsGolden (T6980): the inline --settings layer a launch
// passes, byte for byte, with Claude's sandbox ticked for the folder and
// without; with it, the sandbox is on and Claude exits rather than run its
// shell unsandboxed, and a person's own sandbox.enabled false gives way to
// aggy's, named in the overrides note. Run with -update to rewrite.
func TestSandboxSettingsGolden(t *testing.T) {
	cases := []struct {
		name      string
		req       adapter.InteractiveRequest
		overrides []string
	}{
		{"no-sandbox", adapter.InteractiveRequest{Cwd: "/w", Deny: []string{"Bash(rm -rf*)"}, Ask: []string{"Bash(git push*)"}}, nil},
		{"sandbox", adapter.InteractiveRequest{Cwd: "/w", Deny: []string{"Bash(rm -rf*)"}, Ask: []string{"Bash(git push*)"}, ClaudeSandbox: true}, nil},
		{"sandbox-only", adapter.InteractiveRequest{Cwd: "/w", ClaudeSandbox: true}, nil},
		{"sandbox-over-user", adapter.InteractiveRequest{Cwd: "/w", Deny: []string{"Bash(rm -rf*)"}, ClaudeSandbox: true,
			UserSettings: map[string]any{"sandbox": map[string]any{"enabled": false, "excludedCommands": []any{"docker *"}}}},
			[]string{"sandbox.enabled"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok, err := SettingsJSON(c.req)
			if err != nil || !ok {
				t.Fatalf("SettingsJSON: %v %v", ok, err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", "settings", c.name+".golden.json")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("layer:\n%s\nwant:\n%s", got, want)
			}
			if o := SettingsOverrides(c.req); !slices.Equal(o, c.overrides) {
				t.Errorf("overrides %q, want %q", o, c.overrides)
			}
			cmd, err := Adapter{}.Interactive(c.req)
			if err != nil {
				t.Fatal(err)
			}
			if i := slices.Index(cmd.Args, "--settings"); i < 0 || cmd.Args[i+1]+"\n" != string(want) {
				t.Errorf("args %q, want the layer inline", cmd.Args)
			}
		})
	}
}
