package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/store"
)

// skillSrc isolates HOME, the state dir, and PATH, and writes a skill
// folder named name outside them.
func skillSrc(t *testing.T, name string) (src string) {
	t.Helper()
	isolate(t)
	t.Setenv("PATH", t.TempDir())
	src = filepath.Join(t.TempDir(), name)
	writeAt(t, filepath.Join(src, "SKILL.md"), "---\nname: "+name+"\ndescription: Says hi\n---\nSay hi.\n", 0o644)
	writeAt(t, filepath.Join(src, "scripts", "run.sh"), "echo hi\n", 0o755)
	return src
}

func skillRun(in string, tty bool, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := skillManage(args, strings.NewReader(in), tty, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func skillPin(t *testing.T, name string) (string, bool) {
	t.Helper()
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	d, ok, err := s.ToolSkill(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return d, ok
}

// TestSkillAdd: the preview shows the frontmatter, files, size, and
// sha256 before anything is stored; a yes at a terminal copies the
// folder, writes a skill-only definition (off), and records the pin,
// which aggy tool on then accepts and links.
func TestSkillAdd(t *testing.T) {
	src := skillSrc(t, "hello")
	state := os.Getenv(home.Env)
	dest := filepath.Join(state, "skills", "hello")

	// No answer: nothing stored.
	code, out, errOut := skillRun("n\n", true, "add", src)
	if code != 0 || !strings.Contains(out, "Nothing added") {
		t.Fatalf("no: %d %q %q", code, out, errOut)
	}
	for _, want := range []string{"frontmatter name: hello", "frontmatter description: Says hi", "2 files", "SKILL.md (", "scripts/run.sh (8 bytes)", "sha256", dest} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Lstat(dest); err == nil {
		t.Fatal("stored without a yes")
	}
	// No terminal, even with --yes: refused.
	if code, _, errOut := skillRun("", false, "add", src, "--yes"); code != 1 || !strings.Contains(errOut, "terminal") {
		t.Fatalf("no tty: %d %q", code, errOut)
	}

	if code, out, errOut := skillRun("y\n", true, "add", src); code != 0 || !strings.Contains(out, "Added hello (off)") {
		t.Fatalf("yes: %d %q %q", code, out, errOut)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "scripts", "run.sh")); err != nil || string(b) != "echo hi\n" {
		t.Fatalf("copy: %q %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(dest, "scripts", "run.sh")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("script mode: %v %v", fi.Mode(), err)
	}
	want, err := skillDigest(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := skillPin(t, "hello"); !ok || got != want {
		t.Errorf("pin = %q %v, want %q", got, ok, want)
	}
	if _, out, _ := toolRun("", false, "status", "hello"); !strings.Contains(statusLine(t, out, "hello"), "off") {
		t.Errorf("status:\n%s", out)
	}
	// Turned on, it links; the pin catches a later change.
	if code, _, errOut := toolRun("", false, "on", "hello", "--vendor", "claude"); code != 0 {
		t.Fatalf("on: %d %q", code, errOut)
	}
	if dst, err := os.Readlink(filepath.Join(os.Getenv("HOME"), ".claude", "skills", "hello")); err != nil || dst != dest {
		t.Errorf("link: %q %v", dst, err)
	}
	writeAt(t, filepath.Join(dest, "SKILL.md"), "changed", 0o600)
	if code, _, errOut := toolRun("", false, "on", "hello", "--vendor", "codex"); code != 1 || !strings.Contains(errOut, "tool.skill_changed") {
		t.Errorf("changed: %d %q", code, errOut)
	}

	// A clash, and --replace and remove while on.
	if code, _, errOut := skillRun("", true, "add", src, "--yes"); code != 1 || !strings.Contains(errOut, "tool.skill_exists") {
		t.Errorf("clash: %d %q", code, errOut)
	}
	if code, _, errOut := skillRun("", true, "add", src, "--yes", "--replace"); code != 1 || !strings.Contains(errOut, "tool.still_on") {
		t.Errorf("replace while on: %d %q", code, errOut)
	}
	if code, _, errOut := skillRun("", false, "remove", "hello"); code != 1 || !strings.Contains(errOut, "tool.still_on") || !strings.Contains(errOut, "aggy tool off hello") {
		t.Errorf("remove while on: %d %q", code, errOut)
	}
	if _, err := os.Lstat(dest); err != nil {
		t.Fatal("a refused remove removed the folder")
	}

	// Off, --replace stores the new folder and pins it.
	if code, _, errOut := toolRun("", false, "off", "hello"); code != 0 {
		t.Fatalf("off: %d %q", code, errOut)
	}
	writeAt(t, filepath.Join(src, "SKILL.md"), "---\nname: hello\ndescription: v2\n---\n", 0o644)
	if code, out, errOut := skillRun("", true, "add", src, "--yes", "--replace"); code != 0 || !strings.Contains(out, "replaces the folder") {
		t.Fatalf("replace: %d %q %q", code, out, errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "SKILL.md")); !strings.Contains(string(b), "v2") {
		t.Errorf("not replaced: %q", b)
	}
	want, _ = skillDigest(dest)
	if got, _ := skillPin(t, "hello"); got != want {
		t.Errorf("pin after replace = %q, want %q", got, want)
	}

	// Remove: folder, definition, and pin go.
	if code, _, errOut := skillRun("", false, "remove", "hello"); code != 0 {
		t.Fatalf("remove: %d %q", code, errOut)
	}
	for _, p := range []string{dest, filepath.Join(state, "tools", "hello.toml")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s stayed", p)
		}
	}
	if _, ok := skillPin(t, "hello"); ok {
		t.Error("the pin stayed")
	}
}

// TestSkillAddRefuses: symlinks at any depth, a folder over the size or
// entry cap, a missing SKILL.md, a built-in's name, aggy's own name, and
// a delegated child are refused, and nothing is stored.
func TestSkillAddRefuses(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, src string) []string // extra args
		want  string
	}{
		{"symlinked file", func(t *testing.T, src string) []string {
			secret := filepath.Join(t.TempDir(), "secret")
			writeAt(t, secret, "x", 0o600)
			if err := os.Symlink(secret, filepath.Join(src, "scripts", "ref")); err != nil {
				t.Fatal(err)
			}
			return nil
		}, "tool.skill_refused"},
		{"symlinked folder", func(t *testing.T, src string) []string {
			link := filepath.Join(t.TempDir(), "hello")
			if err := os.Symlink(src, link); err != nil {
				t.Fatal(err)
			}
			return []string{link}
		}, "tool.skill_refused"},
		{"hard link", func(t *testing.T, src string) []string {
			if err := os.Link(filepath.Join(src, "SKILL.md"), filepath.Join(t.TempDir(), "other")); err != nil {
				t.Fatal(err)
			}
			return nil
		}, "tool.skill_refused"},
		{"size cap", func(t *testing.T, src string) []string {
			writeAt(t, filepath.Join(src, "big"), strings.Repeat("x", maxSkillBytes), 0o600)
			return nil
		}, "tool.skill_refused"},
		{"entry cap", func(t *testing.T, src string) []string {
			for i := range maxSkillEntries {
				writeAt(t, filepath.Join(src, "many", strings.Repeat("a", 1+i/26)+string(rune('a'+i%26))), "", 0o600)
			}
			return nil
		}, "tool.skill_refused"},
		{"no SKILL.md", func(t *testing.T, src string) []string {
			if err := os.Remove(filepath.Join(src, "SKILL.md")); err != nil {
				t.Fatal(err)
			}
			return nil
		}, "tool.skill_refused"},
		{"built-in name", func(t *testing.T, src string) []string { return []string{src, "--name", "caveman"} }, "tool.skill_exists"},
		{"built-in without a skill", func(t *testing.T, src string) []string { return []string{src, "--name", "rtk", "--replace"} }, "tool.skill_exists"},
		{"aggy's own", func(t *testing.T, src string) []string { return []string{src, "--name", "aggy"} }, "usage"},
		{"delegated", func(t *testing.T, src string) []string {
			t.Setenv(borrow.Delegated, "1")
			return nil
		}, "tool.delegated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := skillSrc(t, "hello")
			args := tt.setup(t, src)
			if len(args) == 0 {
				args = []string{src}
			}
			code, _, errOut := skillRun("", true, append(append([]string{"add"}, args...), "--yes")...)
			if code == 0 || !strings.Contains(errOut, tt.want) {
				t.Errorf("%d %q, want %s", code, errOut, tt.want)
			}
			for _, n := range []string{"hello", "caveman", "rtk"} {
				if _, err := os.Lstat(filepath.Join(os.Getenv(home.Env), "skills", n)); err == nil {
					t.Errorf("stored %s", n)
				}
			}
		})
	}
}

// TestSkillAddBuiltIn: --replace stores the folder of a built-in that is
// only a skill, with no user definition, and pins it.
func TestSkillAddBuiltIn(t *testing.T) {
	src := skillSrc(t, "caveman")
	if code, _, errOut := skillRun("", true, "add", src, "--yes", "--replace"); code != 0 {
		t.Fatalf("add: %d %q", code, errOut)
	}
	if _, err := os.Lstat(filepath.Join(os.Getenv(home.Env), "tools", "caveman.toml")); err == nil {
		t.Error("a user definition shadows the built-in")
	}
	if _, ok := skillPin(t, "caveman"); !ok {
		t.Error("no pin")
	}
	if code, _, errOut := toolRun("", false, "on", "caveman", "--vendor", "claude"); code != 0 {
		t.Fatalf("on: %d %q", code, errOut)
	}
}
