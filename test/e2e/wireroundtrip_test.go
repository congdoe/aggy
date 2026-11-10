//go:build unix

package e2e

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// snapTree is every file under root, but aggy's state, its binaries, and the
// fake's own files, with its hash and mode, a symlink with its target.
func snapTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == ".aggy" || rel == "bin" || strings.HasPrefix(rel, ".aggy/") || strings.HasPrefix(rel, "bin/") ||
			strings.HasSuffix(rel, "scenario.json") || strings.HasSuffix(rel, "record.jsonl") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			out[rel+"/"] = fi.Mode().String()
		case fi.Mode()&fs.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			out[rel] = "symlink->" + l
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("%x %s", sha256.Sum256(b), fi.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// diffSnap lists what differs between two snapTree results.
func diffSnap(a, b map[string]string) []string {
	var d []string
	for k, v := range a {
		if b[k] != v {
			d = append(d, fmt.Sprintf("changed or removed %s: %q -> %q", k, v, b[k]))
		}
	}
	for k, v := range b {
		if _, ok := a[k]; !ok {
			d = append(d, fmt.Sprintf("added %s: %q", k, v))
		}
	}
	sort.Strings(d)
	return d
}

// TestWireUnwireRoundTrip (T6966, PR #127 review): aggy setup --wire --dry-run
// changes nothing in HOME, and aggy setup --wire followed by the aggy unwire
// --run it names leaves HOME as it was: with no config, with settings of the
// person's own, with settings behind a symlink, and with a rules file of the
// person's own where aggy's would go.
func TestWireUnwireRoundTrip(t *testing.T) {
	for _, tc := range []string{"fresh", "existing-settings", "symlinked-settings", "other-rules"} {
		t.Run(tc, func(t *testing.T) {
			w := newWorld(t)
			_ = os.Remove(filepath.Join(w.home, ".codex", "rules", "aggy.rules"))
			write := func(p, content string, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), mode); err != nil {
					t.Fatal(err)
				}
			}
			settings := filepath.Join(w.home, ".claude", "settings.json")
			switch tc {
			case "existing-settings":
				write(settings, `{"model":"opus","hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo mine"}]}]}}`+"\n", 0o644)
			case "symlinked-settings":
				real := filepath.Join(w.home, "dotfiles", "settings.json")
				write(real, `{"model":"opus"}`+"\n", 0o644)
				if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, settings); err != nil {
					t.Fatal(err)
				}
			case "other-rules":
				write(filepath.Join(w.home, ".codex", "rules", "aggy.rules"), "# mine\n", 0o600)
			}
			env := []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
			before := snapTree(t, w.home)
			dry := exec.Command(buildAggy(t), "setup", "--wire", "--dry-run")
			dry.Dir, dry.Env = w.cwd, env
			if out, err := dry.CombinedOutput(); err != nil {
				t.Fatalf("dry run: %v\n%s", err, out)
			}
			if d := diffSnap(before, snapTree(t, w.home)); len(d) > 0 {
				t.Errorf("the dry run changed files: %v", d)
			}
			if _, err := os.Stat(filepath.Join(w.home, ".aggy", "aggy.db")); err == nil {
				t.Error("the dry run made the index")
			}
			c := exec.Command(buildAggy(t), "setup", "--wire")
			c.Dir, c.Env = w.cwd, env
			out, err := c.CombinedOutput()
			if err != nil {
				t.Fatalf("setup --wire: %v\n%s", err, out)
			}
			if d := diffSnap(before, snapTree(t, w.home)); len(d) == 0 {
				t.Fatalf("setup --wire changed nothing:\n%s", out)
			}
			m := regexp.MustCompile(`aggy unwire --run (\S+)`).FindSubmatch(out)
			if m == nil {
				t.Fatalf("setup --wire named no run to undo:\n%s", out)
			}
			u := w.start(t, map[string]any{}, "unwire", "--run", string(m[1]))
			if code := u.exit(); code != 0 {
				t.Fatalf("unwire exit %d\n%s", code, u.screen())
			}
			if d := diffSnap(before, snapTree(t, w.home)); len(d) > 0 {
				t.Errorf("after unwire, HOME differs from before: %v", d)
			}
		})
	}
}
