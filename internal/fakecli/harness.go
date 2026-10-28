package fakecli

import (
	"path/filepath"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// Harness is a temp HOME, working dir, prompt file, and record file for
// running adapter commands against the fakes.
type Harness struct {
	Fakes  Fakes
	Home   string // HOME for the fakes and the adapter's Getenv
	Cwd    string // an empty working dir, symlinks resolved
	Prompt string // a prompt file holding "the task"
	Record string // the fakes' record file
}

// NewHarness sets HOME to a temp dir for the test, clears the vendor home
// variables, and prepares a working dir and a prompt file.
func NewHarness(t *testing.T, fakes Fakes) *Harness {
	t.Helper()
	h := &Harness{Fakes: fakes, Home: t.TempDir(), Record: filepath.Join(t.TempDir(), "record.jsonl")}
	t.Setenv("HOME", h.Home)
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		t.Setenv(v, "")
	}
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.Cwd = cwd
	h.Prompt = filepath.Join(t.TempDir(), "prompt.md")
	if err := safefs.WriteFile(h.Prompt, []byte("the task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

// Getenv reads the harness's HOME and nothing else, for an adapter's
// Getenv field.
func (h *Harness) Getenv(k string) string {
	if k == "HOME" {
		return h.Home
	}
	return ""
}

// Run runs c against its fake with scenario (a JSON document, or "" for
// none) and returns the output and the run's record.
func (h *Harness) Run(t *testing.T, c adapter.Command, scenario string) (adapter.Output, Record) {
	t.Helper()
	env := []string{"HOME=" + h.Home, "PATH=/usr/bin:/bin", EnvRecord + "=" + h.Record}
	if scenario != "" {
		env = append(env, EnvScenario+"="+WriteScenario(t, scenario))
	}
	before := len(Records(t, h.Record))
	out := h.Fakes.Run(t, c, env)
	recs := Records(t, h.Record)
	var rec Record
	for _, r := range recs[before:] {
		if r.Vendor != "hook" {
			rec = r
		}
	}
	return out, rec
}
