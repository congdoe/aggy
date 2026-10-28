package probe

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// Regression tests for the PR #4 review (C6, C8, C9, C14, S6, S7, S12).
// Each fails on 522a431.

// TestVersionIsAnchored (C6): a version in a notice is not the CLI's, and
// stderr is read when stdout is empty.
func TestVersionIsAnchored(t *testing.T) {
	tests := []struct {
		name           string
		stdout, stderr string
		want           string
	}{
		{"notice before", "A new version 3.0.0 is available\ncodex-cli 0.164.0\n", "", "0.164.0"},
		{"notice after", "grok 1.0.70\nnote: 9.9.9 is the latest", "", "1.0.70"},
		{"stderr only", "", "claude 2.1.311\n", "2.1.311"},
	}
	for _, tt := range tests {
		run := func(context.Context, adapter.Command) (adapter.Output, error) {
			return adapter.Output{Stdout: []byte(tt.stdout), Stderr: []byte(tt.stderr)}, nil
		}
		if got, err := Version(context.Background(), run, "/x/cli", nil); err != nil || got != tt.want {
			t.Errorf("%s: Version = %q, %v, want %q", tt.name, got, err, tt.want)
		}
	}
}

// TestProbeNotCachedAfterHelpFailure (C8): a help run that fails once is
// run again on the next probe, and the full result is then cached.
func TestProbeNotCachedAfterHelpFailure(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	helpRuns := 0
	run := func(_ context.Context, c adapter.Command) (adapter.Output, error) {
		if c.Args[0] == "--version" {
			return adapter.Output{Stdout: []byte("grok 1.0.70\n")}, nil
		}
		helpRuns++
		if helpRuns == 1 {
			return adapter.Output{ExitCode: 1, Stderr: []byte("transient")}, nil
		}
		return adapter.Output{Stdout: []byte("  --session-id <uuid>\n")}, nil
	}
	pr := &Prober{Store: f.store, Run: run}
	if _, _, err := pr.Probe(ctx, adapter.Grok, "/x/grok"); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("first probe = %v, want ErrIncomplete", err)
	}
	p, cached, err := pr.Probe(ctx, adapter.Grok, "/x/grok")
	if err != nil || cached || !Has(p, CapSessionID) {
		t.Fatalf("second probe = %+v, cached %v, %v", p, cached, err)
	}
	if _, cached, _ := pr.Probe(ctx, adapter.Grok, "/x/grok"); !cached || helpRuns != 2 {
		t.Errorf("third probe cached %v after %d help runs, want cached after 2", cached, helpRuns)
	}
}

// TestCodexSubcommandsFromCommandsBlock (C9): exec and resume count only
// as Commands: entries and a working exec resume --help, not as words in
// prose.
func TestCodexSubcommandsFromCommandsBlock(t *testing.T) {
	prose := "Usage: codex [OPTIONS]\n\nYou can exec a prompt and resume it later.\n\nOptions:\n  -c <key=value>\n"
	listed := "Usage: codex [OPTIONS]\n\nCommands:\n  exec    Run non-interactively\n  resume  Resume a session\n\nOptions:\n  -c <key=value>\n"
	for _, tt := range []struct {
		name       string
		top        string
		resumeExit int
		want       []string
	}{
		{"prose only", prose, 2, []string{CapConfigOverride}},
		{"listed", listed, 0, []string{CapHeadless, CapResume, CapConfigOverride}},
	} {
		run := func(_ context.Context, c adapter.Command) (adapter.Output, error) {
			switch strings.Join(c.Args, " ") {
			case "--version":
				return adapter.Output{Stdout: []byte("codex-cli 0.164.0")}, nil
			case "--help":
				return adapter.Output{Stdout: []byte(tt.top)}, nil
			case "exec resume --help":
				if tt.resumeExit != 0 {
					return adapter.Output{ExitCode: tt.resumeExit}, nil
				}
				return adapter.Output{Stdout: []byte("Usage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]\n")}, nil
			}
			return adapter.Output{ExitCode: 2}, nil
		}
		p, err := Detect(context.Background(), run, adapter.Codex, "/x/codex")
		if err != nil || !slices.Equal(p.Capabilities, tt.want) {
			t.Errorf("%s: caps = %v, %v, want %v", tt.name, p.Capabilities, err, tt.want)
		}
	}
}
