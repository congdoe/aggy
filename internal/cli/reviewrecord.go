package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/store"
)

// How a review finds which vendors made the change it reviews from
// aggy's own record of edit borrows (T6747), rather than from the
// Aggy-Vendor trailers anyone can write.

// reviewDiff is the diff a review sees, and the one an edit's record is
// compared with: fixed a/ and b/ prefixes, no rename detection, and paths
// quoted only for a control character, quote, or backslash, whatever the
// repo's config says, so a file has the same header in both.
var reviewDiff = []string{"-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/"}

// editDiffArgs diffs recorded edits, many at once: git diff-tree reads
// "<base tree> <edit tree>" pairs on stdin and writes each pair's line
// before its diff, with full blob ids (--full-index) for the landed check.
var editDiffArgs = []string{"-c", "core.quotePath=false", "diff-tree", "-r", "-p", "-U0", "--full-index", "--no-color",
	"--no-ext-diff", "--no-textconv", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/", "--stdin"}

const (
	// minMatchLine is the fewest characters, spaces aside, an added line
	// needs to tie a recorded edit to a review.
	minMatchLine = 10
	// minMatches is how many distinct lines an edit must share with a
	// review to be in it, unless it has fewer.
	minMatches = 3
	// maxGrepBytes caps the lines one git grep looks for in the base tree.
	maxGrepBytes = 64 << 10
	// recordBudget bounds the time a review spends diffing recorded edits.
	recordBudget = 60 * time.Second
	// maxBatch caps the output of one git call over every recorded edit.
	maxBatch = 32 << 20
)

// diffLines is what a review compares in a diff: its added lines of at
// least minMatchLine characters, normalized (runs of space made one), with
// how often each is added and its first text as written; each file it
// touches, with whether the file has such a line; and, from a
// --full-index diff, the blob each file was left as ("" when deleted).
type diffLines struct {
	added map[string]int
	text  map[string]string
	files map[string]bool
	blobs map[string]string
}

func readDiff(diff string) diffLines {
	d := diffLines{added: map[string]int{}, text: map[string]string{}, files: map[string]bool{}, blobs: map[string]string{}}
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
		case !hunk && strings.HasPrefix(l, "index "):
			_, ids, _ := strings.Cut(l, "..")
			id, _, _ := strings.Cut(ids, " ")
			if strings.Trim(id, "0") == "" {
				id = ""
			}
			d.blobs[file] = id
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

// diffPath is the path a "diff --git a/<p> b/<p>" header names (both
// sides are the same path, as renames are off), or "".
func diffPath(h string) string {
	return pathField(strings.TrimPrefix(h, "diff --git "), true)
}

// pathField reads a path as git writes it with core.quotePath=false: as
// is, or C-quoted when it holds a control character, quote, or
// backslash. pair is a diff header's "a/<p> b/<p>"; "" when unreadable.
func pathField(s string, pair bool) string {
	if strings.HasPrefix(s, `"`) {
		q, err := strconv.QuotedPrefix(s)
		if err != nil {
			return ""
		}
		p, err := strconv.Unquote(q)
		if !pair {
			return p
		}
		if err != nil || !strings.HasPrefix(p, "a/") {
			return ""
		}
		return p[2:]
	}
	if !pair {
		return s
	}
	n := (len(s) - len("a/ b/")) / 2
	if n <= 0 || len(s) != 2*n+len("a/ b/") || !strings.HasPrefix(s, "a/") || s[2+n:] != " b/"+s[2:2+n] {
		return ""
	}
	return s[2 : 2+n]
}

// recordedEdit is one recorded edit and what a review learned of it.
type recordedEdit struct {
	r    store.EditRecord
	pair string // "<base tree> <edit tree>"
	d    diffLines
	done bool // d holds its whole diff
}

// recordAuthors returns the vendors aggy's own store record shows made an
// edit in the reviewed change, for the router to skip whatever the
// trailers say (T6747), and notes for the person. Every edit made in this
// repo (this work tree, or a linked worktree of the same repo) is
// compared, whatever happened to its commits since (a rebase, squash, or
// cherry-pick rewrites the commit it was made from, never its lines),
// with a fixed number of git processes: one cat-file for the trees, one
// diff-tree for every diff, one cat-file for the landed check, and one
// grep. An edit is in the change when editIn says so, or, when the
// reviewed diff was cut short, when the review touches a file of it the
// base does not hold as the edit left it. An edit git no longer has the
// objects of counts while its worktree is there; once aggy gc removed
// the worktree, git may prune them, and it does not count. An edit not
// diffed within recordBudget or maxBatch counts only when the review
// touches a file it touched. Each edit counted blind is named in a note.
func (e borrowEnv) recordAuthors(ctx context.Context, s *store.Store, root string, m *borrow.Material) (vs []adapter.Vendor, notes []string, err error) {
	recs, err := s.EditRecords(ctx)
	if err != nil || len(recs) == 0 {
		return nil, nil, err
	}
	common, croot := e.commonDir(ctx, root), canonicalPath(root)
	var edits []*recordedEdit
	for _, r := range recs {
		if _, err := adapter.ParseVendor(r.Vendor); err == nil && sameRepo(r.Worktree, croot, common) {
			edits = append(edits, &recordedEdit{r: r})
		}
	}
	if len(edits) == 0 {
		return nil, nil, nil
	}
	var counted []store.EditRecord
	note := func(why string, rs []store.EditRecord) {
		if len(rs) > 0 {
			notes = append(notes, why+": passing over "+editNames(rs))
			counted = append(counted, rs...)
		}
	}
	bctx, cancel := context.WithTimeout(ctx, recordBudget)
	defer cancel()
	missing := e.editTrees(bctx, root, edits)
	var unread, unlooked []store.EditRecord
	var live []*recordedEdit
	for _, x := range edits {
		switch {
		case x.pair != "":
			live = append(live, x)
		case !x.r.Worktree.Removed.IsZero():
			// Removed by aggy gc, so git may have pruned its objects.
		case missing[x]:
			unread = append(unread, x.r)
		default:
			unlooked = append(unlooked, x.r)
		}
	}
	note("git no longer has the objects of a recorded edit whose worktree is still there", unread)
	note("git could not look up the trees of recorded edits", unlooked)
	e.editDiffs(bctx, root, live)
	review := readDiff(m.Diff)
	rfiles, rerr := review.files, error(nil)
	if m.DiffTruncated {
		rfiles, rerr = e.reviewFiles(ctx, root, m.Base)
	}
	var undiffed []*recordedEdit
	var diffed []*recordedEdit
	for _, x := range live {
		if x.done {
			diffed = append(diffed, x)
		} else {
			undiffed = append(undiffed, x)
		}
	}
	if len(undiffed) > 0 {
		files, ferr := e.editFileLists(ctx, root, undiffed)
		var hit []store.EditRecord
		for _, x := range undiffed {
			f, ok := files[x.pair]
			if ferr != nil || rerr != nil || !ok || overlaps(f, rfiles) {
				hit = append(hit, x.r)
			}
		}
		note("aggy ran out of time or room diffing recorded edits, so it matched these by the files they touch", hit)
	}
	landed := e.landedFiles(ctx, root, m.Base, diffed)
	if m.DiffTruncated {
		var hit []store.EditRecord
		for _, x := range diffed {
			for f := range x.d.files {
				if !landed[x][f] && (rerr != nil || rfiles[f]) {
					hit = append(hit, x.r)
					break
				}
			}
		}
		note("the reviewed diff is over the size cap, so aggy matched its recorded edits by the files they touch", hit)
	} else {
		// Which edit lines the base tree holds: the ones the review adds
		// first, as they decide a match, then the rest.
		var first, rest []string
		text := map[string]string{}
		for _, x := range diffed {
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
		for _, x := range diffed {
			if editIn(x.d, review, inBase, landed[x]) {
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

// editIn reports whether the edit d is in the review. An edit that landed
// (every file it touched is in the base as it left it) is not. Otherwise
// it is when, of its distinctive lines (long added lines in no file of
// the base tree: common lines such as "if err != nil {" say nothing of
// who wrote them), at least minMatches, or all of them when it has fewer,
// are added exactly once by the review, in any file (code moved or
// pasted elsewhere still counts); when it has none, because each line it
// added is already in the base (a move or copy of code), the same test
// on all its long lines; or when the review touches a file of the edit
// with no long line (binary, short lines, or removals only) that has not
// landed.
func editIn(d, review diffLines, inBase, landed map[string]bool) bool {
	all := len(d.files) > 0
	for f := range d.files {
		all = all && landed[f]
	}
	if all {
		return false
	}
	k, n, ka, na := 0, 0, 0, 0
	for l := range d.added {
		hit := review.added[l] == 1
		ka++
		if hit {
			na++
		}
		if !inBase[l] {
			k++
			if hit {
				n++
			}
		}
	}
	if (k > 0 && n >= min(minMatches, k)) || (k == 0 && ka > 0 && na >= min(minMatches, ka)) {
		return true
	}
	for f, long := range d.files {
		if _, touched := review.files[f]; touched && !long && !landed[f] {
			return true
		}
	}
	return false
}

func overlaps(a, b map[string]bool) bool {
	for f := range a {
		if b[f] {
			return true
		}
	}
	return false
}

func editNames(rs []store.EditRecord) string {
	s := make([]string, len(rs))
	for i, r := range rs {
		s[i] = fmt.Sprintf("%s (borrow %s)", r.Vendor, r.BorrowID)
	}
	return strings.Join(s, ", ")
}

// isPairLine reports whether l is the "<tree> <tree>" line git diff-tree
// --stdin writes before each pair's output: no line of a diff or a file
// list is two object ids.
func isPairLine(l string) bool {
	a, b, ok := strings.Cut(l, " ")
	return ok && isObjectID(a) && isObjectID(b)
}

func isObjectID(s string) bool {
	return (len(s) == 40 || len(s) == 64) && strings.Trim(s, "0123456789abcdef") == ""
}

// editTrees sets each edit's pair from one git cat-file over its base
// commit's tree and its tree, and returns the edits git has no object of.
// When the call fails, it returns none and sets no pair.
func (e borrowEnv) editTrees(ctx context.Context, root string, edits []*recordedEdit) map[*recordedEdit]bool {
	missing := map[*recordedEdit]bool{}
	var asked []*recordedEdit
	var in strings.Builder
	for _, x := range edits {
		if !isObjectID(x.r.Worktree.Base) || !isObjectID(x.r.Worktree.Tree) {
			missing[x] = true
			continue
		}
		asked = append(asked, x)
		fmt.Fprintf(&in, "%s^{tree}\n%s^{tree}\n", x.r.Worktree.Base, x.r.Worktree.Tree)
	}
	if len(asked) == 0 {
		return missing
	}
	o, err := e.gitBatch(ctx, root, in.String(), "cat-file", "--batch-check")
	lines := strings.Split(strings.TrimSuffix(string(o.Stdout), "\n"), "\n")
	if err != nil || o.ExitCode != 0 || len(lines) != 2*len(asked) {
		return missing
	}
	for i, x := range asked {
		base, bt, _ := strings.Cut(lines[2*i], " ")
		tree, tt, _ := strings.Cut(lines[2*i+1], " ")
		if !isObjectID(base) || !isObjectID(tree) || !strings.HasPrefix(bt, "tree ") || !strings.HasPrefix(tt, "tree ") {
			missing[x] = true
			continue
		}
		x.pair = base + " " + tree
	}
	return missing
}

// editDiffs diffs every edit in one git diff-tree and reads each one's
// diff. An edit whose diff did not come back whole (out of time, over
// maxBatch, or git failed) is left not done.
func (e borrowEnv) editDiffs(ctx context.Context, root string, edits []*recordedEdit) {
	if len(edits) == 0 {
		return
	}
	var in strings.Builder
	for _, x := range edits {
		in.WriteString(x.pair + "\n")
	}
	o, err := e.gitBatch(ctx, root, in.String(), editDiffArgs...)
	whole := err == nil && o.ExitCode == 0 && len(o.Stdout) < maxBatch
	sections := pairSections(string(o.Stdout))
	for _, x := range edits {
		sec, ok := sections[x.pair]
		// The last section may be cut short unless the call ended well.
		if ok && (whole || sec.next) {
			x.d, x.done = readDiff(sec.text), true
		}
	}
}

type section struct {
	text string
	next bool // another pair's section followed it, so it is whole
}

// pairSections splits git diff-tree --stdin output by its pair lines.
func pairSections(out string) map[string]section {
	secs := map[string]section{}
	var pair string
	var b strings.Builder
	flush := func(next bool) {
		if pair != "" {
			secs[pair] = section{b.String(), next}
		}
		b.Reset()
	}
	for _, l := range strings.Split(out, "\n") {
		if isPairLine(l) {
			flush(true)
			pair = l
			continue
		}
		b.WriteString(l + "\n")
	}
	flush(false)
	return secs
}

// editFileLists lists the files each edit touched, in one git diff-tree
// --name-only, for the edits editDiffs could not diff whole.
func (e borrowEnv) editFileLists(ctx context.Context, root string, edits []*recordedEdit) (map[string]map[string]bool, error) {
	var in strings.Builder
	for _, x := range edits {
		in.WriteString(x.pair + "\n")
	}
	o, err := e.gitBatch(ctx, root, in.String(), "-c", "core.quotePath=false", "diff-tree", "-r", "--name-only", "--no-renames", "--stdin")
	if err == nil && (o.ExitCode != 0 || len(o.Stdout) >= maxBatch) {
		err = errors.New("git diff-tree could not list the files")
	}
	out := map[string]map[string]bool{}
	for pair, sec := range pairSections(string(o.Stdout)) {
		if !sec.next && err != nil {
			continue
		}
		files := map[string]bool{}
		for _, l := range strings.Split(sec.text, "\n") {
			if p := pathField(l, false); p != "" {
				files[p] = true
			}
		}
		out[pair] = files
	}
	return out, err
}

// landedFiles returns, per edit, which files it touched the base holds
// exactly as the edit left them (the same blob, or no file where it
// deleted one), from one git cat-file over "<base>:<path>". Any doubt is
// not landed.
func (e borrowEnv) landedFiles(ctx context.Context, root, base string, edits []*recordedEdit) map[*recordedEdit]map[string]bool {
	out := map[*recordedEdit]map[string]bool{}
	type ask struct {
		x    *recordedEdit
		path string
	}
	var asks []ask
	var in strings.Builder
	for _, x := range edits {
		out[x] = map[string]bool{}
		for _, f := range slices.Sorted(maps.Keys(x.d.files)) {
			if _, ok := x.d.blobs[f]; ok && !strings.ContainsAny(f, "\n\r") {
				asks = append(asks, ask{x, f})
				in.WriteString(base + ":" + f + "\n")
			}
		}
	}
	if len(asks) == 0 || !validRef(base) {
		return out
	}
	o, err := e.gitBatch(ctx, root, in.String(), "cat-file", "--batch-check")
	lines := strings.Split(strings.TrimSuffix(string(o.Stdout), "\n"), "\n")
	if err != nil || o.ExitCode != 0 || len(lines) != len(asks) {
		return out
	}
	for i, a := range asks {
		id, rest, _ := strings.Cut(lines[i], " ")
		want := a.x.d.blobs[a.path]
		switch {
		case want == "" && strings.HasSuffix(rest, "missing"):
			out[a.x][a.path] = true
		case want != "" && id == want && strings.HasPrefix(rest, "blob "):
			out[a.x][a.path] = true
		}
	}
	return out
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
// count as not in it, as does a line whose matches were cut off, or that
// the base holds only with other spacing inside it (grep's fixed string
// is the text as written, trimmed). git grep reads only git's own
// objects, with fixed strings and no textconv.
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

// gitBatch runs git in root as gitOutput does, with in as its stdin, on
// the batch runner, which keeps up to maxBatch bytes of output.
func (e borrowEnv) gitBatch(ctx context.Context, root, in string, args ...string) (adapter.Output, error) {
	if e.gitPath == nil || e.gitBatchRun == nil {
		return adapter.Output{}, errors.New("git: no batch runner")
	}
	path, err := e.gitPath()
	if err != nil {
		return adapter.Output{}, fmt.Errorf("git: %w", err)
	}
	return e.gitBatchRun(ctx, adapter.Command{Path: path, Args: append(slices.Clone(gitSafe), args...), Dir: root,
		Env: slices.Clone(gitSafeEnv), Stdin: adapter.StdinBytes, StdinData: []byte(in)})
}
