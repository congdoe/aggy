package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/safefs"
)

// gitTimeout bounds each git command a review runs.
const gitTimeout = 30 * time.Second

// gitSafe are the options every git command aggy runs takes: no
// fsmonitor, hook, pager, external diff, or textconv program from the
// reviewed repo's config runs. The repo is untrusted input.
var gitSafe = []string{
	"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "core.pager=cat",
	"-c", "diff.external=", "--no-pager",
}

// reviewRoot returns the git work tree root a review runs in. It refuses
// a dir in no work tree, and a root that is $HOME, /, the aggy state dir
// (or inside it), or that holds the state dir: the reviewer keeps its
// read tools there.
func reviewRoot(cwd, homeDir, stateDir string) (string, error) {
	root, ok := repoRoot(cwd)
	if !ok {
		return "", fmt.Errorf("a review runs in a git work tree, and %s is in none", cwd)
	}
	root = resolvePath(root)
	for _, bad := range []struct{ p, why string }{
		{"/", "the file system root"}, {resolvePath(homeDir), "your home directory"},
	} {
		if bad.p != "" && root == bad.p {
			return "", fmt.Errorf("refusing to review %s: it is %s, which the reviewer could read in full", root, bad.why)
		}
	}
	if st := resolvePath(stateDir); st != "" && (within(root, st) || within(st, root)) {
		return "", fmt.Errorf("refusing to review %s: it holds or is inside aggy's state dir %s", root, st)
	}
	return root, nil
}

func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validRef reports whether ref is safe to pass to git as a revision.
func validRef(ref string) bool {
	if ref == "" || strings.HasPrefix(ref, "-") || len(ref) > 200 {
		return false
	}
	return strings.Trim(ref, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._/~^@-") == ""
}

// git runs git in root with gitSafe and returns stdout. A non-zero exit
// is an error holding stderr.
func (e borrowEnv) git(ctx context.Context, root string, args ...string) (string, bool, error) {
	path, err := e.gitPath()
	if err != nil {
		return "", false, fmt.Errorf("git: %w", err)
	}
	out, err := e.gitRun(ctx, adapter.Command{Path: path, Args: append(append([]string{}, gitSafe...), args...), Dir: root})
	if err != nil {
		return "", false, err
	}
	if out.ExitCode != 0 {
		return "", false, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(out.Stderr)))
	}
	return string(out.Stdout), len(out.Stdout) >= execrun.MaxOutput, nil
}

// diffBase returns the merge-base of HEAD with the default branch
// (origin's HEAD, else main, else master), or base when the caller gave
// one.
func (e borrowEnv) diffBase(ctx context.Context, root, base string) (string, error) {
	if base != "" {
		if !validRef(base) {
			return "", fmt.Errorf("--base %q is not a git revision", base)
		}
		return base, nil
	}
	var branch string
	if out, _, err := e.git(ctx, root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		branch = strings.TrimSpace(out)
	}
	for _, b := range []string{branch, "main", "master"} {
		if !validRef(b) {
			continue
		}
		if out, _, err := e.git(ctx, root, "merge-base", "HEAD", b); err == nil {
			if mb := strings.TrimSpace(out); validRef(mb) {
				return mb, nil
			}
		}
	}
	return "", errors.New("no default branch (origin/HEAD, main, or master) to diff against; pass --base <ref>")
}

// maxFile caps one --path file read for a review.
const maxFile = borrow.MaxMaterial

// reviewMaterial gathers what a review child sees: the diff of the work
// tree against base (committed and uncommitted changes), and the files
// named by --path, read through safefs. It never runs anything but git,
// and git with gitSafe.
func (e borrowEnv) reviewMaterial(ctx context.Context, root, base string, paths []string) (*borrow.Material, error) {
	b, err := e.diffBase(ctx, root, base)
	if err != nil {
		return nil, err
	}
	diff, cut, err := e.git(ctx, root, "diff", "--no-color", "--no-ext-diff", "--no-textconv", b, "--")
	if err != nil {
		return nil, err
	}
	m := &borrow.Material{Base: b, Diff: diff, DiffTruncated: cut}
	if strings.TrimSpace(diff) == "" {
		m.Diff, m.Note = "", "the work tree has no changes against "+b
	}
	for _, p := range paths {
		f := borrow.MaterialFile{Path: p}
		if !within(resolvePath(p), root) {
			f.Note = "outside the repo"
			m.Files = append(m.Files, f)
			continue
		}
		data, err := safefs.ReadFileLimit(p, maxFile)
		switch {
		case err == nil:
			f.Data = string(data)
		case errors.Is(err, safefs.ErrNotRegular):
			f.Note = "not a regular file (a directory or a link)"
		case errors.Is(err, safefs.ErrTooLarge):
			f.Note = fmt.Sprintf("over %d bytes", maxFile)
		case errors.Is(err, fs.ErrNotExist):
			f.Note = "no such file"
		default:
			f.Note = "cannot be read: " + err.Error()
		}
		m.Files = append(m.Files, f)
	}
	return m, nil
}
