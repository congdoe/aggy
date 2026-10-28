package notify

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

func TestClean(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"plain", "Codex hit its usage limit.", 100, "Codex hit its usage limit."},
		{"applescript quotes stay data", `x" & (do shell script "id") & "`, 100, `x" (do shell script "id") "`},
		{"markup dropped", `<b>bold</b> &amp; <a href="x">`, 100, `bbold/b amp; a href="x"`},
		{"controls and newlines", "a\x1b[31m\nb\r\tc\x00", 100, "a[31m b c"},
		{"bidi override dropped", "a\u202eb", 100, "ab"},
		{"leading dash trimmed", "--help me", 100, "help me"},
		{"capped", strings.Repeat("é", 300), 10, strings.Repeat("é", 10)},
		{"cap counts the space", "ab cd", 3, "ab"},
		{"only junk", "\x01<>&", 100, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Clean(tt.in, tt.max); got != tt.want {
				t.Errorf("Clean(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCommand(t *testing.T) {
	hostile := `"); do shell script "rm -rf ~"; ("`
	tests := []struct {
		name     string
		goos     string
		path     string
		title    string
		wantArgs []string
		wantOK   bool
	}{
		{"darwin passes text as argv", "darwin", Osascript, "aggy", append(slices.Clone(osaScript), "aggy", Clean(hostile, MaxBody)), true},
		{"linux after --", "linux", "/usr/bin/notify-send", "aggy", []string{"--", "aggy", Clean(hostile, MaxBody)}, true},
		{"empty title", "linux", "/usr/bin/notify-send", "\x01", []string{"--", "aggy", Clean(hostile, MaxBody)}, true},
		{"windows has none", "windows", "x", "aggy", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, ok := Command(tt.goos, tt.path, tt.title, hostile)
			if ok != tt.wantOK || !slices.Equal(c.Args, tt.wantArgs) || ok && (c.Path != tt.path || len(c.Env) != 0 || c.Stdin != adapter.StdinNull) {
				t.Errorf("Command = %+v %v, want args %q", c, ok, tt.wantArgs)
			}
		})
	}
	// The script itself is fixed: no text reaches an -e argument.
	c, _ := Command("darwin", Osascript, "t", hostile)
	for i, a := range c.Args {
		if a == "-e" && strings.Contains(c.Args[i+1], "rm -rf") {
			t.Errorf("text reached the script: %q", c.Args[i+1])
		}
	}
}

func TestShowRateLimitsAndFailsQuietly(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	var ran []adapter.Command
	n := &Notifier{
		GOOS: "linux", Path: "/usr/bin/notify-send", Every: 30 * time.Second, Now: func() time.Time { return now },
		Run: func(_ context.Context, c adapter.Command) (adapter.Output, error) {
			ran = append(ran, c)
			return adapter.Output{ExitCode: 1}, context.DeadlineExceeded
		},
	}
	ctx := context.Background()
	n.Show(ctx, "a", "aggy", "one")
	n.Show(ctx, "a", "aggy", "two") // within Every
	n.Show(ctx, "b", "aggy", "three")
	now = now.Add(31 * time.Second)
	n.Show(ctx, "a", "aggy", "four")
	var bodies []string
	for _, c := range ran {
		bodies = append(bodies, c.Args[len(c.Args)-1])
	}
	if !slices.Equal(bodies, []string{"one", "three", "four"}) {
		t.Errorf("shown = %q", bodies)
	}
	var none *Notifier
	none.Show(ctx, "a", "aggy", "x") // nil shows nothing
}
