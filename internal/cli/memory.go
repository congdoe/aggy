package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const memoryUsage = `Usage: aggy memory import --from claude [--dir D] [--only a,b] [--yes] [--dry-run]

Copies entries from Claude Code's auto memory for this repo into the
shared memory, .aggy/memory.md, which every CLI aggy starts gets at
session start (4 KB at most). Claude keeps one markdown file per memory
in <config dir>/projects/<project>/memory/; each becomes a "## <name>"
section, and a section already in .aggy/memory.md is skipped. Nothing
else is read: not the index, not transcripts, not credentials.

The change is shown as a diff first. It is written only when you answer
y, or with --yes, and it goes through the manifest, so aggy unwire
undoes it. In a git repo, .aggy/ is also added to .git/info/exclude.

  --from claude  the only source so far
  --dir D        Claude's memory folder; the default is derived from the
                 repo path, which Claude does not document (unverified)
  --only a,b     import only these entries (file names without .md)
  --yes          write without asking
  --dry-run      show the diff and write nothing
`

// maxMemoryEntry caps one Claude memory file read for import.
const maxMemoryEntry = 64 << 10

// memoryEntry is one Claude memory file.
type memoryEntry struct {
	stem, name, body string
}

func runMemory(args []string, stdout, stderr io.Writer) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "aggy memory: %v\n", err)
		return 1
	}
	return memoryWith(args, cwd, os.Stdin, isTerminal(os.Stdin), stdout, stderr)
}

func memoryWith(args []string, cwd string, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "import" {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			fmt.Fprint(stdout, memoryUsage)
			return 0
		}
		fmt.Fprint(stderr, memoryUsage)
		return 2
	}
	fl := flag.NewFlagSet("memory import", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	from := fl.String("from", "", "")
	dir := fl.String("dir", "", "")
	only := fl.String("only", "", "")
	yes := fl.Bool("yes", false, "")
	dryRun := fl.Bool("dry-run", false, "")
	if err := fl.Parse(args[1:]); err != nil || fl.NArg() > 0 || *from != "claude" || (*yes && *dryRun) {
		fmt.Fprintf(stderr, "aggy memory import: bad arguments\n\n%s", memoryUsage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "aggy memory import: %s\n", termsafe.Line(err.Error()))
		return 1
	}
	root, inRepo := repoRoot(cwd)
	if *dir == "" {
		d, err := claudeMemoryDir(root)
		if err != nil {
			return fail(err)
		}
		*dir = d
	} else if !filepath.IsAbs(*dir) {
		*dir = filepath.Join(cwd, *dir)
	}
	entries, skipped, err := readClaudeMemory(*dir)
	if err != nil {
		return fail(fmt.Errorf("%w; pass --dir with Claude's memory folder for this repo", err))
	}
	for _, s := range skipped {
		fmt.Fprintf(stderr, "aggy memory import: skipped %s\n", termsafe.Line(s))
	}
	if *only != "" {
		var keep []memoryEntry
		for _, s := range strings.Split(*only, ",") {
			i := slices.IndexFunc(entries, func(e memoryEntry) bool { return e.stem == strings.TrimSpace(s) })
			if i < 0 {
				return fail(fmt.Errorf("no entry %q in %s", s, *dir))
			}
			keep = append(keep, entries[i])
		}
		entries = keep
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "No Claude memory entries to import.")
		return 0
	}
	fmt.Fprintf(stdout, "Claude memory entries in %s:\n", termsafe.Field(*dir))
	for _, e := range entries {
		fmt.Fprintf(stdout, "  %s: %s (%d bytes)\n", termsafe.Field(e.stem), termsafe.Field(e.name), len(e.body))
	}
	ps, err := memoryPatches(root, inRepo, entries)
	if err != nil {
		return fail(err)
	}
	n, err := manifest.Preview(stdout, ps)
	if err != nil {
		return fail(err)
	}
	if n == 0 || *dryRun {
		return 0
	}
	if !*yes {
		if !tty {
			fmt.Fprintln(stdout, "Not written: run again with --yes, or in a terminal, to write it.")
			return 0
		}
		fmt.Fprint(stdout, "Write these changes? [y/N] ")
		line, _ := bufio.NewReader(io.LimitReader(in, maxAnswer)).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "Nothing written.")
			return 0
		}
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		return fail(err)
	}
	runID := manifest.NewRunID()
	// The patches carry the hashes the preview was planned on, so a file
	// changed since is refused, not overwritten.
	cs, err := m.Run(ctx, manifest.Write, runID, ps, io.Discard)
	for _, c := range cs {
		if c.Status == store.ChangeApplied {
			fmt.Fprintf(stdout, "changed %s\n", termsafe.Field(c.File))
		}
	}
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "Done (run %s). aggy unwire --run %s undoes it.\n", runID, runID)
	return 0
}

// claudeMemoryDir is Claude's auto memory folder for the repo at root:
// <config dir>/projects/<project>/memory, where the config dir is
// $CLAUDE_CONFIG_DIR or ~/.claude (code.claude.com/docs memory.md,
// "Storage location"). The docs say <project> is derived from the git
// repository and shared by its worktrees, but not how: the encoding here,
// the main checkout's absolute path with every character but a letter or
// digit made "-", is what Claude was seen to use, not a documented
// fact (unverified). --dir overrides it.
func claudeMemoryDir(root string) (string, error) {
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if cfg == "" {
		h := os.Getenv("HOME")
		if !filepath.IsAbs(h) {
			return "", errors.New("HOME is not an absolute path")
		}
		cfg = filepath.Join(h, ".claude")
	} else if !filepath.IsAbs(cfg) {
		return "", fmt.Errorf("CLAUDE_CONFIG_DIR=%q is not an absolute path", cfg)
	}
	// A linked worktree shares the main checkout's memory.
	if ex, err := parity.GitExcludePath(root); err == nil {
		if common := filepath.Dir(filepath.Dir(ex)); filepath.Base(common) == ".git" {
			root = filepath.Dir(common)
		}
	}
	project := strings.Map(func(r rune) rune {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, root)
	return filepath.Join(cfg, "projects", project, "memory"), nil
}

// readClaudeMemory reads the memory files in dir: every regular *.md
// file but the MEMORY.md index, sorted by name. A file that is a symlink,
// not regular, or over maxMemoryEntry is skipped and named in skipped.
func readClaudeMemory(dir string) (entries []memoryEntry, skipped []string, err error) {
	des, err := safefs.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, de := range des {
		n := de.Name()
		if !strings.HasSuffix(n, ".md") || n == "MEMORY.md" {
			continue
		}
		b, err := safefs.ReadFileLimit(filepath.Join(dir, n), maxMemoryEntry)
		if err != nil {
			skipped = append(skipped, n+": "+err.Error())
			continue
		}
		e := parseMemoryEntry(strings.TrimSuffix(n, ".md"), string(b))
		if e.body != "" {
			entries = append(entries, e)
		}
	}
	return entries, skipped, nil
}

// parseMemoryEntry splits a Claude memory file into its frontmatter name
// (the file name when there is none) and its body, both cleaned.
func parseMemoryEntry(stem, src string) memoryEntry {
	e := memoryEntry{stem: stem, name: stem}
	src = strings.ReplaceAll(src, "\r\n", "\n")
	if rest, ok := strings.CutPrefix(src, "---\n"); ok {
		if fm, body, ok := strings.Cut(rest, "\n---\n"); ok {
			src = body
			for _, l := range strings.Split(fm, "\n") {
				if v, ok := strings.CutPrefix(l, "name:"); ok {
					if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
						e.name = v
					}
				}
			}
		}
	}
	e.name = strings.Join(strings.Fields(parity.CleanMemory(e.name)), " ")
	e.body = parity.CleanMemory(src)
	return e
}

// memoryPatches returns the patches that add entries to the repo's
// .aggy/memory.md, and in a git repo .aggy/ to its exclude file. The
// memory file must be a regular file in a real .aggy directory, or
// absent.
func memoryPatches(root string, inRepo bool, entries []memoryEntry) ([]patch.Patch, error) {
	path := parity.MemoryPath(root)
	// ReadConfig refuses a symlinked .aggy (even with no memory.md in it)
	// or memory.md, and anything not a regular file.
	b, err := safefs.ReadConfig(path, parity.MemoryCap)
	h := patch.Hash(b)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		b, h = nil, patch.NoFile
	case errors.Is(err, safefs.ErrTooLarge):
		return nil, parity.ErrMemoryTooBig
	case err != nil:
		return nil, err
	}
	add := addMemoryEntries(entries)
	if _, err := add(b); err != nil {
		return nil, err
	}
	var ps []patch.Patch
	if inRepo {
		p, err := parity.GitExcludePatch(root)
		if err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	return append(ps, patch.Patch{File: path, ExpectedHash: h, Transform: add, Mode: 0o644}), nil
}

// addMemoryEntries returns the transform that appends each entry as a
// "## <name>" section, skipping a section whose heading line is already
// there, and refuses a result over parity.MemoryCap.
func addMemoryEntries(entries []memoryEntry) func([]byte) ([]byte, error) {
	return func(src []byte) ([]byte, error) {
		out := string(src)
		lines := strings.Split(out, "\n")
		for _, e := range entries {
			head := "## " + e.name
			if slices.Contains(lines, head) {
				continue
			}
			if out != "" && !strings.HasSuffix(out, "\n") {
				out += "\n"
			}
			if out != "" {
				out += "\n"
			}
			out += head + "\n\n" + e.body + "\n"
			lines = append(lines, head)
		}
		if len(out) > parity.MemoryCap {
			return nil, fmt.Errorf("%s would be %d bytes, over the %d-byte cap; pick fewer entries with --only", parity.MemoryFile, len(out), parity.MemoryCap)
		}
		return []byte(out), nil
	}
}
