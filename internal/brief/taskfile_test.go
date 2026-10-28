package brief

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckTask(t *testing.T) {
	long := TaskTemplate() + strings.Repeat("- x\n", MaxTaskLines)
	for _, tc := range []struct {
		name, text string
		want       int
	}{
		{"template", TaskTemplate(), 0},
		{"missing", "## Goal\n## Done\n", 1},
		{"out of order", "## Done\n## Goal\n## Next\n## Decisions\n## Files touched\n## Open questions\n", 1},
		{"too long", long, 1},
	} {
		if got := CheckTask(tc.text); len(got) != tc.want {
			t.Errorf("%s: %q", tc.name, got)
		}
	}
}

func TestNeedsNudge(t *testing.T) {
	for _, tc := range []struct {
		turns   int
		changed bool
		want    bool
	}{{4, true, false}, {5, true, true}, {9, false, false}, {0, false, false}} {
		if got := NeedsNudge(tc.turns, tc.changed); got != tc.want {
			t.Errorf("NeedsNudge(%d, %v) = %v", tc.turns, tc.changed, got)
		}
	}
}

func TestReadTask(t *testing.T) {
	dir := t.TempDir()
	if got, err := ReadTask(dir); got != "" || err != nil {
		t.Fatalf("missing file: %q, %v", got, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".aggy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, TaskFile), []byte(TaskTemplate()), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadTask(dir); got != TaskTemplate() || err != nil {
		t.Fatalf("got %q, %v", got, err)
	}

	// A symlinked .aggy folder and a symlinked task file are refused.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "task.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkDir := t.TempDir()
	if err := os.Symlink(other, filepath.Join(linkDir, ".aggy")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTask(linkDir); err == nil {
		t.Error("read through a symlinked .aggy")
	}
	linkFile := t.TempDir()
	if err := os.MkdirAll(filepath.Join(linkFile, ".aggy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "task.md"), filepath.Join(linkFile, TaskFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTask(linkFile); err == nil {
		t.Error("read a symlinked task file")
	}
	// An oversized file is refused, not truncated.
	if err := os.WriteFile(filepath.Join(dir, TaskFile), []byte(strings.Repeat("x", maxTaskBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTask(dir); err == nil {
		t.Error("read an oversized task file")
	}
}
