package fakecli

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Regression tests for the PR #4 re-review (N8, M5). Each fails on 2e1e234.

// TestFakesRequirePrompt (N8): "codex exec -" reads its prompt from stdin,
// so an empty stdin is no prompt. Grok's -p is clap's --single <PROMPT>
// (live diagnostic 2026-10-28, grok 1.0.70): it needs a value, never
// reads stdin, and conflicts with a positional prompt; --prompt-file is
// the other headless form. T6740: aggy's old "-p" plus stdin argv fails.
func TestFakesRequirePrompt(t *testing.T) {
	fakes := Build(t)
	tests := []struct {
		name     string
		vendor   string
		stdin    string
		args     []string
		wantCode int
	}{
		{"codex exec - with empty stdin", "codex", "", []string{"exec", "--json", "-"}, 2},
		{"codex exec resume - with empty stdin", "codex", "", []string{"exec", "resume", "t-1", "-"}, 2},
		{"codex exec - with a prompt on stdin", "codex", "task", []string{"exec", "--json", "-"}, 0},
		{"grok -p with no prompt", "grok", "", []string{"-p", "--output-format", "json"}, 2},
		{"grok -p with a prompt argument", "grok", "", []string{"-p", "task"}, 0},
		{"grok --single=value", "grok", "", []string{"--single=task"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, fakes)
			r := e.run(t, tt.vendor, "", tt.stdin, tt.args...)
			if r.code != tt.wantCode || (tt.wantCode == 2 && !strings.Contains(r.stderr, "no prompt") && !strings.Contains(r.stderr, "a value is required")) {
				t.Errorf("exit %d, stderr %q; want %d", r.code, r.stderr, tt.wantCode)
			}
		})
	}
}

// TestFakeGrokPromptForms (T6740): the fake fails both argv forms that
// failed live on grok 1.0.70, with clap's messages, and takes
// --prompt-file.
func TestFakeGrokPromptForms(t *testing.T) {
	fakes := Build(t)
	prompt := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(prompt, []byte("the task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "empty.md")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		stdin      string
		args       []string
		wantCode   int
		wantStderr string
		wantPrompt string
	}{
		{"-p then flags, prompt on stdin (aggy before T6740)", "the task", []string{"-p", "--output-format", "json"}, 2,
			"error: a value is required for '--single <PROMPT>' but none was supplied", ""},
		{"-p last, prompt on stdin", "the task", []string{"--output-format", "json", "-p"}, 2,
			"error: a value is required for '--single <PROMPT>' but none was supplied", ""},
		{"-p first, prompt last", "", []string{"-p", "--output-format", "json", "the task"}, 2,
			"error: a value is required for '--single <PROMPT>' but none was supplied", ""},
		{"-p with a value and a positional", "", []string{"-p", "x", "--output-format", "json", "the task"}, 2,
			"error: the argument '--single <PROMPT>' cannot be used with '[PROMPT]'", ""},
		{"--prompt-file", "", []string{"--prompt-file", prompt, "--output-format", "json"}, 0, "", "the task\n"},
		{"--prompt-file ignores stdin", "other", []string{"--prompt-file", prompt, "--output-format", "json"}, 0, "", "the task\n"},
		{"--prompt-file empty", "the task", []string{"--prompt-file", empty}, 2, "no prompt", ""},
		{"--prompt-file missing", "", []string{"--prompt-file", prompt + ".none"}, 2, "cannot read the prompt file", ""},
		{"--prompt-file with -p", "", []string{"--prompt-file", prompt, "-p", "x"}, 2, "not recorded", ""},
		{"-p value", "", []string{"-p", "hi", "--output-format", "json"}, 0, "", "hi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, fakes)
			r := e.run(t, "grok", "", tt.stdin, tt.args...)
			if r.code != tt.wantCode || !strings.Contains(r.stderr, tt.wantStderr) {
				t.Fatalf("exit %d, stderr %q; want %d and %q", r.code, r.stderr, tt.wantCode, tt.wantStderr)
			}
			if tt.wantCode == 0 {
				if recs := Records(t, e.record); len(recs) != 1 || recs[0].Mode != "headless" || recs[0].Prompt != tt.wantPrompt {
					t.Errorf("records = %+v", recs)
				}
			}
		})
	}
	// --version prints the real format: name, version, hash, channel.
	e := newEnv(t, fakes)
	if r := e.run(t, "grok", "", "", "--version"); r.stdout != "grok 1.0.70 (fakehash) [stable]\n" {
		t.Errorf("version = %q", r.stdout)
	}
	// Grok's --sandbox takes strict, not Codex's read-only.
	for _, tt := range []struct {
		v    string
		code int
	}{{"strict", 0}, {"read-only", 2}} {
		if r := e.run(t, "grok", "", "", "-p", "x", "--sandbox", tt.v); r.code != tt.code {
			t.Errorf("--sandbox %s: exit %d %q, want %d", tt.v, r.code, r.stderr, tt.code)
		}
	}
}

// TestFakesRefuseRealHomeByIdentity (M5): spellings of the real home that
// a name comparison misses are caught by file identity.
func TestFakesRefuseRealHomeByIdentity(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the firmlink and case-folding spellings are macOS file-system features")
	}
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skip("no user home")
	}
	real, err := os.Stat(u.HomeDir)
	if err != nil {
		t.Skip("no user home")
	}
	// alias returns p when it names the real home, else skips.
	alias := func(t *testing.T, p string) string {
		fi, err := os.Stat(p)
		if err != nil || !os.SameFile(fi, real) {
			t.Skipf("%s is not the real home on this machine", p)
		}
		return p
	}
	fakes := Build(t)
	tests := []struct {
		name string
		env  func(t *testing.T) []string
	}{
		{"HOME through the Data volume firmlink", func(t *testing.T) []string {
			return []string{"HOME=" + alias(t, "/System/Volumes/Data"+u.HomeDir)}
		}},
		{"CODEX_HOME through the Data volume firmlink", func(t *testing.T) []string {
			alias(t, "/System/Volumes/Data"+u.HomeDir)
			return []string{"CODEX_HOME=" + filepath.Join("/System/Volumes/Data"+u.HomeDir, ".codex")}
		}},
		{"HOME with a long s", func(t *testing.T) []string {
			if !strings.HasPrefix(u.HomeDir, "/Users/") {
				t.Skip("home is not under /Users")
			}
			return []string{"HOME=" + alias(t, "/Uſers/"+strings.TrimPrefix(u.HomeDir, "/Users/"))}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := tt.env(t)
			e := newEnv(t, fakes)
			e.vars = append(e.vars, env...)
			// --version writes nothing, even on a fake that wrongly runs.
			if r := e.run(t, "codex", "", "", "--version"); r.code != 97 {
				t.Errorf("exit %d (stderr %q), want 97", r.code, r.stderr)
			}
		})
	}
}
