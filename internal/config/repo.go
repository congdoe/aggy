package config

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
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
	// CheckMinutes is each check command's time limit; zero means
	// DefaultCheckMinutes.
	CheckMinutes int `toml:"check_minutes"`
	// Worktree configures new git worktrees for edit borrows.
	Worktree Worktree `toml:"worktree"`
	// ToolsOff names managed tools this repo turns off. A repo config
	// has no way to turn a tool on: cloning a repo must never start an
	// MCP server or a hook on the person's machine.
	ToolsOff []string `toml:"tools_off"`
	// OpenCode is a repo's [opencode] table: nothing in it is acted on. Its
	// free_models is read only so the launch can say it is ignored
	// (SetsFreeModels); any other key there is still an unknown key.
	OpenCode RepoOpenCode `toml:"opencode"`
	// Policy is a repo's [policy] table, decoded whatever it holds and never
	// acted on: the safety policy's settings, a group turned off above all
	// (T6979), are the person's own, in ~/.aggy/permissions.json. A launch
	// says it is ignored (SetsPolicy).
	Policy any `toml:"policy"`
}

// RepoOpenCode is a repo's [opencode] table. A repo can never turn on
// OpenCode's free models: that sends prompts and code to OpenCode's own
// service, which only the person's own config can choose.
type RepoOpenCode struct {
	// FreeModels is decoded, whatever its type, and ignored.
	FreeModels any `toml:"free_models"`
}

// SetsFreeModels reports whether the repo's config sets [opencode]
// free_models, which aggy ignores.
func (rc RepoConfig) SetsFreeModels() bool { return rc.OpenCode.FreeModels != nil }

// SetsPolicy reports whether the repo's config has a [policy] table, which
// aggy ignores.
func (rc RepoConfig) SetsPolicy() bool { return rc.Policy != nil }

// toolNameRE is tooldef's tool name rule.
var toolNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// maxToolsOff bounds tools_off; the config comes with a clone.
const maxToolsOff = 64

// Worktree configures new git worktrees.
type Worktree struct {
	// Copy lists ignored files a new worktree copies from the main
	// checkout, such as ".env". Paths are relative to the repo root.
	Copy []string `toml:"copy"`
	// Setup runs in a new worktree after the copy. Empty means none.
	Setup Command `toml:"setup"`
}

// Limits on the check commands a repo config may hold. The config comes
// with a clone, so it is untrusted input.
const (
	DefaultCheckMinutes = 10
	MaxCheckMinutes     = 120
	maxChecks           = 20
	maxCheckArgs        = 64
	maxCheckArg         = 4096
)

// RepoPath returns the repo config path for the repo at root.
func RepoPath(root string) string { return filepath.Join(root, RepoDir, FileName) }

// LoadRepo reads the repo config of the repo at root through safefs: a
// regular file of at most MaxFileSize, not a symlink, and in a .aggy
// directory that is not a symlink (safefs.ReadConfig), so a cloned repo
// cannot point .aggy at another place on the disk. A missing file gives
// the zero RepoConfig, which narrows nothing. Unknown keys are an error.
func LoadRepo(root string) (RepoConfig, error) {
	path := RepoPath(root)
	b, err := safefs.ReadConfig(path, MaxFileSize)
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
	if err := unknownKeys(md, "policy"); err != nil {
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
			// With opencode listed, any other name is an OpenCode provider
			// (Effective.OpenCodeProviders), since OpenCode is one vendor with
			// many destinations. Without it, a name aggy does not know is
			// still a mistake.
			if !slices.Contains(rc.AllowedVendors, adapter.OpenCode) || !providerRE.MatchString(string(v)) {
				return fmt.Errorf("allowed_vendors: %w", err)
			}
		}
		if seen[v] {
			return fmt.Errorf("allowed_vendors: vendor %q listed twice", v)
		}
		seen[v] = true
	}
	if len(rc.Check) > maxChecks {
		return fmt.Errorf("check: at most %d commands", maxChecks)
	}
	for i, c := range rc.Check {
		if err := checkCommand(c); err != nil {
			return fmt.Errorf("check[%d]: %w", i, err)
		}
	}
	if rc.CheckMinutes < 0 || rc.CheckMinutes > MaxCheckMinutes {
		return fmt.Errorf("check_minutes: want 1 to %d, got %d", MaxCheckMinutes, rc.CheckMinutes)
	}
	if len(rc.Worktree.Setup) > 0 && rc.Worktree.Setup[0] == "" {
		return errors.New("worktree.setup: the program is empty")
	}
	for _, p := range rc.Worktree.Copy {
		if err := checkRepoPath(p); err != nil {
			return fmt.Errorf("worktree.copy: %w", err)
		}
	}
	if len(rc.ToolsOff) > maxToolsOff {
		return fmt.Errorf("tools_off: at most %d tools", maxToolsOff)
	}
	for i, n := range rc.ToolsOff {
		// A name aggy does not know is kept: it turns nothing on.
		if !toolNameRE.MatchString(n) {
			return fmt.Errorf("tools_off[%d]: %q is not a tool name", i, n)
		}
	}
	return nil
}

// checkCommand accepts a program and its arguments: the program a bare
// name looked up on PATH, or ./ and a clean path inside the repo; at most
// maxCheckArgs arguments of at most maxCheckArg bytes, with no control or
// format characters.
func checkCommand(c Command) error {
	if len(c) == 0 || c[0] == "" {
		return errors.New("want a program and its arguments")
	}
	if len(c) > maxCheckArgs {
		return fmt.Errorf("at most %d arguments", maxCheckArgs)
	}
	for _, a := range c {
		if len(a) > maxCheckArg {
			return fmt.Errorf("an argument is over %d bytes", maxCheckArg)
		}
		for _, r := range a {
			if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
				return fmt.Errorf("%q holds a control or format character", a)
			}
		}
	}
	if rel, ok := strings.CutPrefix(c[0], "./"); ok {
		if err := checkRepoPath(rel); err != nil {
			return fmt.Errorf("program: %w", err)
		}
		return nil
	}
	if strings.Contains(c[0], "/") || c[0] == "." || c[0] == ".." {
		return fmt.Errorf("program %q: want a bare name found on PATH, or ./ and a path inside the repo", c[0])
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
	// codex, grok, opencode order.
	Allowed []adapter.Vendor
}

// OpenCodeProviders are the providers an OpenCode session may use in this
// repo: the user's list, plus OpenCode's own provider when the user's
// [opencode] free_models is on, narrowed by allowed_vendors, where xai counts
// as grok, openai as codex, opencode as opencode-free, and any other provider
// needs its own name. A repo's own free_models never counts.
func (e Effective) OpenCodeProviders() []string {
	user := slices.Clone(e.OpenCode.Providers)
	if e.OpenCode.FreeModels && !slices.Contains(user, OpenCodeFreeProvider) {
		user = append(user, OpenCodeFreeProvider)
	}
	if e.Repo.AllowedVendors == nil {
		return user
	}
	counts := map[string]adapter.Vendor{"xai": adapter.Grok, "openai": adapter.Codex,
		OpenCodeFreeProvider: OpenCodeFreeRepoName}
	out := []string{}
	for _, p := range user {
		v, ok := counts[p]
		if !ok {
			v = adapter.Vendor(p)
		}
		if slices.Contains(e.Repo.AllowedVendors, v) {
			out = append(out, p)
		}
	}
	return out
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
	allowed := adapter.Known()
	if repo.AllowedVendors != nil {
		allowed = slices.DeleteFunc(adapter.Known(), func(v adapter.Vendor) bool {
			return !slices.Contains(repo.AllowedVendors, v)
		})
	}
	allowed = slices.DeleteFunc(allowed, func(v adapter.Vendor) bool { return user.Vendors.For(v).Off })
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
