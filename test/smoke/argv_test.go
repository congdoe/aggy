package smoke

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/probe"
)

// plainArg is an argument bash takes as it is, unquoted, in an array.
var plainArg = regexp.MustCompile(`^[A-Za-z0-9_./,=:@+-]+$`)

// bashWord quotes s for a bash 3.2 array literal: as it is when plain,
// else as $'...' with every byte outside printable ASCII as \xHH.
func bashWord(s string) string {
	if plainArg.MatchString(s) {
		return s
	}
	var b strings.Builder
	b.WriteString("$'")
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' || c == '\'':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("'")
	return b.String()
}

// TestScriptArgvMatchesAdapter keeps smoke.sh's Grok argv equal to what
// the Grok adapter builds on macOS, so the live checks run aggy's exact
// argv. The script holds them as bash arrays, since some deny rules hold
// quotes and white space, and writes $HOME as @HOME@ and its case-folded
// glob form as @HOMEFOLD@.
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
			a := grok.Adapter{Caps: []string{probe.CapSandbox, probe.CapKernelSandbox}, GOOS: "darwin", Getenv: func(k string) string {
				if k == "HOME" {
					return "/smokehome"
				}
				return ""
			}, Realpath: func(p string) (string, error) { return p, nil }}
			cmd, err := a.Headless(adapter.HeadlessRequest{Kind: tt.kind, PromptFile: "/p/prompt.md", Cwd: "/c"})
			if err != nil {
				t.Fatal(err)
			}
			// The script passes "--prompt-file <its file>" itself.
			if len(cmd.Args) < 2 || cmd.Args[0] != "--prompt-file" || cmd.Args[1] != "/p/prompt.md" {
				t.Fatalf("argv does not start with --prompt-file: %q", cmd.Args)
			}
			var words []string
			for _, arg := range cmd.Args[2:] {
				arg = strings.ReplaceAll(arg, "/[Ss][Mm][Oo][Kk][Ee][Hh][Oo][Mm][Ee]", "@HOMEFOLD@")
				words = append(words, bashWord(strings.ReplaceAll(arg, "/smokehome", "@HOME@")))
			}
			want := tt.name + "=(" + strings.Join(words, " ") + ")"
			if !strings.Contains(string(script), "\n"+want+"\n") {
				t.Errorf("smoke.sh lacks the line\n%s", want)
			}
		})
	}
}

// TestScriptArraysExpand runs smoke.sh's Grok argv block under bash (as
// macOS ships it, 3.2, where available) with HOME /smokehome, and checks
// each array expands to the adapter's exact argv: the $'...' quoting, the
// @HOME@ and @HOMEFOLD@ substitutions, and the control argv.
func TestScriptArraysExpand(t *testing.T) {
	script, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(script)
	i := strings.Index(s, "\nGROK_REVIEW_ARGS=(")
	if i < 0 {
		t.Fatal("no argv block in smoke.sh")
	}
	j := strings.Index(s[i:], "\ndone\n")
	if j < 0 {
		t.Fatal("no end to the argv block in smoke.sh")
	}
	block := "HOME=/smokehome\n" + s[i:i+j+6] +
		`printf '%s\0' "${GROK_REVIEW_ARGS[@]}"; printf '\1'; printf '%s\0' "${GROK_IMAGE_ARGS[@]}"; printf '\1'; printf '%s\0' "${GROK_CONTROL_ARGS[@]}"`
	bash := "/bin/bash"
	if _, err := os.Stat(bash); err != nil {
		bash = "bash"
	}
	cmd := exec.Command(bash, "-c", block)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	parts := strings.Split(string(out), "\x01")
	if len(parts) != 3 {
		t.Fatalf("output %q", out)
	}
	a := grok.Adapter{Caps: []string{probe.CapSandbox, probe.CapKernelSandbox}, GOOS: "darwin", Getenv: func(k string) string {
		if k == "HOME" {
			return "/smokehome"
		}
		return ""
	}, Realpath: func(p string) (string, error) { return p, nil }}
	argv := func(k adapter.Kind) []string {
		c, err := a.Headless(adapter.HeadlessRequest{Kind: k, PromptFile: "/p/prompt.md", Cwd: "/c"})
		if err != nil {
			t.Fatal(err)
		}
		return c.Args[2:]
	}
	review := argv(adapter.Review)
	var control []string
	for _, w := range review {
		if w != "--sandbox" && w != "strict" {
			control = append(control, w)
		}
	}
	for n, want := range [][]string{review, argv(adapter.Image), control} {
		got := strings.Split(strings.TrimSuffix(parts[n], "\x00"), "\x00")
		if !slices.Equal(got, want) {
			t.Errorf("array %d expands to\n%q\nwant\n%q", n, got, want)
		}
	}
}
