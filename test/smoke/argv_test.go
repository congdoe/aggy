package smoke

import (
	"os"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/probe"
)

// TestScriptArgvMatchesAdapter keeps smoke.sh's Grok argv equal to what
// the Grok adapter builds, so the live checks run aggy's exact argv.
func TestScriptArgvMatchesAdapter(t *testing.T) {
	script, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		kind adapter.Kind
	}{
		{"GROK_REVIEW_ARGS", adapter.Review},
		{"GROK_IMAGE_ARGS", adapter.Image},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := grok.Adapter{Caps: []string{probe.CapSandbox, probe.CapKernelSandbox}, Getenv: func(k string) string {
				if k == "HOME" {
					return "/smokehome"
				}
				return ""
			}}
			cmd, err := a.Headless(adapter.HeadlessRequest{Kind: tt.kind, PromptFile: "/p/prompt.md", Cwd: "/c"})
			if err != nil {
				t.Fatal(err)
			}
			// The script passes "--prompt-file <its file>" itself.
			if len(cmd.Args) < 2 || cmd.Args[0] != "--prompt-file" || cmd.Args[1] != "/p/prompt.md" {
				t.Fatalf("argv does not start with --prompt-file: %q", cmd.Args)
			}
			// The script writes $HOME as @HOME@ and fills it in at run time.
			want := tt.name + `="` + strings.ReplaceAll(strings.Join(cmd.Args[2:], " "), "/smokehome", "@HOME@") + `"`
			if !strings.Contains(string(script), "\n"+want+"\n") {
				t.Errorf("smoke.sh lacks the line\n%s", want)
			}
		})
	}
}
