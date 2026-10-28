package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/parity"
)

// memoryWorld is a git repo with a Claude memory folder at the derived
// path, holding two entries, the index, and a symlink.
func memoryWorld(t *testing.T) (repo, dir string) {
	t.Helper()
	h := isolate(t)
	repo = filepath.Join(h, "src", "my.repo")
	writeAt(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
	if err := os.MkdirAll(filepath.Join(repo, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := claudeMemoryDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(h, ".claude", "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(repo, "-"), "memory"); dir != want {
		t.Fatalf("dir = %s, want %s", dir, want)
	}
	writeAt(t, filepath.Join(dir, "MEMORY.md"), "- [Testing](feedback_testing.md)\n", 0o644)
	writeAt(t, filepath.Join(dir, "feedback_testing.md"), "---\nname: Testing\ndescription: d\ntype: feedback\n---\nRun make test\x1b[2J first.\n", 0o644)
	writeAt(t, filepath.Join(dir, "user_role.md"), "Go developer.\n", 0o644)
	writeAt(t, filepath.Join(h, "elsewhere.md"), "not memory\n", 0o644)
	if err := os.Symlink(filepath.Join(h, "elsewhere.md"), filepath.Join(dir, "linked.md")); err != nil {
		t.Fatal(err)
	}
	return repo, dir
}

func memoryRun(cwd, answer string, tty bool, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := memoryWith(args, cwd, strings.NewReader(answer), tty, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestMemoryImport(t *testing.T) {
	repo, _ := memoryWorld(t)
	mem := parity.MemoryPath(repo)
	want := "## Testing\n\nRun make test first.\n\n## user_role\n\nGo developer.\n"

	// Not a terminal and no --yes: preview only.
	code, out, errOut := memoryRun(repo, "", false, "import", "--from", "claude")
	if code != 0 || !strings.Contains(out, "+## Testing") || !strings.Contains(out, "Not written") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "skipped linked.md") || strings.Contains(out, "not memory") {
		t.Errorf("the symlinked entry was not skipped: %s\n%s", errOut, out)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("preview holds an escape: %q", out)
	}
	if _, err := os.Stat(mem); !os.IsNotExist(err) {
		t.Fatalf("preview wrote %s", mem)
	}
	// A terminal answering no writes nothing.
	if code, out, _ := memoryRun(repo, "n\n", true, "import", "--from", "claude"); code != 0 || !strings.Contains(out, "Nothing written") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(mem); !os.IsNotExist(err) {
		t.Fatalf("a no wrote %s", mem)
	}
	// Yes writes it, with the exclude line.
	if code, out, errOut := memoryRun(repo, "y\n", true, "import", "--from", "claude"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if b, _ := os.ReadFile(mem); string(b) != want {
		t.Errorf("memory.md = %q, want %q", b, want)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude")); string(b) != ".aggy/\n" {
		t.Errorf("exclude = %q", b)
	}
	// Again: nothing new to add.
	if code, out, _ := memoryRun(repo, "", false, "import", "--from", "claude", "--yes"); code != 0 || !strings.Contains(out, "# 0 files would change") {
		t.Errorf("repeat: exit %d\n%s", code, out)
	}
	// unwire takes it all back.
	mustRun(t, "unwire", "--all")
	if _, err := os.Stat(filepath.Join(repo, ".aggy")); !os.IsNotExist(err) {
		t.Errorf("unwire left .aggy: %v", err)
	}
}

func TestMemoryImportAppendsAndCaps(t *testing.T) {
	repo, dir := memoryWorld(t)
	mem := parity.MemoryPath(repo)
	writeAt(t, mem, "# Mine\n\n## Testing\n\nkept as is", 0o644)
	code, out, errOut := memoryRun(repo, "", false, "import", "--from", "claude", "--only", "feedback_testing,user_role", "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if b, _ := os.ReadFile(mem); string(b) != "# Mine\n\n## Testing\n\nkept as is\n\n## user_role\n\nGo developer.\n" {
		t.Errorf("memory.md = %q", b)
	}
	if code, _, errOut := memoryRun(repo, "", false, "import", "--from", "claude", "--only", "nope"); code != 1 || !strings.Contains(errOut, `no entry "nope"`) {
		t.Errorf("unknown entry: exit %d %s", code, errOut)
	}
	writeAt(t, filepath.Join(dir, "big.md"), strings.Repeat("x", parity.MemoryCap), 0o644)
	before, _ := os.ReadFile(mem)
	if code, _, errOut := memoryRun(repo, "", false, "import", "--from", "claude", "--only", "big", "--yes"); code != 1 || !strings.Contains(errOut, "over the 4096-byte cap") {
		t.Errorf("over the cap: exit %d %s", code, errOut)
	}
	if after, _ := os.ReadFile(mem); !bytes.Equal(before, after) {
		t.Error("an import over the cap changed memory.md")
	}
}

func TestMemoryImportRefusesSymlinks(t *testing.T) {
	repo, _ := memoryWorld(t)
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(repo, ".aggy")); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := memoryRun(repo, "", false, "import", "--from", "claude", "--yes"); code != 1 {
		t.Errorf("symlinked .aggy accepted: %s", errOut)
	}
	if _, err := os.Stat(filepath.Join(other, "memory.md")); !os.IsNotExist(err) {
		t.Error("wrote through a symlinked .aggy")
	}
}

func TestMemoryImportArgs(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{nil, {"import"}, {"import", "--from", "codex"}, {"import", "--from", "claude", "--yes", "--dry-run"}, {"export"}} {
		if code, _, _ := memoryRun(t.TempDir(), "", false, args...); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
	if code, _, errOut := memoryRun(t.TempDir(), "", false, "import", "--from", "claude"); code != 1 || !strings.Contains(errOut, "--dir") {
		t.Errorf("no memory folder: exit %d %s", code, errOut)
	}
}
