package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// entry is one path in a tree snapshot.
type entry struct {
	mode fs.FileMode
	data string // file bytes, or a symlink's target
}

// snapshot records every path under root with its type, permission bits,
// and content. Tests may use os directly.
func snapshot(t *testing.T, root string) map[string]entry {
	t.Helper()
	out := map[string]entry{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		e := entry{mode: fi.Mode()}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			e.data, err = os.Readlink(p)
		case fi.Mode().IsRegular():
			var b []byte
			b, err = os.ReadFile(p)
			e.data = string(b)
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = e
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// diffSnapshots lists every difference between two snapshots.
func diffSnapshots(a, b map[string]entry) []string {
	var out []string
	for p, ea := range a {
		eb, ok := b[p]
		switch {
		case !ok:
			out = append(out, "missing: "+p)
		case ea.mode != eb.mode:
			out = append(out, fmt.Sprintf("mode %s: %v -> %v", p, ea.mode, eb.mode))
		case ea.data != eb.data:
			out = append(out, fmt.Sprintf("content %s: %q -> %q", p, ea.data, eb.data))
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			out = append(out, "extra: "+p)
		}
	}
	sort.Strings(out)
	return out
}

func writeAt(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // past the umask
		t.Fatal(err)
	}
}
