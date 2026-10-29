package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
	"github.com/congdoe/aggy/internal/toolwire"
)

// toolIndex is a built-in's per-repo index (T6711): the folder its
// prebuild writes under the work-tree root, its database (the file an
// index has), and the files a worktree's copy takes, the database last.
type toolIndex struct {
	dir, db string
	copy    []string
}

// toolIndexes are the built-ins' index folders. CodeGraph 1.6.2 keeps
// .codegraph/codegraph.db, a SQLite database with -wal and -shm
// sidecars (its dist/directory.d.ts and dist/index.d.ts; docs/
// verification.md, "Managed tools"). A copy takes the database and its
// WAL only: the main checkout's daemon.pid, socket, and locks belong to
// its own server, and SQLite rebuilds -shm.
// TODO(T6711): under WSL on a Windows drive codegraph may use
// .codegraph-wsl instead (directory.d.ts); aggy does not launch there.
var toolIndexes = map[string]toolIndex{
	"codegraph": {dir: ".codegraph", db: "codegraph.db", copy: []string{"codegraph.db-wal", "codegraph.db"}},
}

// indexMarker is aggy's own file in an index folder: written only once
// the prebuild exited 0 (or a copy is complete), so a database an
// interrupted build left behind never counts as an index.
const indexMarker = ".aggy-indexed"

// prebuildTimeout bounds a prebuild. CodeGraph's README has the Linux
// kernel indexed in under 12 minutes on two cores.
const prebuildTimeout = 30 * time.Minute

// maxIndexCopy caps the files a worktree's copy of an index takes; a
// test lowers it.
var maxIndexCopy int64 = 1 << 30

// errNotIndexed is a person's no to the first index of a repo: nothing
// is recorded, and aggy tool on exits 0.
var errNotIndexed = errors.New("not indexed")

// indexDir reports whether p is a real folder (not a symlink), or
// absent (fs.ErrNotExist).
func indexDir(p string) (fs.FileInfo, error) {
	fi, err := safefs.Lstat(p)
	if err == nil && !fi.IsDir() {
		err = fmt.Errorf("%s is not a real folder (a symlink?)", p)
	}
	return fi, err
}

// regular reports whether p is there as a regular file.
func regular(p string) bool {
	fi, err := safefs.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}

// prebuildIndex is the lazy index of aggy tool on --repo (T6711): for a
// built-in with a prebuild, it checks that the program is installed and,
// when the main checkout has no finished index (its database and aggy's
// marker), asks at a terminal, puts the index folder in the repo's git
// exclude file (through the manifest, so aggy unwire undoes it), and
// runs the prebuild there as the MCP shim runs a server: the
// definition's argv with {repo} filled in, through execrun with an
// environment built from scratch (the variables the tool's servers
// declare), no stdin, a private empty working folder, and a time limit.
// Ctrl-C, SIGTERM, or SIGHUP during the build ends it and kills its
// process group. Any failure refuses the switch, so nothing is recorded. A repo
// where the tool is never turned on is never indexed.
func prebuildIndex(ctx context.Context, t tooldef.Tool, repo toolRepoAt, in io.Reader, tty bool, stdout io.Writer) error {
	if !t.BuiltIn || len(t.Install.Prebuild) == 0 {
		return nil
	}
	fail := func(err error) error {
		dlog.Log(ctx, diag.Rec{Event: diag.EvToolIndexRefused, Code: diag.CodeToolIndexFailed, Msg: fixedMsg(diag.CodeToolIndexFailed),
			Attrs: map[string]any{"tool": t.Name, "step": "init"}})
		return withCode(diag.CodeToolIndexFailed, err)
	}
	ix, ok := toolIndexes[t.Name]
	if !ok {
		return fail(fmt.Errorf("%s has a prebuild, but aggy knows no index folder for it", t.Name))
	}
	path := t.Install.Prebuild[0]
	if err := resolveProgram(path); err != nil {
		return fail(fmt.Errorf("%s is not installed (%s: %v); %s, then turn it on again", t.Name, path, err, installHowTo(t)))
	}
	if !filepath.IsAbs(path) {
		var err error
		if path, err = probe.LookPath(path); err != nil {
			return fail(err)
		}
	}
	// The folder is checked without following a symlink, so neither the
	// check nor the build reaches outside the repo through one.
	dir := filepath.Join(repo.main, ix.dir)
	if _, err := indexDir(dir); err == nil {
		if regular(filepath.Join(dir, indexMarker)) && regular(filepath.Join(dir, ix.db)) {
			if committed, _ := indexOwner(ctx, t.Name, repo, ix); !committed {
				return nil // indexed already; its server keeps it fresh
			}
			fmt.Fprintf(stdout, "%s/ holds an index committed in the repo, not one aggy built; indexing it once.\n", ix.dir)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fail(err)
	}
	// A database without aggy's marker is an index a build left
	// unfinished (an interrupted init). codegraph init exits 0 at once on
	// it, so the rebuild runs instead, then init if the rebuild fails (a
	// database without the schema, which only init rebuilds).
	steps := [][]string{t.Install.Prebuild}
	left := regular(filepath.Join(dir, ix.db))
	if left && len(t.Install.Rebuild) > 0 {
		steps = [][]string{t.Install.Rebuild, t.Install.Prebuild}
	}
	argvs := make([][]string, len(steps))
	for i, st := range steps {
		var err error
		if argvs[i], err = toolwire.Fill(st, repo.main); err != nil {
			return fail(err)
		}
	}
	argv := argvs[0]
	switch {
	case tty:
		fmt.Fprintf(stdout, "Index %s with %s? This runs %s, which writes %s/ in the repo. [y/N] ", termsafe.Field(repo.main), t.Name, quoteArgv(argv), ix.dir)
		line, _ := bufio.NewReader(io.LimitReader(in, maxAnswer)).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "Nothing indexed or turned on.")
			return errNotIndexed
		}
	case os.Getenv("AGGY_VENDOR") != "":
		// A nudge, not a security boundary: an agent in a session aggy
		// started is asked to leave the first index, which runs the tool
		// over the repo, to the person; clearing the variable gets past it.
		return fail(fmt.Errorf("the first index of a repo runs %s, so you run it yourself, at a terminal: aggy tool on %s --repo %s", quoteArgv(argv), t.Name, termsafe.Field(repo.main)))
	}
	excluded := excludeIndex(ctx, repo.main, ix.dir, stdout)
	var names []string
	for _, m := range t.MCP {
		names = append(names, m.Env...)
	}
	slices.Sort(names)
	tmp, err := safefs.MkdirTemp("", "aggy-prebuild-")
	if err != nil {
		return fail(err)
	}
	defer func() { _ = safefs.RemoveAll(tmp) }()
	// Only the build listens for Ctrl-C, SIGTERM, and SIGHUP, and then
	// kills its process group; a prompt ends on them as any program does.
	rctx, stop := execrun.NotifyContext(ctx)
	defer stop()
	fmt.Fprintf(stdout, "Indexing %s once for %s: %s. This may take a while (up to %s); Ctrl-C cancels.\n", termsafe.Field(repo.main), t.Name, quoteArgv(argv), prebuildTimeout)
	for i, a := range argvs {
		if a[0] != t.Install.Prebuild[0] {
			return fail(fmt.Errorf("%s: a rebuild runs the prebuild's program", quoteArgv(a)))
		}
		out, err := execrun.ToolRunner(prebuildTimeout, slices.Compact(names))(rctx, adapter.Command{Path: path, Args: a[1:], Dir: tmp})
		switch {
		case rctx.Err() != nil:
			err = errors.New("interrupted; nothing was recorded, and the next aggy tool on builds it again")
		case err == nil && out.ExitCode != 0:
			err = fmt.Errorf("exit %d: %s", out.ExitCode, termsafe.Field(strings.Join(borrow.Excerpt(string(out.Stderr), 5, 160), " | ")))
			if i+1 < len(argvs) {
				fmt.Fprintf(stdout, "%s failed (%s); trying %s.\n", quoteArgv(a), termsafe.Line(err.Error()), quoteArgv(argvs[i+1]))
				continue
			}
		}
		if err != nil {
			return fail(fmt.Errorf("%s: %w", quoteArgv(a), err))
		}
		break
	}
	dfi, err := indexDir(dir)
	if err != nil || !regular(filepath.Join(dir, ix.db)) {
		return fail(errors.Join(err, fmt.Errorf("%s ran but left no %s", quoteArgv(argv), filepath.Join(ix.dir, ix.db))))
	}
	if left && len(t.Install.Rebuild) == 0 {
		// The prebuild may have stopped at once on the unfinished index:
		// no marker, and the tool's server completes it on connect.
		fmt.Fprintf(stdout, "An unfinished index was in %s; %s's server completes it when a session connects.\n", termsafe.Field(dir), t.Name)
		return nil
	}
	if err := safefs.WriteFileNoFollowIn(dfi, filepath.Join(dir, indexMarker), []byte("built by aggy tool on\n"), 0o600, nil); err != nil && !errors.Is(err, fs.ErrExist) {
		return fail(err)
	}
	if err := recordIndexBuilt(t.Name, repo, ix); err != nil {
		fmt.Fprintf(stdout, "warning: aggy could not record that it built this index (%s); in a repo that commits it, the next aggy tool on indexes it again.\n", termsafe.Line(err.Error()))
	}
	dlog.Log(ctx, diag.Rec{Event: diag.EvToolIndexed, Attrs: map[string]any{"tool": t.Name, "step": "init"}})
	if excluded {
		fmt.Fprintf(stdout, "Indexed into %s, which the repo's git exclude file ignores; the tool's server keeps it fresh.\n", termsafe.Field(dir))
	} else {
		fmt.Fprintf(stdout, "Indexed into %s; the tool's server keeps it fresh. Add %s/ to .gitignore or .git/info/exclude so git ignores it.\n", termsafe.Field(dir), ix.dir)
	}
	return nil
}

// indexOwner says whose the finished-looking index of tool in repo is.
// It is aggy's when aggy's record of its last build there names the
// database's file identity. Otherwise, when git tracks the database or
// aggy's marker, the repo committed it (aggy's exclude line only keeps
// untracked files out) and aggy never built it: committed. When git
// cannot answer (not on PATH, say), the index counts as aggy's, and
// gitUnsure says so.
func indexOwner(ctx context.Context, tool string, repo toolRepoAt, ix toolIndex) (committed, gitUnsure bool) {
	if id, ok := indexDBID(repo, ix); ok {
		if p, err := indexRecordPath(tool, repo); err == nil {
			if b, err := safefs.ReadFileLimit(p, 256); err == nil && string(b) == id {
				return false, false
			}
		}
	}
	out, _, err := defaultBorrowEnv().git(ctx, repo.main, "ls-files", "-z", "--",
		filepath.ToSlash(filepath.Join(ix.dir, ix.db)), filepath.ToSlash(filepath.Join(ix.dir, indexMarker)))
	if err != nil {
		return false, true
	}
	return out != "", false
}

// indexRecordPath is aggy's record, in its state folder and never in the
// repo, of the index it last built for tool in repo: the database's file
// identity then.
func indexRecordPath(tool string, repo toolRepoAt) (string, error) {
	state, err := home.Dir()
	if err != nil {
		return "", err
	}
	key := repo.key
	if key == "" {
		key = repo.main
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(state, "indexes", tool+"-"+hex.EncodeToString(sum[:16])), nil
}

// indexDBID is the file identity (device and inode) of ix's database in
// repo. The tool's server updates the database in place, so the identity
// lasts; a checkout that rewrites the file gives it a new one.
func indexDBID(repo toolRepoAt, ix toolIndex) (string, bool) {
	fi, err := safefs.Lstat(filepath.Join(repo.main, ix.dir, ix.db))
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	dev, ino, ok := safefs.FileID(fi)
	return fmt.Sprintf("%d:%d\n", dev, ino), ok
}

// recordIndexBuilt writes aggy's record of the index it just built.
func recordIndexBuilt(tool string, repo toolRepoAt, ix toolIndex) error {
	id, ok := indexDBID(repo, ix)
	if !ok {
		return errors.New("the index database has no file identity")
	}
	p, err := indexRecordPath(tool, repo)
	if err != nil {
		return err
	}
	if err := safefs.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return safefs.WriteFile(p, []byte(id), 0o600)
}

// excludeIndex adds "<dir>/" to the git exclude file of the repo at
// main, which its linked worktrees share, through the manifest. It
// reports whether the line is there; a failure is a warning.
func excludeIndex(ctx context.Context, main, dir string, stdout io.Writer) bool {
	err := func() error {
		p, err := parity.GitExcludeLinePatch(main, dir+"/")
		if err != nil {
			return err
		}
		s, err := store.OpenDefault(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		m, err := manifest.Open(s)
		if err != nil {
			return err
		}
		runID := manifest.NewRunID()
		cs, err := m.Run(ctx, manifest.Write, runID, []patch.Patch{p}, io.Discard)
		for _, c := range cs {
			if c.Status == store.ChangeApplied {
				fmt.Fprintf(stdout, "Added %s/ to %s (aggy unwire --run %s undoes it).\n", dir, termsafe.Field(c.File), runID)
			}
		}
		return err
	}()
	if err != nil {
		fmt.Fprintf(stdout, "warning: %s/ was not added to the repo's git exclude file: %s\n", dir, termsafe.Line(err.Error()))
	}
	return err == nil
}

// copyIndexes copies the main checkout's finished index of each tool in
// on that has one (worktree-copy) and was wired (wired names it) into
// the worktree a session is about to start in (T6711), only when that is
// one of aggy's own edit worktrees (<state>/worktrees/<id>), which aggy
// gc removes with the copy; a worktree of the person's own gets none.
// Worktrees never share an index, and one where the tool is off never
// gets a copy. Each failure is a warning: the session starts without
// the copy, and the tool's server builds what it can there.
func copyIndexes(ctx context.Context, on []switchboard.State, wired []string, repo toolRepoAt) []string {
	if repo.root == "" || repo.root == repo.main {
		return nil
	}
	state, err := home.Dir()
	if err != nil || filepath.Dir(resolvePath(repo.root)) != resolvePath(filepath.Join(state, worktreesDir)) || !validBorrowID(filepath.Base(repo.root)) {
		return nil
	}
	var warns []string
	for _, st := range on {
		t := st.Tool
		ix, ok := toolIndexes[t.Name]
		if !ok || !t.BuiltIn || !slices.Contains(t.Lifecycle, "worktree-copy") || !slices.Contains(wired, t.Name) {
			continue
		}
		copied, err := copyIndex(ix, repo.main, repo.root)
		if err != nil {
			dlog.Log(ctx, diag.Rec{Event: diag.EvToolIndexRefused, Code: diag.CodeToolIndexCopy, Msg: fixedMsg(diag.CodeToolIndexCopy),
				Attrs: map[string]any{"tool": t.Name, "step": "copy"}})
			warns = append(warns, fmt.Sprintf("tool %s: the main checkout's index was not copied into this worktree: %s (%s)", t.Name, termsafe.Line(err.Error()), diag.CodeToolIndexCopy))
			continue
		}
		if copied {
			dlog.Log(ctx, diag.Rec{Event: diag.EvToolIndexed, Attrs: map[string]any{"tool": t.Name, "step": "copy"}})
		}
	}
	return warns
}

// copyIndex copies ix's files from the main checkout main into the
// worktree wt, through safefs. It copies only a finished index (the
// database and aggy's marker in main), and nothing when wt's folder has
// a database already (an earlier copy, or its own index). The source
// folder must be a real folder; a symlink in it refuses the copy, and so
// does a file to copy that is not a regular file with one link, or
// files over maxIndexCopy in all. In wt the folder's .gitignore (*) is
// written first unless one is there, each file is copied under a temp
// name and renamed into place only once every source is checked again
// against the first look (identity, size, mtime: the main checkout's
// server may write while it is copied), the database last, then aggy's
// marker. A refused or cut-short copy leaves no database, so it is never
// taken for an index, and the next launch copies again.
func copyIndex(ix toolIndex, main, wt string) (bool, error) {
	src, dst := filepath.Join(main, ix.dir), filepath.Join(wt, ix.dir)
	if _, err := indexDir(dst); err == nil {
		if _, err := safefs.Lstat(filepath.Join(dst, ix.db)); !errors.Is(err, fs.ErrNotExist) {
			return false, err // there, or not checkable
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if _, err := indexDir(src); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if !regular(filepath.Join(src, indexMarker)) {
		return false, nil // not a finished index
	}
	ents, err := safefs.ReadDir(src)
	if err != nil {
		return false, err
	}
	snap := map[string]fs.FileInfo{}
	var total int64
	for _, e := range ents {
		p := filepath.Join(src, e.Name())
		fi, err := safefs.Lstat(p)
		switch {
		case err != nil:
			return false, err
		case fi.Mode()&fs.ModeSymlink != 0:
			return false, fmt.Errorf("%s is a symlink", p)
		case !slices.Contains(ix.copy, e.Name()):
			continue
		case !fi.Mode().IsRegular() || safefs.LinkCount(fi) > 1:
			return false, fmt.Errorf("%s is not a regular file with one link", p)
		}
		if total += fi.Size(); total > maxIndexCopy {
			return false, fmt.Errorf("the index is over the %d MiB a worktree copy takes", maxIndexCopy>>20)
		}
		snap[e.Name()] = fi
	}
	if snap[ix.db] == nil {
		return false, nil
	}
	if err := safefs.Mkdir(dst, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return false, err
	}
	dfi, err := indexDir(dst)
	if err != nil {
		return false, err
	}
	if err := safefs.WriteFileNoFollowIn(dfi, filepath.Join(dst, ".gitignore"), []byte("*\n"), 0o600, nil); err != nil && !errors.Is(err, fs.ErrExist) {
		return false, err
	}
	// Left by a copy cut short: no database came with them.
	stale := []string{indexMarker}
	for _, n := range ix.copy {
		stale = append(stale, n, copyTemp(n))
	}
	for _, n := range stale {
		p := filepath.Join(dst, n)
		if fi, err := safefs.Lstat(p); err == nil {
			if !fi.Mode().IsRegular() {
				return false, fmt.Errorf("%s is not a regular file", p)
			}
			if err := safefs.RemoveSame(p, fi); err != nil {
				return false, err
			}
		}
	}
	type staged struct {
		name, tmp string
		fi        fs.FileInfo
	}
	var done []staged
	defer func() {
		for _, s := range done {
			_ = safefs.RemoveSame(s.tmp, s.fi)
		}
	}()
	for _, n := range ix.copy {
		if snap[n] == nil {
			continue
		}
		tmp := filepath.Join(dst, copyTemp(n))
		if err := copyRegular(filepath.Join(src, n), snap[n], tmp); err != nil {
			return false, err
		}
		fi, err := safefs.Lstat(tmp)
		if err != nil {
			return false, err
		}
		done = append(done, staged{n, tmp, fi})
	}
	// Every source again, after all are copied: a checkpoint between the
	// database and its WAL would make a pair that does not match.
	if testHookIndexCopied != nil {
		testHookIndexCopied()
	}
	for n, want := range snap {
		fi, err := safefs.Lstat(filepath.Join(src, n))
		if err != nil || !os.SameFile(fi, want) || fi.Size() != want.Size() || !fi.ModTime().Equal(want.ModTime()) {
			return false, fmt.Errorf("%s changed while the index was copied", filepath.Join(src, n))
		}
	}
	for len(done) > 0 {
		s := done[0]
		if err := safefs.RenameNoReplace(s.tmp, filepath.Join(dst, s.name), s.fi); err != nil {
			return false, err
		}
		done = done[1:]
	}
	if err := safefs.WriteFileNoFollowIn(dfi, filepath.Join(dst, indexMarker), []byte("copied by aggy\n"), 0o600, nil); err != nil {
		return false, err
	}
	return true, nil
}

// testHookIndexCopied runs once copyIndex has copied every file and
// before it checks the sources again; a test changes one there.
var testHookIndexCopied func()

// copyTemp is the name a copied file has until it is renamed into place.
func copyTemp(name string) string { return "." + name + ".aggy-copy" }

// copyRegular copies src, which must still be the file want describes,
// to dst, a new file, and refuses a src that changed meanwhile.
func copyRegular(src string, want fs.FileInfo, dst string) error {
	in, err := safefs.OpenRegular(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	same := func() bool {
		fi, err := in.Stat()
		return err == nil && os.SameFile(fi, want) && fi.Size() == want.Size() && fi.ModTime().Equal(want.ModTime())
	}
	if !same() {
		return fmt.Errorf("%s changed while it was copied", src)
	}
	out, err := safefs.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, want.Size()+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && (n != want.Size() || !same()) {
		err = fmt.Errorf("%s changed while it was copied", src)
	}
	if err != nil {
		_ = safefs.Remove(dst)
	}
	return err
}
