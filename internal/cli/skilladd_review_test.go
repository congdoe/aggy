package cli

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/home"
)

// answerAfter is a terminal that runs fn while the y/N question waits,
// then answers y.
type answerAfter struct {
	fn   func()
	done bool
}

func (h *answerAfter) Read(p []byte) (int, error) {
	if !h.done {
		h.done = true
		h.fn()
		return copy(p, "y\n"), nil
	}
	return 0, io.EOF
}

func skillAddWaiting(src string, fn func(), args ...string) (int, string, string) {
	var so, se strings.Builder
	code := skillManage(append([]string{"add", src}, args...), &answerAfter{fn: fn}, true, &so, &se)
	return code, so.String(), se.String()
}

// TestSkillAddForeignFolderKept (H1): a folder put at the store path
// while the question waits is refused and left alone; only this run's
// temp folder is cleaned up.
func TestSkillAddForeignFolderKept(t *testing.T) {
	src := skillSrc(t, "hello")
	skillsDir := filepath.Join(os.Getenv(home.Env), "skills")
	dest := filepath.Join(skillsDir, "hello")
	code, _, errOut := skillAddWaiting(src, func() { writeAt(t, filepath.Join(dest, "SKILL.md"), "someone else's\n", 0o600) })
	if code == 0 {
		t.Fatalf("added over a foreign folder: %q", errOut)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "SKILL.md")); err != nil || string(b) != "someone else's\n" {
		t.Errorf("the foreign folder was touched: %q %v", b, err)
	}
	des, _ := os.ReadDir(skillsDir)
	if len(des) != 1 {
		t.Errorf("leftovers in %s: %v", skillsDir, des)
	}
}

// TestSkillAddConcurrentOn (M1): a tool on in another shell while add
// --replace waits refuses the add, and the linked folder is untouched.
func TestSkillAddConcurrentOn(t *testing.T) {
	src := skillSrc(t, "hello")
	if code, _, errOut := skillRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatal(errOut)
	}
	dest := filepath.Join(os.Getenv(home.Env), "skills", "hello")
	before, _ := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	writeAt(t, filepath.Join(src, "SKILL.md"), "---\nname: hello\ndescription: v2\n---\n", 0o644)
	code, _, errOut := skillAddWaiting(src, func() {
		if code, _, e := toolRun("", false, "on", "hello", "--vendor", "claude"); code != 0 {
			t.Errorf("concurrent on: %q", e)
		}
	}, "--replace")
	if code == 0 || !strings.Contains(errOut, "tool.still_on") {
		t.Errorf("replace after a concurrent on: %d %q", code, errOut)
	}
	if after, _ := os.ReadFile(filepath.Join(dest, "SKILL.md")); string(after) != string(before) {
		t.Errorf("the linked folder changed: %q", after)
	}
}

// TestSkillAddReplaceFailsIntact (H1): a replace that fails after the
// swap (here the index cannot be written) puts back the old folder and
// definition, and leaves no temp entries.
func TestSkillAddReplaceFailsIntact(t *testing.T) {
	src := skillSrc(t, "hello")
	if code, _, errOut := skillRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatal(errOut)
	}
	state := os.Getenv(home.Env)
	dest := filepath.Join(state, "skills", "hello")
	def := filepath.Join(state, "tools", "hello.toml")
	oldSkill, _ := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	oldDef, _ := os.ReadFile(def)
	writeAt(t, filepath.Join(src, "SKILL.md"), "---\nname: hello\ndescription: v2\n---\n", 0o644)
	db := filepath.Join(state, "aggy.db")
	if err := os.Chmod(db, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(db, 0o600) })
	code, _, errOut := skillRun("", true, "add", src, "--yes", "--replace")
	if code == 0 {
		t.Skip("the index stayed writable at 0400 here; cannot force the failure")
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "SKILL.md")); string(b) != string(oldSkill) {
		t.Errorf("old folder not put back: %q (%s)", b, errOut)
	}
	if b, _ := os.ReadFile(def); string(b) != string(oldDef) {
		t.Errorf("old definition not put back: %q", b)
	}
	for _, d := range []string{filepath.Dir(dest), filepath.Dir(def)} {
		des, _ := os.ReadDir(d)
		for _, e := range des {
			if strings.HasPrefix(e.Name(), ".") {
				t.Errorf("leftover %s in %s", e.Name(), d)
			}
		}
	}
}

// TestSkillPinKeptByNoopOff (M2): an off that unlinks nothing keeps the
// add-time pin, so a changed folder is still refused by on.
func TestSkillPinKeptByNoopOff(t *testing.T) {
	src := skillSrc(t, "hello")
	if code, _, errOut := skillRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatal(errOut)
	}
	dest := filepath.Join(os.Getenv(home.Env), "skills", "hello")
	writeAt(t, filepath.Join(dest, "SKILL.md"), "---\nname: hello\n---\nEVIL\n", 0o600)
	if code, _, errOut := toolRun("", false, "off", "hello"); code != 0 {
		t.Fatal(errOut)
	}
	if _, ok := skillPin(t, "hello"); !ok {
		t.Error("a no-op off dropped the pin")
	}
	code, _, errOut := toolRun("", false, "on", "hello", "--vendor", "claude")
	if code == 0 || !strings.Contains(errOut, "tool.skill_changed") || !strings.Contains(errOut, "skill add") {
		t.Errorf("changed folder after a no-op off: %d %q", code, errOut)
	}
	// Re-adding pins it again.
	if code, _, errOut := skillRun("", true, "add", src, "--yes", "--replace"); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := toolRun("", false, "on", "hello", "--vendor", "claude"); code != 0 {
		t.Errorf("on after re-add: %q", errOut)
	}
}

// TestSkillPinKeptByUnlinkingOff (T6822): add, on, off (which unlinks
// the folder), edit, on refuses with tool.skill_changed and points at
// aggy skill add --replace, the only re-pin; aggy tool remove forgets
// the pin.
func TestSkillPinKeptByUnlinkingOff(t *testing.T) {
	src := skillSrc(t, "hello")
	if code, _, errOut := skillRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatal(errOut)
	}
	link := filepath.Join(os.Getenv("HOME"), ".claude", "skills", "hello")
	if code, _, errOut := toolRun("", false, "on", "hello", "--vendor", "claude"); code != 0 {
		t.Fatal(errOut)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("not linked: %v", err)
	}
	if code, _, errOut := toolRun("", false, "off", "hello"); code != 0 {
		t.Fatal(errOut)
	}
	if _, err := os.Lstat(link); err == nil {
		t.Fatal("off left the link")
	}
	if _, ok := skillPin(t, "hello"); !ok {
		t.Error("an unlinking off dropped the pin")
	}
	dest := filepath.Join(os.Getenv(home.Env), "skills", "hello")
	writeAt(t, filepath.Join(dest, "SKILL.md"), "---\nname: hello\n---\nEVIL\n", 0o600)
	code, _, errOut := toolRun("", false, "on", "hello", "--vendor", "claude")
	if code == 0 || !strings.Contains(errOut, "tool.skill_changed") || !strings.Contains(errOut, "aggy skill add") || !strings.Contains(errOut, "--replace") {
		t.Errorf("a folder changed while off: %d %q", code, errOut)
	}
	if _, err := os.Lstat(link); err == nil {
		t.Error("a refused on linked the folder")
	}
	if code, _, errOut := skillRun("", true, "add", src, "--yes", "--replace"); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := toolRun("", false, "on", "hello", "--vendor", "claude"); code != 0 {
		t.Errorf("on after the re-pin: %q", errOut)
	}
	if code, _, errOut := toolRun("", false, "off", "hello"); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := toolRun("", false, "remove", "hello"); code != 0 {
		t.Fatal(errOut)
	}
	if _, ok := skillPin(t, "hello"); ok {
		t.Error("aggy tool remove kept the pin")
	}
}

// TestSkillRemoveRefusedDef (M3): skill remove never deletes a refused
// or non-skill user definition.
func TestSkillRemoveRefusedDef(t *testing.T) {
	isolate(t)
	def := filepath.Join(os.Getenv(home.Env), "tools", "foo.toml")
	writeAt(t, def, "name = \"foo\"\ndescription = \"x\"\nversion = \"1\"\nscope = \"global\"\nbogus = 1\n", 0o600)
	code, _, errOut := skillRun("", false, "remove", "foo")
	if code == 0 || !strings.Contains(errOut, "aggy tool remove foo") {
		t.Errorf("remove: %d %q", code, errOut)
	}
	if _, err := os.Lstat(def); err != nil {
		t.Error("skill remove deleted a refused definition")
	}
}

// TestSkillRemoveSwappedFolder (T6820): a store folder swapped after
// the lookup is kept and the remove is refused (tool.skill_swapped): at
// the first check, before the definition is removed; or right before
// the folder is moved aside, after it.
func TestSkillRemoveSwappedFolder(t *testing.T) {
	for _, stage := range []string{"check", "aside"} {
		t.Run(stage, func(t *testing.T) {
			src := skillSrc(t, "hello")
			if code, _, errOut := skillRun("", true, "add", src, "--yes"); code != 0 {
				t.Fatal(errOut)
			}
			state := os.Getenv(home.Env)
			dest, def := filepath.Join(state, "skills", "hello"), filepath.Join(state, "tools", "hello.toml")
			testHookSkillRemove = func(s string) {
				if s != stage {
					return
				}
				if err := os.Rename(dest, dest+".old"); err != nil {
					t.Error(err)
				}
				writeAt(t, filepath.Join(dest, "keep"), "mine\n", 0o600)
			}
			t.Cleanup(func() { testHookSkillRemove = func(string) {} })
			code, _, errOut := skillRun("", false, "remove", "hello")
			if code == 0 || !strings.Contains(errOut, "changed since") || !strings.Contains(errOut, "tool.skill_swapped") {
				t.Errorf("remove: %d %q", code, errOut)
			}
			if _, err := os.Lstat(filepath.Join(dest, "keep")); err != nil {
				t.Errorf("the swapped-in folder was removed: %v", err)
			}
			if _, err := os.Lstat(def); (err == nil) != (stage == "check") {
				t.Errorf("definition after a refusal at %s: %v", stage, err)
			}
		})
	}
}

// TestSkillAddPreviewText (L4, L5, F): the preview escapes the
// frontmatter, warns on a name that differs or shadows, and says a
// definition is replaced.
func TestSkillAddPreviewText(t *testing.T) {
	src := skillSrc(t, "hello")
	writeAt(t, filepath.Join(src, "SKILL.md"), "---\nname: \x1b]0;pwn\x07hi\ndescription: \u202eabc \x1b[2J\n---\n", 0o644)
	_, out, _ := skillRun("n\n", true, "add", src)
	if strings.ContainsAny(out, "\x1b\x07\u202e") {
		t.Errorf("raw escape in the preview: %q", out)
	}
	if !strings.Contains(out, "warning: its frontmatter names it") {
		t.Errorf("no name warning:\n%s", out)
	}
	writeAt(t, filepath.Join(src, "SKILL.md"), "---\nname: caveman\n---\n", 0o644)
	_, out, _ = skillRun("n\n", true, "add", src)
	if !strings.Contains(out, "built-in tool caveman") {
		t.Errorf("no shadow warning:\n%s", out)
	}
	writeAt(t, filepath.Join(src, "SKILL.md"), "---\nname: hello\n---\n", 0o644)
	if code, _, errOut := skillRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatal(errOut)
	}
	_, out, _ = skillRun("n\n", true, "add", src, "--replace")
	if !strings.Contains(out, "replaces the user definition") || strings.Contains(out, "warning:") {
		t.Errorf("replace preview:\n%s", out)
	}
}

// TestToolStatusWritesNothing (T6726): status changes no file under
// HOME or the state dir, and adds or removes none. A folder's own mtime
// is not compared: the read-only index open, as on main before T6726,
// touches the state dir's.
func TestToolStatusWritesNothing(t *testing.T) {
	src := skillSrc(t, "hello")
	if code, _, errOut := skillRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := toolRun("", false, "on", "hello"); code != 0 {
		t.Fatal(errOut)
	}
	t.Chdir(t.TempDir())
	roots := []string{os.Getenv("HOME"), os.Getenv(home.Env)}
	snap := func() map[string]string {
		m := map[string]string{}
		for _, r := range roots {
			_ = filepath.WalkDir(r, func(p string, _ fs.DirEntry, err error) error {
				if fi, lerr := os.Lstat(p); err == nil && lerr == nil {
					m[p] = fi.Mode().String()
					if !fi.IsDir() {
						m[p] += " " + fi.ModTime().Format(time.RFC3339Nano)
					}
				}
				return nil
			})
		}
		return m
	}
	before := snap()
	time.Sleep(20 * time.Millisecond)
	if code, out, errOut := toolRun("", false, "status"); code != 0 {
		t.Fatalf("status: %d %s %s", code, out, errOut)
	}
	after := snap()
	for p, v := range after {
		if before[p] != v {
			t.Errorf("status changed %s", p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			t.Errorf("status removed %s", p)
		}
	}
}
