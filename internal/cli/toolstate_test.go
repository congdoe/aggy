package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/home"
)

// TestToolStatusColumns (T6726): status shows each tool installed,
// ready, and wired per vendor, reading only.
func TestToolStatusColumns(t *testing.T) {
	src, _ := wireHome(t)
	repo := switchRepo(t, "") // git is on PATH only until the next line
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	t.Chdir(t.TempDir())
	if code, _, errOut := toolRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatalf("add: %d %q", code, errOut)
	}
	line := func(name string) string {
		t.Helper()
		_, out, _ := toolRun("", false, "status", name)
		if !strings.Contains(out, "INSTALLED") || !strings.Contains(out, "READY") || !strings.Contains(out, "WIRED") {
			t.Errorf("no columns:\n%s", out)
		}
		return statusLine(t, out, name)
	}
	if got := line("demo"); !strings.HasPrefix(got, "demo off off off no: demo not on PATH - - ") {
		t.Errorf("off, missing: %q", got)
	}
	if code, _, errOut := toolRun("", false, "on", "demo", "--vendor", "claude"); code != 0 {
		t.Fatalf("on: %d %q", code, errOut)
	}
	if got := line("demo"); !strings.HasPrefix(got, "demo on off off no: demo not on PATH - claude no (tool.exec_refused) ") {
		t.Errorf("on, missing: %q", got)
	}
	writeAt(t, filepath.Join(bin, "demo"), "#!/bin/sh\n", 0o755)
	if got := line("demo"); !strings.HasPrefix(got, "demo on off off yes - claude ") {
		t.Errorf("on, installed: %q", got)
	}

	// A skill: missing, then pinned, then changed.
	if got := line("caveman"); !strings.HasPrefix(got, "caveman off off off no: skill folder no pin - ") {
		t.Errorf("caveman missing: %q", got)
	}
	skill := filepath.Join(t.TempDir(), "caveman")
	writeAt(t, filepath.Join(skill, "SKILL.md"), "---\nname: caveman\n---\n", 0o644)
	if code, _, errOut := skillRun("", true, "add", skill, "--yes", "--replace"); code != 0 {
		t.Fatalf("skill add: %d %q", code, errOut)
	}
	if got := line("caveman"); !strings.HasPrefix(got, "caveman off off off yes pin ok - ") {
		t.Errorf("caveman added: %q", got)
	}
	writeAt(t, filepath.Join(os.Getenv(home.Env), "skills", "caveman", "SKILL.md"), "changed", 0o600)
	if got := line("caveman"); !strings.HasPrefix(got, "caveman off off off yes pin changed - ") {
		t.Errorf("caveman changed: %q", got)
	}
	// CodeGraph's index: n/a outside a repo; in one, indexed only with
	// its database and aggy's marker.
	if got := line("codegraph"); !strings.Contains(got, "index n/a outside a repo") {
		t.Errorf("codegraph outside: %q", got)
	}
	t.Chdir(repo)
	writeAt(t, filepath.Join(repo, ".codegraph", "codegraph.db"), "db", 0o600)
	if got := line("codegraph"); !strings.Contains(got, "not indexed") {
		t.Errorf("codegraph without the marker: %q", got)
	}
	writeAt(t, filepath.Join(repo, ".codegraph", indexMarker), "x", 0o600)
	if got := line("codegraph"); !strings.Contains(got, " indexed") || strings.Contains(got, "not indexed") {
		t.Errorf("codegraph indexed: %q", got)
	}
}
