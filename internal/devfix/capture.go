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
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
)

// Limits on what Capture copies from the directories it watches.
const (
	maxSnapshotEntries = 200_000
	maxCopiedFiles     = 200
	maxCopiedFile      = 1 << 20
)

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
	// Identity is what the scrub replaces besides pattern matches. The
	// zero value means the current user's.
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

// Capture runs the CLI once and records a fixture in opts.Out: stdout,
// stderr, meta.json, and under files/vendor_home/ and files/cwd/ the files
// it created or changed in its vendor home and in its working dir (a
// fresh temp dir). Then it scrubs the fixture. It is for maintainers: it
// runs whatever CLI Bin names, which may spend quota.
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

	if err := safefs.MkdirAll(opts.Out, 0o755); err != nil {
		return meta, err
	}
	s := NewScrubber(id)
	for _, name := range []string{"vendor_home", "cwd"} {
		root := roots[name]
		after := snapshot(root)
		for _, rel := range sortedKeys(after) {
			if old, ok := before[name][rel]; ok && old == after[rel] {
				continue
			}
			dest := path.Join(name, s.String(rel))
			if len(meta.Files) >= maxCopiedFiles {
				meta.Skipped = append(meta.Skipped, dest+": too many files")
				continue
			}
			b, err := safefs.ReadFileLimit(filepath.Join(root, filepath.FromSlash(rel)), maxCopiedFile)
			if err != nil {
				meta.Skipped = append(meta.Skipped, dest+": "+s.String(err.Error()))
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

func sortedKeys(m map[string]stamp) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
