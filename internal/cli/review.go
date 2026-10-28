package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
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

// gitSafeEnv is the environment every git command aggy runs adds: a
// partial clone never fetches a missing object from its remote (git
// 2.45+; older git has no lazy-fetch switch and ignores it). The
// --no-lazy-fetch option is left out: git before 2.45 refuses it.
var gitSafeEnv = []string{"GIT_NO_LAZY_FETCH=1"}

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

// gitOutput runs git in root with gitSafe and gitSafeEnv and returns its
// whole output, whatever the exit code.
func (e borrowEnv) gitOutput(ctx context.Context, root string, args ...string) (adapter.Output, error) {
	if e.gitPath == nil || e.gitRun == nil {
		return adapter.Output{}, errors.New("git: no git runner")
	}
	path, err := e.gitPath()
	if err != nil {
		return adapter.Output{}, fmt.Errorf("git: %w", err)
	}
	return e.gitRun(ctx, adapter.Command{Path: path, Args: append(append([]string{}, gitSafe...), args...), Dir: root, Env: slices.Clone(gitSafeEnv)})
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
// compared with: fixed a/ and b/ prefixes, no rename detection, and paths
// quoted only for a control character, quote, or backslash, whatever the
// repo's config says, so a file has the same header in both.
var reviewDiff = []string{"-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/"}

const (
	// minMatchLine is the fewest characters, spaces aside, an added line
	// needs to tie a recorded edit to a review.
	minMatchLine = 10
	// minMatches is how many distinct lines an edit must share with a
	// review to be in it, unless it has fewer distinctive ones.
	minMatches = 3
	// maxEdits caps the recorded edits a review diffs, newest first.
	maxEdits = 50
	// maxGrepBytes caps the lines one git grep looks for in the base tree.
	maxGrepBytes = 64 << 10
)

// diffLines is what a review compares in a reviewDiff diff: its added
// lines of at least minMatchLine characters, normalized (runs of space
// made one), with how often each is added and its first text as written,
// and each file it touches, with whether the file has such a line.
// Removed lines are left out: every one of them is in the base tree.
type diffLines struct {
	added map[string]int
	text  map[string]string
	files map[string]bool
}

func readDiff(diff string) diffLines {
	d := diffLines{added: map[string]int{}, text: map[string]string{}, files: map[string]bool{}}
	file, hunk := "", false
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			file, hunk = diffPath(l), false
			if file != "" && !d.files[file] {
				d.files[file] = false
			}
		case file == "":
		case strings.HasPrefix(l, "@@"):
			hunk = true // no line inside a hunk starts with @@
		case hunk && strings.HasPrefix(l, "+"):
			f := strings.Fields(l[1:])
			if len(strings.Join(f, "")) < minMatchLine {
				continue
			}
			n := strings.Join(f, " ")
			if d.added[n]++; d.added[n] == 1 {
				d.text[n] = strings.TrimSpace(l[1:])
			}
			d.files[file] = true
		}
	}
	return d
}

// diffPath is the path a reviewDiff "diff --git a/<p> b/<p>" header
// names (both sides are the same path, as renames are off), or "".
func diffPath(h string) string {
	rest := strings.TrimPrefix(h, "diff --git ")
	if strings.HasPrefix(rest, `"`) {
		q, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return ""
		}
		p, err := strconv.Unquote(q)
		if err != nil || !strings.HasPrefix(p, "a/") {
			return ""
		}
		return p[2:]
	}
	n := (len(rest) - len("a/ b/")) / 2
	if n <= 0 || len(rest) != 2*n+len("a/ b/") || !strings.HasPrefix(rest, "a/") || rest[2+n:] != " b/"+rest[2:2+n] {
		return ""
	}
	return rest[2 : 2+n]
}

// recordAuthors returns the vendors aggy's own store record shows made an
// edit in the reviewed change, for the router to skip whatever the
// trailers say (T6747), and notes for the person. It considers the edits
// made in this repo (this work tree, or a linked worktree of the same
// repo), whatever the commits since: a rebase, squash, or cherry-pick
// rewrites them, never the edit's lines. The newest maxEdits are
// diffed; any older ones count. An edit is in the change when editIn
// says so, or, when the reviewed diff was cut short, when the review
// touches a file of it that the base does not already hold as the edit
// left it. An edit whose lines are all in the base tree landed before
// the base (or holds only common lines), so it does not count. It fails
// closed: an edit git cannot diff counts, and so does every edit when
// the touched files cannot be listed.
func (e borrowEnv) recordAuthors(ctx context.Context, s *store.Store, root string, m *borrow.Material) (vs []adapter.Vendor, notes []string, err error) {
	recs, err := s.EditRecords(ctx)
	if err != nil || len(recs) == 0 {
		return nil, nil, err
	}
	common, croot := e.commonDir(ctx, root), canonicalPath(root)
	var edits []store.EditRecord
	for _, r := range recs {
		if _, err := adapter.ParseVendor(r.Vendor); err == nil && sameRepo(r.Worktree, croot, common) {
			edits = append(edits, r)
		}
	}
	var counted []store.EditRecord
	note := func(why string, rs []store.EditRecord) {
		if len(rs) > 0 {
			notes = append(notes, why+": passing over "+editNames(rs))
			counted = append(counted, rs...)
		}
	}
	if len(edits) > maxEdits {
		note(fmt.Sprintf("this repo has more than %d recorded edits, and the older ones were not compared", maxEdits), edits[maxEdits:])
		edits = edits[:maxEdits]
	}
	type diffed struct {
		r store.EditRecord
		d diffLines
	}
	var ds []diffed
	var unread []store.EditRecord
	for _, r := range edits {
		d, err := e.editLines(ctx, root, r.Worktree)
		if err != nil {
			unread = append(unread, r)
			continue
		}
		ds = append(ds, diffed{r, d})
	}
	note("git could not give the diff of a recorded edit", unread)
	var review diffLines
	if !m.DiffTruncated {
		review = readDiff(m.Diff)
	}
	// Which edit lines the base tree holds: the ones the review adds
	// first, as they decide a match, then the rest.
	var first, rest []string
	text := map[string]string{}
	for _, x := range ds {
		for l, t := range x.d.text {
			if _, ok := text[l]; ok {
				continue
			}
			text[l] = t
			if review.added[l] == 1 {
				first = append(first, l)
			} else {
				rest = append(rest, l)
			}
		}
	}
	slices.Sort(first)
	slices.Sort(rest)
	inBase := e.inBase(ctx, root, m.Base, append(first, rest...), text)
	if m.DiffTruncated {
		files, err := e.reviewFiles(ctx, root, m.Base)
		var hit []store.EditRecord
		for _, x := range ds {
			if err != nil {
				hit = append(hit, x.r)
				continue
			}
			if landed(x.d, inBase) {
				continue
			}
			var touched []string
			for f := range x.d.files {
				if files[f] {
					touched = append(touched, f)
				}
			}
			if len(touched) > 0 && !e.filesLanded(ctx, root, m.Base, x.r.Worktree, touched) {
				hit = append(hit, x.r)
			}
		}
		note("the reviewed diff is over the size cap, so aggy matched its recorded edits by the files they touch", hit)
	} else {
		for _, x := range ds {
			in, files := editIn(x.d, review, inBase)
			if in || (len(files) > 0 && !e.filesLanded(ctx, root, m.Base, x.r.Worktree, files)) {
				counted = append(counted, x.r)
			}
		}
	}
	for _, r := range counted {
		if v, _ := adapter.ParseVendor(r.Vendor); !slices.Contains(vs, v) {
			vs = append(vs, v)
		}
	}
	return vs, notes, nil
}

// landed reports whether every long line the edit d added is in the base
// tree: the edit landed before the base, or holds only common lines.
func landed(d diffLines, inBase map[string]bool) bool {
	for l := range d.added {
		if !inBase[l] {
			return false
		}
	}
	return len(d.added) > 0
}

// editIn reports whether the edit d is in the review by its lines: of its
// distinctive lines (long added lines in no file of the base tree, since
// common lines such as "if err != nil {" say nothing of who wrote them),
// at least minMatches, or all of them when it has fewer, are added
// exactly once by the review, in any file (code moved or pasted
// elsewhere still counts). An edit that landed has none. Otherwise it
// returns the files of the edit with no long line (binary, short lines,
// or removals only) that the review touches, for filesLanded.
func editIn(d, review diffLines, inBase map[string]bool) (bool, []string) {
	if landed(d, inBase) {
		return false, nil
	}
	dist, n := 0, 0
	for l := range d.added {
		if !inBase[l] {
			dist++
			if review.added[l] == 1 {
				n++
			}
		}
	}
	if dist > 0 && n >= min(minMatches, dist) {
		return true, nil
	}
	var files []string
	for f, long := range d.files {
		if _, touched := review.files[f]; touched && !long {
			files = append(files, f)
		}
	}
	slices.Sort(files)
	return false, files
}

// filesLanded reports whether the base already holds files exactly as
// the edit in wt left them. Any doubt (a git error) is no.
func (e borrowEnv) filesLanded(ctx context.Context, root, base string, wt store.Worktree, files []string) bool {
	if !validRef(base) || !validRef(wt.Tree) {
		return false
	}
	args := append([]string{"--literal-pathspecs", "diff", "--quiet", "--no-ext-diff", "--no-textconv", "--no-renames", base, wt.Tree, "--"}, files...)
	o, err := e.gitOutput(ctx, root, args...)
	return err == nil && o.ExitCode == 0
}

func editNames(rs []store.EditRecord) string {
	s := make([]string, len(rs))
	for i, r := range rs {
		s[i] = fmt.Sprintf("%s (borrow %s)", r.Vendor, r.BorrowID)
	}
	return strings.Join(s, ", ")
}

// commonDir is the canonical common git dir of the work tree at root, or
// "" when git cannot say.
func (e borrowEnv) commonDir(ctx context.Context, root string) string {
	out, _, err := e.git(ctx, root, "rev-parse", "--git-common-dir")
	c := strings.TrimSpace(out)
	if err != nil || c == "" {
		return ""
	}
	if !filepath.IsAbs(c) {
		c = filepath.Join(root, c)
	}
	return canonicalPath(c)
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

// editLines diffs the edit recorded in wt from its base commit to its
// tree. git diffs two of its own objects in root, which shares the
// edit's object store, so no work tree file and no filter is read. A
// diff cut short is an error.
func (e borrowEnv) editLines(ctx context.Context, root string, wt store.Worktree) (diffLines, error) {
	if !validRef(wt.Base) || !validRef(wt.Tree) {
		return diffLines{}, errors.New("no base or tree recorded")
	}
	out, cut, err := e.git(ctx, root, append(slices.Clone(reviewDiff), "-U0", wt.Base, wt.Tree, "--")...)
	if err == nil && cut {
		err = errors.New("the diff is too large")
	}
	return readDiff(out), err
}

// reviewFiles lists the files the reviewed diff touches (the work tree
// against base, filters off), for a review whose diff was cut short.
func (e borrowEnv) reviewFiles(ctx context.Context, root, base string) (map[string]bool, error) {
	off, err := e.filterOff(ctx, root)
	if err != nil {
		return nil, err
	}
	out, cut, err := e.git(ctx, root, append(off, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", base, "--")...)
	if err == nil && cut {
		err = errors.New("too many files to list")
	}
	files := map[string]bool{}
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files[f] = true
		}
	}
	return files, err
}

// inBase returns which of lines (diffLines keys, with their text) are a
// whole line of some file of the base tree, compared normalized: git grep
// finds the files' lines that hold each text, and only a line equal to it
// counts. Lines past maxGrepBytes, or all of them when git grep fails,
// count as not in it (fail closed), as does a line whose matches were cut
// off, or that the base holds only with other spacing inside it (grep's
// fixed string is the text as written, trimmed). git grep reads only git's own objects, with fixed strings and no
// textconv.
func (e borrowEnv) inBase(ctx context.Context, root, base string, lines []string, text map[string]string) map[string]bool {
	found := map[string]bool{}
	if len(lines) == 0 || !validRef(base) {
		return found
	}
	args := []string{"-c", "grep.lineNumber=false", "-c", "grep.column=false", "-c", "grep.fullName=false",
		"grep", "-I", "-F", "-h", "--no-color"}
	size := 0
	for _, l := range lines {
		if size += len(text[l]); size > maxGrepBytes {
			break
		}
		args = append(args, "-e", text[l])
	}
	o, err := e.gitOutput(ctx, root, append(args, base, "--")...)
	if err != nil || o.ExitCode != 0 {
		return found
	}
	want := map[string]bool{}
	for _, l := range lines {
		want[l] = true
	}
	for _, s := range strings.Split(string(o.Stdout), "\n") {
		if n := strings.Join(strings.Fields(s), " "); want[n] {
			found[n] = true
		}
	}
	return found
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
