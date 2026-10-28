package config

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
	"golang.org/x/text/unicode/norm"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// RepoDir is the per-repo folder, hidden from git through
// .git/info/exclude.
const RepoDir = ".aggy"

// Command is a program and its arguments. It is run directly, never
// through a shell.
type Command []string

// RepoConfig is <repo>/.aggy/config.toml. A repo's config comes with its
// clone, so treat its commands with the same trust as the repo's own
// build files: aggy runs them only where the person asked for checks or a
// worktree.
type RepoConfig struct {
	// AllowedVendors lists the vendors that may see this repo's content.
	// Nil means the key is unset and every vendor is allowed; an empty
	// list allows none.
	AllowedVendors []adapter.Vendor `toml:"allowed_vendors"`
	// Check lists the commands aggy check runs, in order.
	Check []Command `toml:"check"`
	// Worktree configures new git worktrees for edit borrows.
	Worktree Worktree `toml:"worktree"`
}

// Worktree configures new git worktrees.
type Worktree struct {
	// Copy lists ignored files a new worktree copies from the main
	// checkout, such as ".env". Paths are relative to the repo root.
	Copy []string `toml:"copy"`
	// Setup runs in a new worktree after the copy. Empty means none.
	Setup Command `toml:"setup"`
}

// RepoPath returns the repo config path for the repo at root.
func RepoPath(root string) string { return filepath.Join(root, RepoDir, FileName) }

// LoadRepo reads the repo config of the repo at root through safefs: a
// regular file of at most MaxFileSize, not a symlink. A missing file gives
// the zero RepoConfig, which narrows nothing. Unknown keys are an error.
func LoadRepo(root string) (RepoConfig, error) {
	path := RepoPath(root)
	b, err := safefs.ReadFileLimit(path, MaxFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return RepoConfig{}, nil
	}
	if err != nil {
		return RepoConfig{}, err
	}
	rc, err := ParseRepo(string(b))
	if err != nil {
		return RepoConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	return rc, nil
}

// ParseRepo decodes and validates repo config text.
func ParseRepo(text string) (RepoConfig, error) {
	var rc RepoConfig
	md, err := toml.Decode(text, &rc)
	if err != nil {
		return RepoConfig{}, err
	}
	if err := unknownKeys(md); err != nil {
		return RepoConfig{}, err
	}
	if md.IsDefined("allowed_vendors") && rc.AllowedVendors == nil {
		rc.AllowedVendors = []adapter.Vendor{}
	}
	if err := rc.Validate(); err != nil {
		return RepoConfig{}, err
	}
	return rc, nil
}

// Validate checks vendor names, commands, and copy paths.
func (rc RepoConfig) Validate() error {
	seen := map[adapter.Vendor]bool{}
	for _, v := range rc.AllowedVendors {
		if _, err := adapter.ParseVendor(string(v)); err != nil {
			return fmt.Errorf("allowed_vendors: %w", err)
		}
		if seen[v] {
			return fmt.Errorf("allowed_vendors: vendor %q listed twice", v)
		}
		seen[v] = true
	}
	for i, c := range rc.Check {
		if len(c) == 0 || c[0] == "" {
			return fmt.Errorf("check[%d]: want a program and its arguments", i)
		}
	}
	if len(rc.Worktree.Setup) > 0 && rc.Worktree.Setup[0] == "" {
		return errors.New("worktree.setup: the program is empty")
	}
	for _, p := range rc.Worktree.Copy {
		if err := checkRepoPath(p); err != nil {
			return fmt.Errorf("worktree.copy: %w", err)
		}
	}
	return nil
}

// checkRepoPath accepts a clean relative path inside the repo that has no
// .git component at any depth, compared after Unicode NFC normalization
// and case folding (APFS and HFS+ treat ".GIT" and a decomposed spelling
// as ".git"), and no control or format characters (such as zero-width
// joiners, which HFS+ ignores in names).
//
// Validation is only half the defense: the worktree copier (T6595 follow
// up) must open the repo with os.Root and refuse symlinks, so a path that
// passes here cannot be redirected on disk.
func checkRepoPath(p string) error {
	for _, r := range p {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%q holds a control or format character", p)
		}
	}
	switch {
	case p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "/"):
		return fmt.Errorf("%q must be a path relative to the repo root", p)
	case filepath.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../"):
		return fmt.Errorf("%q must be a clean path inside the repo", p)
	}
	for _, c := range strings.Split(p, "/") {
		if strings.EqualFold(norm.NFC.String(c), ".git") {
			return fmt.Errorf("%q is or is inside a .git directory", p)
		}
	}
	return nil
}

// Effective is the user config narrowed by a repo's config.
type Effective struct {
	Config
	Repo RepoConfig
	// Allowed lists the vendors this repo may be sent to, in claude,
	// codex, grok order.
	Allowed []adapter.Vendor
}

// Allows reports whether vendor v may see this repo.
func (e Effective) Allows(v adapter.Vendor) bool { return slices.Contains(e.Allowed, v) }

// Merge narrows user by repo. Repo settings never widen user settings:
//
//   - allowed_vendors removes vendors from the switch order and from every
//     routing list, and clears a default vendor it does not allow (aggy
//     then picks the first connected allowed vendor)
//   - a "not-author" route stays, and the router applies Allowed to it
//   - models and budgets come from the user config only
//   - check commands and worktree settings exist only in repo config
//
// The result's switch order or a route may be empty, which means no
// vendor is available for it in this repo.
func Merge(user Config, repo RepoConfig) Effective {
	allowed := adapter.Vendors()
	if repo.AllowedVendors != nil {
		allowed = slices.DeleteFunc(adapter.Vendors(), func(v adapter.Vendor) bool {
			return !slices.Contains(repo.AllowedVendors, v)
		})
	}
	keep := func(vs []adapter.Vendor) []adapter.Vendor {
		out := []adapter.Vendor{}
		for _, v := range vs {
			if slices.Contains(allowed, v) {
				out = append(out, v)
			}
		}
		return out
	}
	e := Effective{Config: user, Repo: repo, Allowed: allowed}
	if e.DefaultVendor != "" && !slices.Contains(allowed, e.DefaultVendor) {
		e.DefaultVendor = ""
	}
	e.Switch.Order = keep(user.Switch.Order)
	r := &e.Routing
	for _, route := range []*Route{&r.Image, &r.Video, &r.Research, &r.News, &r.Review, &r.Edit} {
		if !route.NotAuthor {
			route.Vendors = keep(route.Vendors)
		}
	}
	return e
}
