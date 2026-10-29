package cli

import (
	"context"
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
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
	"github.com/congdoe/aggy/internal/toolwire"
)

// toolIndex is a built-in's per-repo index (T6711): the folder its
// prebuild writes under the work-tree root, the file whose presence
// means the repo is indexed, and the files a linked worktree's copy
// takes.
type toolIndex struct {
	dir, marker string
	copy        []string
}

// toolIndexes are the built-ins' index folders. CodeGraph 1.6.2 keeps
// .codegraph/codegraph.db, a SQLite database with -wal and -shm
// sidecars, and counts a repo as indexed once codegraph.db is there
// (its dist/directory.d.ts and dist/index.d.ts; docs/verification.md,
// "Managed tools"). A copy takes the database and its WAL only: the
// main checkout's daemon.pid and lock files belong to its own server,
// and SQLite rebuilds -shm.
var toolIndexes = map[string]toolIndex{
	"codegraph": {dir: ".codegraph", marker: "codegraph.db", copy: []string{"codegraph.db", "codegraph.db-wal"}},
}

// toolInstallHow says how to install a built-in's pinned version
// ({version}); aggy only says it and never installs anything.
var toolInstallHow = map[string]string{
	"codegraph": "npm install -g @colbymchenry/codegraph@{version}", // docs/verification.md, "Managed tools"
}

// installHow is the fix for a tool whose program is missing.
func installHow(t tooldef.Tool) string {
	if how, ok := toolInstallHow[t.Name]; ok && t.BuiltIn {
		return "install " + t.Name + " " + t.Version + " yourself: " + strings.ReplaceAll(how, "{version}", t.Version)
	}
	return "install " + t.Name + " " + t.Version
}

// prebuildTimeout bounds a prebuild. CodeGraph's README has the Linux
// kernel indexed in under 12 minutes on two cores.
const prebuildTimeout = 30 * time.Minute

// maxIndexCopy caps the files a worktree's copy of an index takes; a
// test lowers it.
var maxIndexCopy int64 = 1 << 30

// gitignoreAll is the .gitignore aggy puts in an index folder, so git
// ignores the folder and everything in it, itself included.
const gitignoreAll = "*\n"

// prebuildIndex is the lazy index of aggy tool on --repo (T6711): for a
// built-in with a prebuild, it checks that the program is installed and,
// when the main checkout has no index yet, runs the prebuild there once
// as the MCP shim runs a server: the definition's argv with {repo}
// filled in, through execrun with an environment built from scratch
// (the variables the tool's servers declare), no stdin, a private empty
// working folder, and a time limit. Any failure refuses the switch, so
// nothing is recorded. A repo where the tool is never turned on is never
// indexed.
func prebuildIndex(ctx context.Context, t tooldef.Tool, repo toolRepoAt, stdout io.Writer) error {
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
		return fail(fmt.Errorf("%s is not installed (%s: %v); %s, then turn it on again", t.Name, path, err, installHow(t)))
	}
	if !filepath.IsAbs(path) {
		var err error
		if path, err = probe.LookPath(path); err != nil {
			return fail(err)
		}
	}
	dir := filepath.Join(repo.main, ix.dir)
	switch fi, err := safefs.Lstat(filepath.Join(dir, ix.marker)); {
	case err == nil && fi.Mode().IsRegular():
		return nil // indexed already; its server keeps it fresh
	case err == nil:
		return fail(fmt.Errorf("%s is not a regular file", filepath.Join(dir, ix.marker)))
	case !errors.Is(err, fs.ErrNotExist):
		return fail(err)
	}
	argv, err := toolwire.Fill(t.Install.Prebuild, repo.main)
	if err != nil {
		return fail(err)
	}
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
	fmt.Fprintf(stdout, "Indexing %s once for %s (up to %s): %s\n", termsafe.Field(repo.main), t.Name, prebuildTimeout, quoteArgv(argv))
	out, err := execrun.ToolRunner(prebuildTimeout, slices.Compact(names))(ctx, adapter.Command{Path: path, Args: argv[1:], Dir: tmp})
	if err == nil && out.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", out.ExitCode, termsafe.Field(strings.Join(borrow.Excerpt(string(out.Stderr), 5, 160), " | ")))
	}
	if err != nil {
		return fail(fmt.Errorf("%s: %w", quoteArgv(argv), err))
	}
	if fi, err := safefs.Lstat(filepath.Join(dir, ix.marker)); err != nil || !fi.Mode().IsRegular() {
		return fail(fmt.Errorf("%s ran but left no %s", quoteArgv(argv), filepath.Join(ix.dir, ix.marker)))
	}
	// The index lives in the repo: aggy makes sure git ignores it.
	dfi, err := safefs.Lstat(dir)
	if err == nil && !dfi.IsDir() {
		err = errors.New("not a real folder")
	}
	if err == nil {
		if err = safefs.WriteFileNoFollowIn(dfi, filepath.Join(dir, ".gitignore"), []byte(gitignoreAll), 0o644, nil); errors.Is(err, fs.ErrExist) {
			err = nil // one is there already; it is left as it is
		}
	}
	if err != nil {
		fmt.Fprintf(stdout, "warning: no .gitignore was written in %s (%s): add %s/ to the repo's .gitignore\n", termsafe.Field(dir), termsafe.Line(err.Error()), ix.dir)
	}
	dlog.Log(ctx, diag.Rec{Event: diag.EvToolIndexed, Attrs: map[string]any{"tool": t.Name, "step": "init"}})
	fmt.Fprintf(stdout, "Indexed into %s, which git ignores; the tool's server keeps it fresh.\n", termsafe.Field(dir))
	return nil
}

// copyIndexes copies the main checkout's index of each tool in on that
// has one (worktree-copy) and was wired (wired names it) into the linked
// worktree a session is about to start in, when the worktree has none
// yet (T6711). Worktrees never share an index, and one where the tool is
// off never gets a copy. Each failure is a warning: the session starts
// without the copy, and the tool's server builds what it can there. aggy
// gc removes the copy with the worktree.
func copyIndexes(ctx context.Context, on []switchboard.State, wired []string, repo toolRepoAt) []string {
	if repo.root == "" || repo.root == repo.main {
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
// worktree wt, through safefs: the index folder must be a real folder;
// a symlink anywhere in it refuses the copy, and so does a file to copy
// that is not a regular file with one link, or files over maxIndexCopy
// in all. Nothing is copied when wt has the folder already (an earlier
// copy, or its own index) or main has no index. A file that changes
// while it is copied (the main checkout's server writing) refuses the
// copy, and a refused copy leaves nothing behind. The copy gets its own
// .gitignore, so it never lands in an edit's diff.
func copyIndex(ix toolIndex, main, wt string) (bool, error) {
	src, dst := filepath.Join(main, ix.dir), filepath.Join(wt, ix.dir)
	switch _, err := safefs.Lstat(dst); {
	case err == nil:
		return false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	sfi, err := safefs.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sfi.IsDir() {
		return false, fmt.Errorf("%s is not a real folder (a symlink?)", src)
	}
	ents, err := safefs.ReadDir(src)
	if err != nil {
		return false, err
	}
	type file struct {
		name string
		fi   fs.FileInfo
	}
	var files []file
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
		files = append(files, file{e.Name(), fi})
	}
	if !slices.ContainsFunc(files, func(f file) bool { return f.name == ix.marker }) {
		return false, nil // not indexed yet
	}
	if wfi, err := safefs.Lstat(wt); err != nil || !wfi.IsDir() {
		return false, errors.Join(err, fmt.Errorf("%s is not a real folder", wt))
	}
	if err := safefs.Mkdir(dst, 0o700); err != nil {
		return false, err
	}
	done := false
	defer func() {
		if !done {
			_ = safefs.RemoveAll(dst)
		}
	}()
	dfi, err := safefs.Lstat(dst)
	if err != nil || !dfi.IsDir() {
		return false, errors.Join(err, fmt.Errorf("%s is not a real folder", dst))
	}
	for _, f := range files {
		if err := copyRegular(filepath.Join(src, f.name), f.fi, filepath.Join(dst, f.name)); err != nil {
			return false, err
		}
	}
	if err := safefs.WriteFileNoFollowIn(dfi, filepath.Join(dst, ".gitignore"), []byte(gitignoreAll), 0o600, nil); err != nil {
		return false, err
	}
	done = true
	return true, nil
}

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
	if err != nil {
		return err
	}
	if n != want.Size() || !same() {
		return fmt.Errorf("%s changed while it was copied", src)
	}
	return nil
}
