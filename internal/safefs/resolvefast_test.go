package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolveExistingRef is resolveExisting as it was before it checked each
// prefix with lstat first (T6954).
func resolveExistingRef(p string) (string, error) {
	r, err := filepath.EvalSymlinks(p)
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	sep := string(filepath.Separator)
	end := len(p)
	for {
		i := strings.LastIndex(p[:end], sep)
		if i <= 0 {
			return filepath.Clean(p), nil
		}
		r, err := filepath.EvalSymlinks(p[:i])
		if err == nil {
			return filepath.Join(r, p[i+1:]), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		end = i
	}
}

// TestResolveExistingSame (T6954): the lstat-first walk, with and without a
// memo, answers as the old walk did, for paths that are there, missing at
// any depth, through links (dangling, to a file, looping, relative with ..),
// under a file, and with . and .. in them.
func TestResolveExistingSame(t *testing.T) {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"a/b", "real/sub"} {
		if err := os.MkdirAll(filepath.Join(d, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(d, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, l := range []struct{ name, to string }{
		{"toreal", "real"},
		{"dangling", "nowhere/x"},
		{"tofile", "file"},
		{"loop1", "loop2"},
		{"loop2", "loop1"},
		{"up", "a/b/../../real"},
	} {
		if err := os.Symlink(l.to, filepath.Join(d, l.name)); err != nil {
			t.Fatal(err)
		}
	}
	var paths []string
	for _, rel := range []string{
		"", "a", "a/b", "a/b/c", "a/b/c/d/e", "x/y/z", "toreal", "toreal/sub", "toreal/sub/new/deeper",
		"toreal/../a", "toreal/../a/missing", "dangling", "dangling/more", "tofile", "tofile/x", "file/x/y",
		"loop1", "loop1/x", "up/sub", "up/missing/x", "./a/./b/../b/new", "a/../x/../real/sub/n", "missing/../a",
	} {
		paths = append(paths, filepath.Join(d, rel), d+"/"+rel)
	}
	for _, p := range paths {
		want, werr := resolveExistingRef(p)
		for _, m := range []evalMemo{nil, {}} {
			got, gerr := resolveExistingMemo(p, m)
			if got != want || (gerr == nil) != (werr == nil) {
				t.Errorf("%s: got %q %v, want %q %v", p, got, gerr, want, werr)
			}
		}
	}
}
