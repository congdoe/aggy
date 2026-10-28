package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
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

// vendorHomeVars name the vendor config dirs set by environment.
var vendorHomeVars = []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"}

// vendorHomes are the dirs that hold vendor credentials and config: the
// defaults under home, ~/.agents, and any set by vendorHomeVars.
func vendorHomes(getenv func(string) string) []string {
	var out []string
	if h := getenv("HOME"); filepath.IsAbs(h) {
		for _, d := range []string{".claude", ".codex", ".grok", ".agents"} {
			out = append(out, filepath.Join(h, d))
		}
	}
	for _, k := range vendorHomeVars {
		if v := getenv(k); filepath.IsAbs(v) {
			out = append(out, v)
		}
	}
	return out
}

// reviewRoot returns the git work tree root a review runs in. It refuses
// a dir in no work tree, and a root that is /, is or holds $HOME, is,
// holds, or sits inside a vendor home (where credentials live), or is,
// holds, or sits inside the aggy state dir: the reviewer keeps its read
// tools there.
func reviewRoot(cwd string, getenv func(string) string, stateDir string) (string, error) {
	homeDir := getenv("HOME")
	root, ok := repoRoot(cwd)
	if !ok {
		return "", fmt.Errorf("a review runs in a git work tree, and %s is in none", cwd)
	}
	root = resolvePath(root)
	// Compare canonical forms: symlinks resolved and, where the file
	// system ignores case, case-folded, so <home>/.CODEX is <home>/.codex
	// (C17-9).
	croot, err := safefs.CanonicalPath(root)
	if err != nil {
		return "", fmt.Errorf("refusing to review %s: %w", root, err)
	}
	if croot == "/" {
		return "", fmt.Errorf("refusing to review %s: it is the file system root, which the reviewer could read in full", root)
	}
	// A root that is or holds the home dir would let the reviewer read
	// all of it, whatever AGGY_HOME says (#10-7).
	if h := canonicalPath(homeDir); h != "" && within(h, croot) {
		return "", fmt.Errorf("refusing to review %s: it is or holds your home directory, which the reviewer could read in full", root)
	}
	for _, vh := range vendorHomes(getenv) {
		if c := canonicalPath(vh); within(croot, c) || within(c, croot) {
			return "", fmt.Errorf("refusing to review %s: it is, holds, or is inside the vendor config dir %s, where credentials live", root, vh)
		}
	}
	if st := canonicalPath(stateDir); st != "" && (within(croot, st) || within(st, croot)) {
		return "", fmt.Errorf("refusing to review %s: it holds or is inside aggy's state dir %s", root, stateDir)
	}
	return root, nil
}

// canonicalPath is safefs.CanonicalPath, or "" for "" and the cleaned
// path when it cannot be canonicalized.
func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	if c, err := safefs.CanonicalPath(p); err == nil {
		return c
	}
	return filepath.Clean(p)
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
	out, err := e.gitOutput(ctx, root, args...)
	if err != nil {
		return "", false, err
	}
	if out.ExitCode != 0 {
		return "", false, fmt.Errorf("git %s: %s", subcommand(args), strings.TrimSpace(string(out.Stderr)))
	}
	return string(out.Stdout), len(out.Stdout) >= execrun.MaxOutput, nil
}

// subcommand is the git subcommand in args, past any -c options.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return args[i]
	}
	return ""
}

// gitOutput runs git in root with gitSafe and returns its whole output,
// whatever the exit code.
func (e borrowEnv) gitOutput(ctx context.Context, root string, args ...string) (adapter.Output, error) {
	if e.gitPath == nil || e.gitRun == nil {
		return adapter.Output{}, errors.New("git: no git runner")
	}
	path, err := e.gitPath()
	if err != nil {
		return adapter.Output{}, fmt.Errorf("git: %w", err)
	}
	return e.gitRun(ctx, adapter.Command{Path: path, Args: append(append([]string{}, gitSafe...), args...), Dir: root})
}

// filterOff returns the -c options that switch off every filter driver
// git's config defines for root (repo, user, system, and their
// includes). A diff of the work tree runs a driver's clean (or process)
// command on any stat-dirty file its attributes name, and attributes come
// from the repo itself (.gitattributes, .git/info/attributes), so the
// only safe way is to empty each defined driver. A driver name aggy
// cannot pass back to git -c is refused.
//
// pre are git options that come before config, such as a worktree's
// --git-dir and --work-tree.
func (e borrowEnv) filterOff(ctx context.Context, root string, pre ...string) ([]string, error) {
	out, err := e.gitOutput(ctx, root, append(slices.Clone(pre), "config", "-z", "--get-regexp", `^filter\.`)...)
	if err != nil {
		return nil, err
	}
	switch {
	case out.ExitCode == 1 && len(out.Stdout) == 0:
		return nil, nil // no filter driver is defined
	case out.ExitCode != 0:
		return nil, fmt.Errorf("git config: %s", strings.TrimSpace(string(out.Stderr)))
	case len(out.Stdout) >= execrun.MaxOutput:
		return nil, errors.New("git config: too many filter settings to switch off")
	}
	var opts []string
	seen := map[string]bool{}
	for _, entry := range strings.Split(string(out.Stdout), "\x00") {
		key, _, _ := strings.Cut(entry, "\n")
		if key == "" {
			continue
		}
		rest, ok := strings.CutPrefix(key, "filter.")
		dot := strings.LastIndexByte(rest, '.')
		if !ok || dot <= 0 {
			continue // filter.<var> with no driver name is no driver
		}
		name := rest[:dot]
		if strings.ContainsAny(name, "=\n\x00") {
			return nil, fmt.Errorf("refusing to review: git filter driver %q cannot be switched off", name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		for _, v := range []string{"clean=", "smudge=", "process=", "required=false"} {
			opts = append(opts, "-c", "filter."+name+"."+v)
		}
	}
	return opts, nil
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
// tree against base (committed and uncommitted changes to tracked files,
// with every filter driver switched off), and the files
// named by --path, read through safefs. It never runs anything but git,
// and git with gitSafe.
func (e borrowEnv) reviewMaterial(ctx context.Context, root, base string, paths []string) (*borrow.Material, error) {
	b, err := e.diffBase(ctx, root, base)
	if err != nil {
		return nil, err
	}
	off, err := e.filterOff(ctx, root)
	if err != nil {
		return nil, err
	}
	diff, cut, err := e.git(ctx, root, append(off, "diff", "--no-color", "--no-ext-diff", "--no-textconv", b, "--")...)
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
