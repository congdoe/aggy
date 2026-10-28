package parity

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
)

// ExcludeLine is the line aggy adds to a repo's git exclude file, so the
// per-repo .aggy folder never shows up as untracked.
const ExcludeLine = ".aggy/"

// maxGitFile caps the small git files read here (.git, commondir).
const maxGitFile = 4 << 10

// GitExcludePath returns the exclude file git reads for the repo whose
// working tree is root: what `git rev-parse --git-path info/exclude`
// prints, found without running git.
//
//   - root/.git is a directory (or a symlink to one): it is the git dir.
//   - root/.git is a file holding "gitdir: <path>", as in a linked
//     worktree or a submodule: that path, relative to root when it is
//     not absolute, is the git dir.
//   - The exclude file is shared by every worktree, so it lives in the
//     common dir: the path in <gitdir>/commondir (relative to the git
//     dir when not absolute), or the git dir itself when there is none.
//
// The result is <common dir>/info/exclude. The info directory may not
// exist. GIT_DIR and GIT_COMMON_DIR in the environment are not
// consulted.
//
// A .git file can name any directory, so the git dir must hold a HEAD
// file and the common dir a HEAD file and an objects directory, and
// neither info nor info/exclude may be a symlink. Otherwise it is an
// error and nothing is planned.
func GitExcludePath(root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	dotgit := filepath.Join(root, ".git")
	fi, err := safefs.Lstat(dotgit)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("parity: %s is not a git repository (no .git)", root)
		}
		return "", err
	}
	gitDir := dotgit
	switch {
	case fi.IsDir():
	case fi.Mode()&fs.ModeSymlink != 0:
		if fi, err = safefs.Stat(dotgit); err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("parity: %s links to something that is not a directory", dotgit)
		}
	case fi.Mode().IsRegular():
		b, err := safefs.ReadFileLimit(dotgit, maxGitFile)
		if err != nil {
			return "", err
		}
		line := strings.TrimSpace(firstLine(b))
		p, ok := strings.CutPrefix(line, "gitdir:")
		if !ok || strings.TrimSpace(p) == "" {
			return "", fmt.Errorf("parity: %s does not name a git dir", dotgit)
		}
		gitDir = absUnder(root, strings.TrimSpace(p))
	default:
		return "", fmt.Errorf("parity: %s is not a directory or a gitdir file", dotgit)
	}
	if err := looksLikeGitDir(gitDir, false); err != nil {
		return "", err
	}
	common := gitDir
	b, err := safefs.ReadFileLimit(filepath.Join(gitDir, "commondir"), maxGitFile)
	switch {
	case err == nil:
		if p := strings.TrimSpace(firstLine(b)); p != "" {
			common = absUnder(gitDir, p)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	if err := looksLikeGitDir(common, true); err != nil {
		return "", err
	}
	// Neither info nor the exclude file may be a symlink: the patch must
	// change the repo's own exclude file, never a file a link points at.
	info := filepath.Join(common, "info")
	exclude := filepath.Join(info, "exclude")
	if fi, err := safefs.Lstat(info); err == nil && !fi.IsDir() {
		return "", fmt.Errorf("parity: %s is not a directory (a symlink is refused)", info)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if fi, err := safefs.Lstat(exclude); err == nil && !fi.Mode().IsRegular() {
		return "", fmt.Errorf("parity: %s is not a regular file (a symlink is refused)", exclude)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return exclude, nil
}

// looksLikeGitDir refuses a directory that is not a git dir: it must hold
// a regular HEAD file and, for the common dir, an objects directory. A
// crafted .git file can name any directory, such as ~/.ssh or the home
// directory, and aggy must not create info/exclude there.
func looksLikeGitDir(dir string, common bool) error {
	notGit := func(why string) error {
		return fmt.Errorf("parity: %s does not look like a git dir: %s", dir, why)
	}
	fi, err := safefs.Lstat(dir)
	if err != nil {
		return notGit(err.Error())
	}
	if !fi.IsDir() {
		// A .git symlink was already resolved to a directory; a git dir
		// named by gitdir: or commondir must be a real directory.
		if fi.Mode()&fs.ModeSymlink == 0 {
			return notGit("not a directory")
		}
		if fi, err = safefs.Stat(dir); err != nil || !fi.IsDir() {
			return notGit("not a directory")
		}
	}
	if fi, err := safefs.Lstat(filepath.Join(dir, "HEAD")); err != nil || !fi.Mode().IsRegular() {
		return notGit("no HEAD file")
	}
	if common {
		if fi, err := safefs.Lstat(filepath.Join(dir, "objects")); err != nil || !fi.IsDir() {
			return notGit("no objects directory")
		}
	}
	return nil
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSuffix(s, "\r")
}

// absUnder returns p, made absolute against base when it is relative.
func absUnder(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// GitExcludePatch returns the patch that adds ".aggy/" to the git exclude
// file of the repo at root, never to .gitignore, guarded by the file's
// current hash. Apply it through a manifest, so aggy unwire removes the
// line again (or the file and the info directory, when aggy created
// them).
//
// The edit is idempotent: a file that already has the line (or "/.aggy/",
// ".aggy", or "/.aggy") is left alone. The line goes at the end, with the
// file's own line ending (CRLF when the file has any), after a line
// ending added to a last line that has none. A missing file is created,
// mode 0644 as git makes it.
//
// To commit the shared memory (.aggy/memory.md) instead of keeping it
// local, delete the line, or force-add the file (git add -f): exclude
// rules never apply to a tracked file. With the line deleted and nothing
// else edited, aggy unwire treats the change as already undone.
func GitExcludePatch(root string) (patch.Patch, error) {
	path, err := GitExcludePath(root)
	if err != nil {
		return patch.Patch{}, err
	}
	h, err := patch.HashFile(path)
	if err != nil {
		return patch.Patch{}, err
	}
	return patch.Patch{File: path, ExpectedHash: h, Transform: AddExcludeLine, Mode: 0o644}, nil
}

// AddExcludeLine is GitExcludePatch's transform. It gets nil for a file
// that does not exist.
func AddExcludeLine(src []byte) ([]byte, error) {
	if src == nil {
		return []byte(ExcludeLine + "\n"), nil
	}
	for _, l := range strings.Split(string(src), "\n") {
		switch strings.TrimSpace(l) {
		case ".aggy/", "/.aggy/", ".aggy", "/.aggy":
			return src, nil
		}
	}
	nl := eol(src)
	out := bytes.Clone(src)
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, nl...)
	}
	out = append(out, ExcludeLine...)
	return append(out, nl...), nil
}
