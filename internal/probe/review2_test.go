package probe

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// Regression tests for the PR #4 re-review (N2, N3, N8, L2). Each fails on
// 2e1e234.

// codexRun answers --version and the top-level help like codex 0.164.0,
// and the exec help and exec resume help from the arguments.
func codexRun(execHelp string, resume adapter.Output) adapter.Runner {
	return func(_ context.Context, c adapter.Command) (adapter.Output, error) {
		switch strings.Join(c.Args, " ") {
		case "--version":
			return adapter.Output{Stdout: []byte("codex-cli 0.164.0\n")}, nil
		case "--help":
			return adapter.Output{Stdout: []byte("Commands:\n  exec    Run\n  resume  Resume\n")}, nil
		case "exec --help":
			return adapter.Output{Stdout: []byte(execHelp)}, nil
		case "exec resume --help":
			return resume, nil
		}
		return adapter.Output{ExitCode: 2}, nil
	}
}

// TestCodexResumeUsageErrorIsCachedAbsent (N2): a codex whose exec has no
// resume subcommand answers with a usage error. That is a stable absent
// capability, so the probe is complete and cached, not re-run forever.
func TestCodexResumeUsageErrorIsCachedAbsent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	usage := adapter.Output{ExitCode: 2, Stderr: []byte("error: unrecognized subcommand 'resume'\n\nUsage: codex exec [OPTIONS] [PROMPT]\n")}
	pr := &Prober{Store: f.store, Run: codexRun("Usage: codex exec [OPTIONS] [PROMPT]\n  --json\n", usage)}
	p, cached, err := pr.Probe(ctx, adapter.Codex, "/x/codex")
	if err != nil || cached || Has(p, CapResume) || !Has(p, CapJSONOutput) {
		t.Fatalf("first probe = %+v, cached %v, %v; want complete, without resume", p, cached, err)
	}
	if _, cached, err := pr.Probe(ctx, adapter.Codex, "/x/codex"); err != nil || !cached {
		t.Errorf("second probe cached %v, %v; want cached", cached, err)
	}
}

// TestCodexResumeNeedsHelpEvidence (N2): an exit 0 alone does not prove
// exec resume exists, since clap may take "resume" as a prompt and print
// the exec help. Presence comes from the exec Commands block or a
// session-id argument in the resume help.
func TestCodexResumeNeedsHelpEvidence(t *testing.T) {
	execHelp := "Usage: codex exec [OPTIONS] [PROMPT]\n\nOptions:\n  --json\n"
	tests := []struct {
		name     string
		execHelp string
		resume   adapter.Output
		want     bool
	}{
		{"exit 0 with the exec help", execHelp, adapter.Output{Stdout: []byte(execHelp)}, false},
		{"exit 0 with no output", execHelp, adapter.Output{}, false},
		{"session id argument", execHelp, adapter.Output{Stdout: []byte("Usage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]\n")}, true},
		{"listed in exec commands", "Usage: codex exec [OPTIONS]\n\nCommands:\n  resume  Resume a previous session\n  help    Print help\n", adapter.Output{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Detect(context.Background(), codexRun(tt.execHelp, tt.resume), adapter.Codex, "/x/codex")
			if err != nil || Has(p, CapResume) != tt.want {
				t.Errorf("caps = %v, %v; want resume %v", p.Capabilities, err, tt.want)
			}
		})
	}
}

// TestParseVersionKeyedOnProgram (N3).
func TestParseVersionKeyedOnProgram(t *testing.T) {
	claude := Specs[adapter.Claude].Programs
	codex := Specs[adapter.Codex].Programs
	grok := Specs[adapter.Grok].Programs
	tests := []struct {
		name     string
		out      string
		programs []string
		want     string
		wantErr  error
	}{
		{"program line before a runtime line", "codex-cli 0.164.0\nnode v20.11.1\n", codex, "0.164.0", nil},
		{"program line after a runtime line", "node v20.11.1\ncodex-cli 0.164.0\n", codex, "0.164.0", nil},
		{"two-word name with a v prefix", "Claude Code v2.1.311\n", claude, "2.1.311", nil},
		{"name in the note", "2.1.311 (Claude Code)\n", claude, "2.1.311", nil},
		{"name in another case", "GROK 1.0.70\n", grok, "1.0.70", nil},
		{"one unnamed line", "1.0.70\n", grok, "1.0.70", nil},
		{"several unnamed lines", "tool 1.0.0\nnode v20.11.1\n", grok, "", ErrAmbiguousVersion},
		{"several named lines", "codex-cli 0.164.0\ncodex 0.165.0\n", codex, "", ErrAmbiguousVersion},
		{"same version twice", "codex-cli 0.164.0\ncodex 0.164.0\n", codex, "0.164.0", nil},
		{"no names given, several lines", "codex-cli 0.164.0\nnode v20.11.1\n", nil, "", ErrAmbiguousVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVersion([]byte(tt.out), nil, tt.programs)
			if got != tt.want || !errors.Is(err, tt.wantErr) {
				t.Errorf("ParseVersion = %q, %v; want %q, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// TestCommandsBlockSkipsWrappedLines (N8): a description wrapped onto the
// next line is not a command.
func TestCommandsBlockSkipsWrappedLines(t *testing.T) {
	help := "Usage: codex [OPTIONS]\n\nCommands:\n" +
		"  exec        Run Codex non-interactively; see\n" +
		"              resume to continue a run\n" +
		"  login       Manage login\n" +
		"\tlogout      Remove credentials\n" +
		"\nOptions:\n  -c <key=value>\n"
	if got, want := commandsBlock(help), []string{"exec", "login"}; !slices.Equal(got, want) {
		t.Errorf("commandsBlock = %v, want %v", got, want)
	}
}

// TestEnvAllowlist (L2): a command may add only AGGY_* variables. Each of
// these passed the old deny list.
func TestEnvAllowlist(t *testing.T) {
	base := []string{"HOME=/h", "PATH=/usr/bin"}
	for _, k := range []string{
		"BASH_ENV", "ENV", "NODE_PATH", "GIT_SSH_COMMAND", "GIT_CONFIG_GLOBAL", "PYTHONPATH",
		"PERL5OPT", "RUBYOPT", "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "npm_config_registry",
		"ELECTRON_RUN_AS_NODE", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME", "XDG_CONFIG_HOME",
		"aggy_delegated",
	} {
		if _, err := Env(base, []string{k + "=x"}); err == nil {
			t.Errorf("%s accepted in a command's env", k)
		}
	}
	env, err := Env(base, []string{"AGGY_DELEGATED=1", "AGGY_FAKE_SCENARIO=/s"})
	if err != nil || !slices.Contains(env, "AGGY_DELEGATED=1") {
		t.Errorf("AGGY_* refused: %v, %v", env, err)
	}
}

// TestEnvRelativePathUnset (L2): when every PATH entry is relative, PATH is
// left unset rather than set empty.
func TestEnvRelativePathUnset(t *testing.T) {
	env, err := Env([]string{"HOME=/h", "PATH=.:bin::rel/x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			t.Errorf("PATH set to %q, want unset", kv)
		}
	}
}

// TestLookPath finds an executable on the absolute PATH entries only.
func TestLookPath(t *testing.T) {
	dir := t.TempDir()
	if err := safefs.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := safefs.WriteFile(filepath.Join(dir, "plain"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "rel:"+dir)
	if p, err := LookPath("codex"); err != nil || p != filepath.Join(dir, "codex") {
		t.Errorf("LookPath = %q, %v", p, err)
	}
	for _, name := range []string{"plain", "missing", "../codex", ""} {
		if _, err := LookPath(name); err == nil {
			t.Errorf("LookPath(%q) succeeded", name)
		}
	}
}
