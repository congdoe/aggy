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
// so an empty stdin is no prompt, and "grok -p" needs a prompt like the
// other headless CLIs.
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
		{"grok -p with a prompt on stdin", "grok", "task", []string{"-p"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, fakes)
			r := e.run(t, tt.vendor, "", tt.stdin, tt.args...)
			if r.code != tt.wantCode || (tt.wantCode == 2 && !strings.Contains(r.stderr, "no prompt")) {
				t.Errorf("exit %d, stderr %q; want %d", r.code, r.stderr, tt.wantCode)
			}
		})
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
