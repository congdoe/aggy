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

	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/tooldef"
	"github.com/congdoe/aggy/internal/toolwire"
)

// execTool is execrun.ExecTool; a test swaps it to see what would run.
var execTool = execrun.ExecTool

// runToolExec is aggy tool exec <dir> <name> <digest> <repo> -- <argv...>
// (toolwire.ExecArgs), the command a vendor CLI runs for each managed
// tool's MCP server (W71-2). It writes nothing to stdout, the server's
// MCP channel. It starts the server only when:
//
//   - dir is a launch folder directly in aggy's run directory: a real
//     directory of this user, mode 0700 (ownPrivateDir);
//   - the tool's definition is loaded as aggy loads it, and its digest
//     still starts with digest, so an edited file never runs unseen;
//   - argv is one of the definition's MCP servers with {repo} filled in
//     from repo (toolwire.Match), so a changed config cannot make it run
//     anything else.
//
// The server then runs in dir with an environment built from scratch
// (execrun.ToolEnv): the basics plus the tool's declared variables, read
// now from the CLI's environment and never written to a file, and the
// telemetry opt-outs, or with telemetry allowed the person's own
// DO_NOT_TRACK (toolTelemetry).
func runToolExec(args []string, stderr io.Writer) int {
	ctx := context.Background()
	name := ""
	fail := func(check string, err error) int {
		attrs := map[string]any{"check": check}
		if name != "" {
			attrs["tool"] = name
		}
		refuse(ctx, stderr, "tool exec", diag.EvToolExecRefused, diag.CodeToolExec, err.Error(), diag.Rec{Attrs: attrs})
		return 1
	}
	if len(args) < 6 || args[4] != "--" {
		return fail("usage", errors.New("want <dir> <name> <digest> <repo> -- <program> [args...]; aggy runs this itself"))
	}
	dir, digest, repo, argv := args[0], args[2], args[3], args[5:]
	name = args[1]
	state, dirInfo, err := checkLaunchDir(dir)
	if err != nil {
		return fail("dir", err)
	}
	// F74-4: what follows is logged under the state dir the definitions
	// come from, also for a CLI (Codex) that does not pass AGGY_HOME.
	if err := os.Setenv(home.Env, state); err != nil {
		return fail("env", err)
	}
	r, _, err := tooldef.Load(state)
	if err != nil {
		return fail("definition", err)
	}
	t, ok := r.Get(name)
	if !ok {
		return fail("definition", fmt.Errorf("no tool %s is defined, or its definition is refused (aggy tool list)", name))
	}
	if len(digest) != toolwire.DigestLen || len(t.Digest) < len(digest) || t.Digest[:len(digest)] != digest {
		return fail("digest", fmt.Errorf("%s's definition changed since this session wired it; start a new session", name))
	}
	if repo == toolwire.NoRepo {
		repo = ""
	}
	m, err := toolwire.Match(t, repo, argv)
	if err != nil {
		return fail("argv", err)
	}
	// F74-2: no PATH entry from the repo picks the program or reaches it.
	// Outside a repo, the folder the CLI started the shim in.
	at := repo
	if at == "" {
		if at, err = os.Getwd(); err != nil {
			return fail("env", err)
		}
	}
	p, err := toolPath(at)
	if err == nil {
		err = os.Setenv("PATH", p)
	}
	if err != nil {
		return fail("env", err)
	}
	prog := argv[0]
	if !filepath.IsAbs(prog) {
		if prog, err = probe.LookPath(prog); err != nil {
			return fail("program", err)
		}
	}
	pass, set := toolTelemetry(state, t)
	env, err := execrun.ToolEnv(os.Environ(), append(slices.Clone(m.Env), pass...))
	if err == nil {
		env, err = execrun.SetEnv(env, set)
	}
	if err != nil {
		return fail("env", err)
	}
	if err := execTool(prog, argv[1:], dir, dirInfo, env); err != nil {
		return fail("exec", err)
	}
	return 0 // only a test's execTool returns
}

// checkLaunchDir refuses a folder that is not one of aggy's launch
// folders: a clean absolute path directly in a run folder, both real
// directories (not symlinks) of this user with mode 0700. It returns the
// state dir the run folder is in. The state dir comes from the path, not
// from AGGY_HOME, which a vendor CLI may not pass to the servers it
// starts (Codex passes its defaults and env_vars only); the definitions
// are then loaded from there with tooldef's own owner and mode checks.
// It also returns dir's Lstat, which the move into it is checked against
// (F74-1).
func checkLaunchDir(dir string) (string, fs.FileInfo, error) {
	run := filepath.Dir(dir)
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || filepath.Base(run) != "run" || run == dir {
		return "", nil, fmt.Errorf("%s is not a launch folder in aggy's run directory", dir)
	}
	var fi fs.FileInfo
	for _, d := range []string{run, dir} {
		var err error
		if fi, err = safefs.Lstat(d); err != nil {
			return "", nil, err
		}
		if !ownPrivateDir(fi) {
			return "", nil, fmt.Errorf("%s is not a private (0700) directory of this user", d)
		}
	}
	return filepath.Dir(run), fi, nil
}

// toolPath is PATH for a tool's program, the lookup of it included
// (F74-2): without the entries a repo could have put there (direnv's
// PATH_add, a project's env.PATH), that is any under the work tree
// holding dir or under its main checkout, whose .envrc a linked worktree
// nested in it inherits; outside a repo, any under dir itself, unless dir
// is the home folder or above it, whose entries are the person's own. An
// error means dir cannot be placed, and the caller fails closed. aggy
// tool exec, aggy hook-tool, and a tool's proxy all use it.
func toolPath(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("no folder to keep PATH out of")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	r, err := toolRepo(dir, false)
	if err != nil {
		return "", err
	}
	roots := []string{r.root, r.main}
	if r.root == "" {
		roots = []string{dir}
		if h, err := os.UserHomeDir(); err == nil && len(pathOutside(h, roots)) == 0 {
			roots = nil // dir is home or above it
		}
	}
	return pathOutside(os.Getenv("PATH"), roots), nil
}

// pathOutside is PATH p without the entries that are one of roots or lie
// below one, compared as written and canonically.
func pathOutside(p string, dirs []string) string {
	var roots []string
	for _, d := range dirs {
		if d == "" {
			continue
		}
		roots = append(roots, filepath.Clean(d))
		if c, err := safefs.CanonicalPath(d); err == nil {
			roots = append(roots, c)
		}
	}
	in := func(d string) bool {
		forms := []string{filepath.Clean(d)}
		if c, err := safefs.CanonicalPath(d); err == nil {
			forms = append(forms, c)
		}
		for _, f := range forms {
			for _, r := range roots {
				if f == r || strings.HasPrefix(f, strings.TrimSuffix(r, "/")+"/") {
					return true
				}
			}
		}
		return false
	}
	var keep []string
	for _, d := range filepath.SplitList(p) {
		if !in(d) {
			keep = append(keep, d)
		}
	}
	return strings.Join(keep, string(filepath.ListSeparator))
}
