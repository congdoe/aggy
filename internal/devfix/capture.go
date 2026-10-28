package devfix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
)

// Limits on what Capture copies.
const (
	maxSnapshotEntries = 200_000
	maxCopiedFiles     = 200
	maxCopiedFile      = 1 << 20
)

// SessionPaths are the vendor-home files Capture may copy, as globs
// relative to the vendor home in which {id} is a session id the run
// printed. Nothing else in the vendor home is copied, so another
// session's transcript never is. Only the Codex image folder and the Grok
// session folder are recorded; the transcript paths are unconfirmed.
var SessionPaths = map[adapter.Vendor][]string{
	adapter.Claude: {"projects/*/{id}.jsonl"},
	adapter.Codex:  {"generated_images/{id}/*", "sessions/*/*/*/rollout-*{id}.jsonl"},
	adapter.Grok:   {"sessions/*/{id}/*", "sessions/*/{id}/*/*"},
}

// CaptureOptions describes one capture.
type CaptureOptions struct {
	Vendor adapter.Vendor
	// Bin is the CLI to run, a clean absolute path. Empty means the
	// vendor's program on PATH.
	Bin  string
	Args []string
	// Out is the fixture directory to create. It must not exist.
	Out string
	// Env holds extra KEY=value entries; the runner accepts only AGGY_*.
	Env []string
	// Timeout bounds the run. Zero means five minutes.
	Timeout time.Duration
	// Run runs the CLI. Nil means probe.ExecRunner(Timeout).
	Run adapter.Runner
	// Identity is what the scrub replaces besides pattern matches. Nil
	// means the current user's.
	Identity *Identity
}

// Meta is the fixture's meta.json.
type Meta struct {
	Vendor   string   `json:"vendor"`
	Args     []string `json:"args"`
	ExitCode int      `json:"exit_code"`
	Files    []string `json:"files"`   // copied, relative to files/
	Skipped  []string `json:"skipped"` // written by the CLI but not copied, with why
}

// vendorHome is the vendor's config directory from the environment: the
// relocation variable, else ~/.<vendor>.
func vendorHome(v adapter.Vendor) string {
	env := map[adapter.Vendor]string{adapter.Claude: "CLAUDE_CONFIG_DIR", adapter.Codex: "CODEX_HOME", adapter.Grok: "GROK_HOME"}[v]
	if h := os.Getenv(env); h != "" {
		return filepath.Clean(h)
	}
	if h := os.Getenv("HOME"); h != "" {
		return filepath.Join(h, "."+string(v))
	}
	return ""
}

// CurrentIdentity is the running user's home, user name, and host name.
func CurrentIdentity() Identity {
	id := Identity{Home: os.Getenv("HOME"), User: os.Getenv("USER")}
	id.Host, _ = os.Hostname()
	return id
}

type stamp struct {
	size int64
	mod  time.Time
}

// snapshot records the regular files under root by slash path. A missing
// root is empty; unreadable entries (such as refused credential files)
// are skipped.
func snapshot(root string) map[string]stamp {
	out := map[string]stamp{}
	if root == "" {
		return out
	}
	count := 0
	var walk func(rel string)
	walk = func(rel string) {
		entries, err := safefs.ReadDir(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return
		}
		for _, e := range entries {
			if count++; count > maxSnapshotEntries {
				return
			}
			r := path.Join(rel, e.Name())
			switch {
			case e.IsDir():
				walk(r)
			case e.Type().IsRegular():
				if fi, err := e.Info(); err == nil {
					out[r] = stamp{fi.Size(), fi.ModTime()}
				}
			}
		}
	}
	walk("")
	return out
}

// adapters parse a run's output for its session id.
var adapters = map[adapter.Vendor]adapter.Adapter{
	adapter.Claude: claude.New(), adapter.Codex: codex.New(), adapter.Grok: grok.New(),
}

// sessionIDs returns the run's session id: from the vendor's structured
// field (Claude session_id, Codex thread.started, Grok sessionId) through
// the adapter's ParseHeadless, else the one UUID the output holds. With
// none, or several candidates, it returns no id and why, so no vendor-home
// file is copied: a reply that mentions another session must not copy
// that session's transcript.
func sessionIDs(v adapter.Vendor, out adapter.Output) ([]string, string) {
	if a, ok := adapters[v]; ok {
		if res, err := a.ParseHeadless(adapter.HeadlessRequest{}, out); err == nil && res.SessionID != "" {
			return []string{res.SessionID}, ""
		}
	}
	var cands []string
	for _, m := range uuidRE.FindAllString(string(out.Stdout)+"\n"+string(out.Stderr), -1) {
		if !slices.ContainsFunc(cands, func(c string) bool { return strings.EqualFold(c, m) }) {
			cands = append(cands, m)
		}
	}
	if len(cands) == 1 {
		return cands, ""
	}
	return nil, fmt.Sprintf(" (no session id: %d candidates in the output)", len(cands))
}

// sessionFile reports whether rel, under the vendor home, matches one of
// the vendor's SessionPaths for one of ids.
func sessionFile(v adapter.Vendor, rel string, ids []string) bool {
	for _, g := range SessionPaths[v] {
		for _, id := range ids {
			if ok, _ := path.Match(strings.ReplaceAll(g, "{id}", id), rel); ok {
				return true
			}
		}
	}
	return false
}

// Capture runs the CLI once and records a fixture in opts.Out: stdout,
// stderr, meta.json, and under files/ the text files the run created or
// changed in its working dir (a fresh temp dir) and, in its vendor home,
// only those matching SessionPaths for a session id it printed. Binary
// files are never copied. Then it scrubs the fixture. It is for
// maintainers: it runs whatever CLI Bin names, which may spend quota.
func Capture(ctx context.Context, opts CaptureOptions) (Meta, error) {
	meta := Meta{Vendor: string(opts.Vendor), Args: opts.Args, Files: []string{}, Skipped: []string{}}
	if _, err := adapter.ParseVendor(string(opts.Vendor)); err != nil {
		return meta, err
	}
	if err := adapter.CheckAbs("fixture dir", opts.Out); err != nil {
		return meta, err
	}
	if _, err := safefs.Lstat(opts.Out); err == nil {
		return meta, fmt.Errorf("devfix: %s already exists", opts.Out)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return meta, err
	}
	bin := opts.Bin
	if bin == "" {
		p, err := probe.LookPath(string(opts.Vendor))
		if err != nil {
			return meta, err
		}
		bin = p
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	run := opts.Run
	if run == nil {
		run = probe.ExecRunner(timeout)
	}
	id := CurrentIdentity()
	if opts.Identity != nil {
		id = *opts.Identity
	}

	cwd, err := safefs.MkdirTemp("", "aggy-capture-")
	if err != nil {
		return meta, err
	}
	defer func() { _ = safefs.RemoveAll(cwd) }()
	roots := map[string]string{"vendor_home": vendorHome(opts.Vendor), "cwd": cwd}
	before := map[string]map[string]stamp{}
	for name, root := range roots {
		before[name] = snapshot(root)
	}

	out, err := run(ctx, adapter.Command{Path: bin, Args: opts.Args, Env: opts.Env, Dir: cwd})
	if err != nil {
		return meta, err
	}
	meta.ExitCode = out.ExitCode
	ids, why := sessionIDs(opts.Vendor, out)

	if err := safefs.MkdirAll(opts.Out, 0o755); err != nil {
		return meta, err
	}
	s := NewScrubber(id)
	for _, name := range []string{"vendor_home", "cwd"} {
		root := roots[name]
		after := snapshot(root)
		keys := make([]string, 0, len(after))
		for k := range after {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, rel := range keys {
			if old, ok := before[name][rel]; ok && old == after[rel] {
				continue
			}
			dest := path.Join(name, s.String(rel))
			if name == "vendor_home" && !sessionFile(opts.Vendor, rel, ids) {
				meta.Skipped = append(meta.Skipped, dest+": not a file of this run's session"+why)
				continue
			}
			if len(meta.Files) >= maxCopiedFiles {
				meta.Skipped = append(meta.Skipped, dest+": too many files")
				continue
			}
			b, err := safefs.ReadFileLimit(filepath.Join(root, filepath.FromSlash(rel)), maxCopiedFile)
			if err != nil {
				meta.Skipped = append(meta.Skipped, dest+": "+s.String(err.Error()))
				continue
			}
			if !IsText(b) {
				meta.Skipped = append(meta.Skipped, dest+": binary")
				continue
			}
			p := filepath.Join(opts.Out, "files", filepath.FromSlash(dest))
			if err := safefs.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return meta, err
			}
			if err := safefs.WriteFile(p, b, 0o644); err != nil {
				return meta, err
			}
			meta.Files = append(meta.Files, dest)
		}
	}
	for name, data := range map[string][]byte{"stdout": out.Stdout, "stderr": out.Stderr} {
		if !IsText(data) {
			meta.Skipped = append(meta.Skipped, name+": binary")
			data = nil
		}
		if err := safefs.WriteFile(filepath.Join(opts.Out, name), data, 0o644); err != nil {
			return meta, err
		}
	}
	mb, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return meta, err
	}
	if err := safefs.WriteFile(filepath.Join(opts.Out, "meta.json"), append(mb, '\n'), 0o644); err != nil {
		return meta, err
	}
	if _, err := ScrubDir(opts.Out, s); err != nil {
		return meta, err
	}
	return meta, nil
}

// ModulePath identifies aggy's repo root by its go.mod.
const ModulePath = "github.com/congdoe/aggy"

// RepoRoot walks up from start to the directory whose go.mod declares
// ModulePath.
func RepoRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		b, err := safefs.ReadFileLimit(filepath.Join(dir, "go.mod"), 1<<16)
		if err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if f := strings.Fields(line); len(f) == 2 && f[0] == "module" && f[1] == ModulePath {
					return dir, nil
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("devfix: not inside the aggy repo (no go.mod for %s above %s)", ModulePath, start)
		}
		dir = parent
	}
}

// FixturesDir is testdata/fixtures under the repo root.
func FixturesDir(root string) string { return filepath.Join(root, "testdata", "fixtures") }

// resolve makes p absolute and resolves symlinks in its longest existing
// prefix.
func resolve(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest), nil
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// UnderFixtures returns p resolved, or an error unless it is strictly
// inside the repo's testdata/fixtures, which, with testdata, must be a
// real directory and not a symlink.
func UnderFixtures(root, p string) (string, error) {
	// A symlinked testdata or testdata/fixtures would lead the limit
	// anywhere, so neither may be a link.
	for _, d := range []string{filepath.Join(root, "testdata"), FixturesDir(root)} {
		fi, err := safefs.Lstat(d)
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			return "", fmt.Errorf("devfix: %s must be a real directory, not a symlink", d)
		}
	}
	fx, err := resolve(FixturesDir(root))
	if err != nil {
		return "", err
	}
	rp, err := resolve(p)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(rp, fx+string(filepath.Separator)) {
		return "", fmt.Errorf("devfix: %s is not under %s", p, FixturesDir(root))
	}
	return rp, nil
}
