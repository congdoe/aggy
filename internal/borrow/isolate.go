package borrow

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// childDirs are one child's directories. Every child gets a fresh temp
// dir from the OS temp location (never under the state dir or the repo):
// run/ holds aggy's files for the run (the prompt and the files an
// adapter asks for), and work/, empty, is the working dir of every kind
// but review. Review runs read-only in the repo, as design section 7
// says: plan mode on Claude and Grok, --sandbox read-only on Codex.
type childDirs struct {
	root     string // the temp dir holding the others, removed afterwards
	run      string // aggy's own files for the run
	cwd      string // the child's working dir
	isolated bool   // cwd is a fresh, empty temp dir
	gitRepo  bool   // cwd is in a git work tree
}

func (d childDirs) remove() { _ = safefs.RemoveAll(d.root) }

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// resolve returns p with symlinks resolved, or p when it cannot be.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// TODO(T6737): check that the isolated cwd is outside any git work tree
// (a TMPDIR inside some other repo), and keep absolute --path values out
// of research and news prompts, which go to the web (review finding
// #10-13).
//
// makeDirs makes the child's directories for req.
func (b *Borrower) makeDirs(req Request) (childDirs, error) {
	root, err := safefs.MkdirTemp(b.TempDir, "aggy-borrow-")
	if err != nil {
		return childDirs{}, err
	}
	// The CLIs see the cwd with symlinks resolved (/var is /private/var
	// on macOS), so aggy names it the same way.
	root = resolve(root)
	d := childDirs{root: root, run: filepath.Join(root, "run"), cwd: filepath.Join(root, "work"), isolated: true}
	// A TMPDIR inside the repo or the state dir would let the child's
	// "empty" dir sit next to files it must not see.
	for _, bad := range []string{resolve(req.Repo), resolve(b.StateDir)} {
		if within(root, bad) {
			d.remove()
			return childDirs{}, fmt.Errorf("the temp dir %s is inside %s; set TMPDIR to a directory outside it", root, bad)
		}
	}
	for _, p := range []string{d.run, d.cwd} {
		if err := safefs.MkdirAll(p, 0o700); err != nil {
			d.remove()
			return childDirs{}, err
		}
	}
	if req.Kind == adapter.Review {
		d.cwd, d.isolated = req.Repo, false
		d.gitRepo = inGitRepo(req.Repo)
	}
	return d, nil
}

// inGitRepo reports whether dir or one of its parents holds .git.
func inGitRepo(dir string) bool {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := safefs.Lstat(filepath.Join(d, ".git")); err == nil {
			return true
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

// writeChildFiles writes the per-run files a adapter asks for into
// d.run. They are not vendor config: they live in the run's temp dir and
// go with it.
func writeChildFiles(a adapter.Adapter, req adapter.HeadlessRequest, run string) error {
	cf, ok := a.(adapter.ChildFiler)
	if !ok {
		return nil
	}
	files, err := cf.ChildFiles(req)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.Name == "" || f.Name != filepath.Base(f.Name) || strings.HasPrefix(f.Name, ".") {
			return fmt.Errorf("adapter asked for a per-run file named %q", f.Name)
		}
		if err := safefs.WriteFile(filepath.Join(run, f.Name), f.Data, 0o600); err != nil {
			return err
		}
	}
	return nil
}
