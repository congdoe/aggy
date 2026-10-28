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
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
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

// maxAuthorCommits caps the commits whose trailers a review reads.
const maxAuthorCommits = 1000

// reviewAuthors returns the vendors the Aggy-Vendor trailers of the
// commits in base..HEAD name, in first-seen order. A trailer is anyone's
// to write, so the result only steers routing; a value that is not a
// vendor is ignored, and any git failure gives none (the normal route).
// git runs with gitSafe and without signature checks, so no program from
// the repo's config runs.
//
// Trailers are best-effort and forgeable: anyone can commit with
// AGGY_VENDOR set, or skip the hook and write one by hand, and an
// accepted edit is committed under the on-screen vendor's AGGY_VENDOR,
// not the edit's. So they are a weak hint beside aggy's own record
// (recordAuthors), which the router lets them add to, never override.
func (e borrowEnv) reviewAuthors(ctx context.Context, root, base string) []adapter.Vendor {
	if !validRef(base) {
		return nil
	}
	out, _, err := e.git(ctx, root, "log", "--no-show-signature", fmt.Sprintf("--max-count=%d", maxAuthorCommits),
		"--format=%(trailers:key="+parity.TrailerVendor+",valueonly)", base+"..HEAD", "--")
	if err != nil {
		return nil
	}
	var vs []adapter.Vendor
	for _, l := range strings.Split(out, "\n") {
		v, err := adapter.ParseVendor(strings.TrimSpace(l))
		if err == nil && !slices.Contains(vs, v) {
			vs = append(vs, v)
		}
	}
	return vs
}

// reviewDiff is the diff a review sees, and the one an edit's record is
// compared with: fixed a/ and b/ prefixes and no rename detection,
// whatever the repo's config says, so a file has the same header in both.
var reviewDiff = []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/"}

// minMatchLine is the fewest characters, spaces aside, a changed line
// needs to tie a recorded edit to a review: shorter lines ("}", "return
// err") are in too many changes to say who wrote them.
const minMatchLine = 10

// changedLines reads a unified diff and returns, per file (its diff --git
// header line), its added and removed lines of at least minMatchLine
// characters, each as its sign and its text with runs of space made one.
// A file with no such line (binary, or short lines only) has an empty set.
func changedLines(diff string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	var file map[string]bool
	hunk := false
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			file, hunk = map[string]bool{}, false
			out[l] = file
		case file == nil:
		case strings.HasPrefix(l, "@@"):
			hunk = true // no line inside a hunk starts with @@
		case hunk && (strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-")):
			if f := strings.Fields(l[1:]); len(strings.Join(f, "")) >= minMatchLine {
				file[l[:1]+strings.Join(f, " ")] = true
			}
		}
	}
	return out
}

// recordAuthors returns the vendors aggy's own store record shows made an
// edit in the reviewed change, for the router to skip whatever the
// trailers say (T6747). Edits made in this repo (this work tree, or a
// linked worktree of the same repo) are compared with the reviewed diff:
// an edit is in it when one of its changed lines (changedLines) is changed
// the same way in the same file, or when the review touches a file of
// the edit that has no such line (binary, or short lines only). It
// fails closed: when the
// reviewed diff was cut short, or git cannot give an edit's diff while
// its worktree is still there, the edit counts. An edit whose worktree
// aggy gc removed and whose objects git has since pruned no longer
// counts, and neither does one aggy gc pruned from the index.
func (e borrowEnv) recordAuthors(ctx context.Context, s *store.Store, root string, m *borrow.Material) ([]adapter.Vendor, error) {
	recs, err := s.EditRecords(ctx)
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	common := ""
	if out, _, err := e.git(ctx, root, "rev-parse", "--git-common-dir"); err == nil && strings.TrimSpace(out) != "" {
		c := strings.TrimSpace(out)
		if !filepath.IsAbs(c) {
			c = filepath.Join(root, c)
		}
		common = canonicalPath(c)
	}
	croot := canonicalPath(root)
	review := changedLines(m.Diff)
	var vs []adapter.Vendor
	for _, r := range recs {
		v, err := adapter.ParseVendor(r.Vendor)
		if err != nil || slices.Contains(vs, v) || !sameRepo(r.Worktree, croot, common) {
			continue
		}
		if m.DiffTruncated || e.editInReview(ctx, root, r.Worktree, review) {
			vs = append(vs, v)
		}
	}
	return vs, nil
}

// sameRepo reports whether wt was made from the work tree root, or from
// any work tree of the repo whose common git dir is common. Both are
// canonical paths; common may be "".
func sameRepo(wt store.Worktree, root, common string) bool {
	if canonicalPath(wt.Repo) == root {
		return true
	}
	// A linked worktree's admin dir is <common dir>/worktrees/<name>.
	admin := filepath.Dir(wt.GitDir)
	return common != "" && filepath.IsAbs(wt.GitDir) && filepath.Base(admin) == "worktrees" && canonicalPath(filepath.Dir(admin)) == common
}

// editInReview reports whether the edit recorded in wt is in review, the
// reviewed diff's changedLines (see recordAuthors). git diffs two of its
// own objects in root, which share the edit's object store, so no work
// tree file and no filter is read.
func (e borrowEnv) editInReview(ctx context.Context, root string, wt store.Worktree, review map[string]map[string]bool) bool {
	if !validRef(wt.Base) || !validRef(wt.Tree) {
		return wt.Removed.IsZero()
	}
	out, cut, err := e.git(ctx, root, append(slices.Clone(reviewDiff), "-U0", wt.Base, wt.Tree, "--")...)
	switch {
	case err != nil:
		return wt.Removed.IsZero()
	case cut:
		return true
	}
	for f, ls := range changedLines(out) {
		if _, touched := review[f]; touched && len(ls) == 0 {
			return true
		}
		for l := range ls {
			if review[f][l] {
				return true
			}
		}
	}
	return false
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
	diff, cut, err := e.git(ctx, root, append(append(off, reviewDiff...), b, "--")...)
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
