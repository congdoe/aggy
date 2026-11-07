package cli

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestHelpEveryCommand (F15): --help, -h, and aggy help X give each command's
// own help on stdout and exit 0, and start nothing. The commands come from
// aggy's own help and from Run's switch, so a new one cannot be missed.
func TestHelpEveryCommand(t *testing.T) {
	var cmds []string
	section := strings.SplitAfter(usage, "Commands:\n")[1]
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z-]+)`).FindAllStringSubmatch(section, -1) {
		cmds = append(cmds, m[1])
	}
	src, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?m)^\tcase (.*):$`).FindAllStringSubmatch(string(src), -1) {
		for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
			if q[1] != "--help" && q[1] != "-h" {
				cmds = append(cmds, q[1])
			}
		}
	}
	if len(cmds) < 30 {
		t.Fatalf("only %d commands found: %q", len(cmds), cmds)
	}
	for _, cmd := range cmds {
		for _, args := range [][]string{{cmd, "--help"}, {cmd, "-h"}, {"help", cmd}} {
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				isolate(t)
				t.Setenv("PATH", t.TempDir())
				var out, errb bytes.Buffer
				if code := Run(args, &out, &errb); code != 0 {
					t.Errorf("exit = %d, stderr %q", code, errb.String())
				}
				if errb.Len() != 0 {
					t.Errorf("stderr = %q", errb.String())
				}
				if !strings.Contains(out.String(), "aggy") || strings.Contains(out.String(), "Commands:") {
					t.Errorf("stdout = %q, want this command's own help", out.String())
				}
			})
		}
	}
}

// TestHelpSubcommands: a subcommand's --help is its command's help, and a
// flag after the arguments counts; after "--" it belongs to something else.
func TestHelpSubcommands(t *testing.T) {
	tests := [][]string{
		{"tool", "list", "--help"}, {"tool", "on", "x", "--help"}, {"tool", "add", "-h"}, {"tool", "status", "--help"},
		{"skill", "link", "--help"}, {"skill", "status", "-h"}, {"skill", "add", "--help"},
		{"hooks", "install", "--help"}, {"hooks", "status", "-h"},
		{"memory", "import", "--help"}, {"policy", "wire", "--help"}, {"mcp", "wire", "-h"},
		{"attribution", "wire", "--help"}, {"diag", "show", "--help"}, {"diag", "codes", "-h"},
		{"dev", "capture", "--help"}, {"dev", "scrub", "-h"},
		{"resume", "abcd", "--help"}, {"run", "claude", "--help"}, {"claude", "--model", "x", "--help"},
		{"borrow", "research", "--help"}, {"switch", "--brief", "-h"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			var out, errb bytes.Buffer
			if code := Run(args, &out, &errb); code != 0 || errb.Len() != 0 {
				t.Errorf("exit = %d, stderr %q", code, errb.String())
			}
			if out.String() != commandHelp[args[0]] {
				t.Errorf("stdout = %q", out.String())
			}
		})
	}
	if wantsHelp([]string{"capture", "claude", "--", "--help"}) {
		t.Error(`a --help after "--" was taken as aggy's`)
	}
}

// TestUsageExits (F15): a group command with no subcommand, and a flag a
// command does not take, are usage errors alike: exit 2, tagged, usage text
// on stderr, nothing on stdout.
func TestUsageExits(t *testing.T) {
	for _, args := range [][]string{
		{"skill"}, {"hooks"}, {"tool"}, {"memory"}, {"policy"}, {"mcp"}, {"attribution"}, {"dev"},
		{"version", "--frob"}, {"--version", "x"}, {"-v", "x"}, {"help", "frob"}, {"help", "a", "b"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			var out, errb bytes.Buffer
			if code := Run(args, &out, &errb); code != 2 {
				t.Errorf("exit = %d", code)
			}
			if out.Len() != 0 || !strings.Contains(errb.String(), "[cli.usage · run ") {
				t.Errorf("stdout %q, stderr %q", out.String(), errb.String())
			}
		})
	}
}
