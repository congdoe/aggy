package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSmallOutput (F19): each case runs a command and checks one line of what
// it prints.
func TestSmallOutput(t *testing.T) {
	tests := []struct {
		name    string
		do      func(t *testing.T) string // what to check
		want    string
		notWant string
	}{
		{
			name: "skill status after unwire says it is not installed",
			do: func(t *testing.T) string {
				mustRun(t, "skill", "link")
				mustRun(t, "unwire", "--all")
				return mustRun(t, "skill", "status")
			},
			want: "installed: no;", notWant: "installed: /",
		},
		{
			name: "skill status while linked names the folder",
			do: func(t *testing.T) string {
				mustRun(t, "skill", "link")
				return mustRun(t, "skill", "status")
			},
			want: "installed: /", notWant: "installed: no",
		},
		{
			name: "a second skill link changes nothing and says so",
			do: func(t *testing.T) string {
				mustRun(t, "skill", "link")
				return mustRun(t, "skill", "link")
			},
			want: "Nothing to change", notWant: "Done (run",
		},
		{
			name:    "help no longer ends pre-alpha",
			do:      func(t *testing.T) string { return mustRun(t, "help") },
			notWant: "pre-alpha",
		},
		{
			name: "setup no longer calls parity a later release",
			do:   func(t *testing.T) string { _, out, _ := run("setup", "--only", "claude"); return out },
			want: "aggy policy wire", notWant: "later release",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			t.Setenv("PATH", t.TempDir())
			got := tt.do(t)
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Errorf("output %q, want %q", got, tt.want)
			}
			if tt.notWant != "" && strings.Contains(got, tt.notWant) {
				t.Errorf("output %q, has %q", got, tt.notWant)
			}
		})
	}
}

// TestUnwireStrippedSaysSo (F19): undoing hooks in a file edited after the
// install says what happened to it, in the dry run and the real one. The line
// had no verb: "  <file> (change <id>)".
func TestUnwireStrippedSaysSo(t *testing.T) {
	h := hooksHome(t)
	settings := filepath.Join(h, ".claude", "settings.json")
	writeAt(t, settings, userSettings, 0o644)
	mustRun(t, "hooks", "install", "--vendor", "claude")
	b, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	writeAt(t, settings, strings.Replace(string(b), `"model": "opus"`, `"model": "sonnet"`, 1), 0o644)
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"unwire", "--all", "--dry-run"}, `(?m)^would be stripped .*settings\.json \(change `},
		{[]string{"unwire", "--all"}, `(?m)^stripped .*settings\.json \(change `},
	}
	for _, tt := range tests {
		out := mustRun(t, tt.args...)
		if !regexp.MustCompile(tt.want).MatchString(out) {
			t.Errorf("%q printed:\n%s\nwant %s", tt.args, out, tt.want)
		}
	}
}
