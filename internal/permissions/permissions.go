package permissions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"

	"github.com/congdoe/aggy/internal/safefs"
)

// MaxFile bounds each file read.
const MaxFile = 1 << 20

// FileName is aggy's own rules file, in its state folder.
const FileName = "permissions.json"

// Kind is what a rule does. Claude evaluates deny, then ask, then allow
// (permissions.md).
type Kind string

const (
	Allow Kind = "allow"
	Ask   Kind = "ask"
	Deny  Kind = "deny"
)

// Kinds lists them in Claude's order.
var Kinds = []Kind{Deny, Ask, Allow}

// Origin is where a rule came from.
type Origin string

const (
	FromClaudeUser  Origin = "claude-user"    // ~/.claude/settings.json
	FromClaudeLocal Origin = "claude-local"   // <repo>/.claude/settings.local.json
	FromAggy        Origin = "aggy"           // permissions.json, every repo
	FromAggyRepo    Origin = "aggy-repo"      // permissions.json, this repo
	FromConfig      Origin = "aggy-config"    // config.toml's [permissions] mode
	FromGrokGrant   Origin = "grok-approval"  // Grok's sessions/<scope>/permission.toml
	FromCodexGrant  Origin = "codex-approval" // Codex's rules/default.rules
)

// Rule is one rule and its source.
type Rule struct {
	Kind   Kind
	Text   string
	Origin Origin
	// Path is the file it was read from.
	Path string
}

// Skip is a rule that does not apply, and why.
type Skip struct {
	Rule
	Why string
}

// Problem is a source that could not be read.
type Problem struct {
	Path string
	Why  string
	// Repo says a clone could have brought it: the file is tracked, git cannot say,
	// or it or .claude is a link. It costs only that file's allows, or a repo could
	// turn off the person's. Any other problem is the person's own, where a deny
	// may be missing, so it withholds this layer's allows.
	Repo bool
}

// FileError is a mistake in aggy's own rules file, by line where it has one. A
// launch refuses to start on one: a deny there would otherwise be lost, and the
// file is the person's to fix.
type FileError struct {
	Path string
	Line int
	Msg  string
}

func (e *FileError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s line %d: %s; fix it, aggy doctor says more", e.Path, e.Line, e.Msg)
	}
	return fmt.Sprintf("%s: %s; fix it, aggy doctor says more", e.Path, e.Msg)
}

// Set is what one repo gets.
type Set struct {
	// Mode is the default mode in force, in aggy's spelling, and ModeFrom
	// its source. Empty means none is set.
	Mode     string
	ModeFrom Origin
	Rules    []Rule
	Skipped  []Skip
	Problems []Problem
	// Trusted is the repo's trust, for what a CLI's own matching makes of a rule.
	Trusted bool
	// FileErr is a mistake in aggy's own file, which refuses a launch.
	FileErr *FileError
	// Withhold says why no allow from this layer goes out at all: a source could
	// not be read, so a deny in it may be missing. Never fewer denies.
	Withhold string
	// Notes are things to say that change nothing, such as a key aggy skips.
	Notes []string
	// HeldLocal counts allows in a repo's settings.local.json waiting for trust.
	HeldLocal int
}

// Input says where to read from.
type Input struct {
	// ClaudeDir is Claude's config folder ($CLAUDE_CONFIG_DIR or ~/.claude).
	ClaudeDir string
	// AggyFile is ~/.aggy/permissions.json.
	AggyFile string
	// Main is the repo's main checkout, where Claude saves "don't ask again"
	// rules, "resolved through worktrees to the main checkout" (permissions.md).
	// Empty outside a git repo: with no git to ask, a folder's
	// settings.local.json could have come with it, so it is not read.
	Main string
	// RepoKey is Main's key in permissions.json (its canonical path).
	RepoKey string
	// Tracked reports whether git tracks a file. Nil, or an error, counts as
	// tracked: a committed file must never become an allow.
	Tracked func(path string) (bool, error)
	// Trusted says the person trusted this repo (aggy trust).
	Trusted bool
	// GrokHome and CodexHome hold those CLIs' approvals; empty skips them.
	GrokHome, CodexHome string
	// Worktree is this work tree's real path: Grok keeps grants per worktree.
	Worktree string
}

// Object is Claude's permissions object.
type Object struct {
	DefaultMode string   `json:"defaultMode,omitempty"`
	Allow       []string `json:"allow,omitempty"`
	Ask         []string `json:"ask,omitempty"`
	Deny        []string `json:"deny,omitempty"`
}

// claudeFile is the part of a Claude settings file aggy reads.
type claudeFile struct {
	Permissions Object `json:"permissions"`
}

// RepoRules are one repo's rules in aggy's own file.
type RepoRules struct {
	Allow []string `json:"allow,omitempty"`
	Ask   []string `json:"ask,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// AggyFile is ~/.aggy/permissions.json: Claude's object, the same format,
// plus rules kept for one repo, by the main checkout's canonical path.
type AggyFile struct {
	Permissions Object               `json:"permissions"`
	Repos       map[string]RepoRules `json:"repos,omitempty"`
}

// ParseAggyFile decodes the file. A key aggy does not know is no error, since
// $schema and additionalDirectories are valid in this format: UnknownKeys names
// them. Bad JSON, a wrong type, or text after the JSON is an error.
func ParseAggyFile(b []byte) (AggyFile, error) {
	var f AggyFile
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&f); err != nil {
		return AggyFile{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return AggyFile{}, &json.SyntaxError{Offset: dec.InputOffset()}
	}
	return f, nil
}

// UnknownKeys lists keys in the file aggy does not act on. $schema is left out:
// it is only for editors.
func UnknownKeys(b []byte) []string {
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) != nil {
		return nil
	}
	var out []string
	object := func(where string, raw json.RawMessage, known ...string) {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return
		}
		for _, k := range slices.Sorted(maps.Keys(m)) {
			if !slices.Contains(known, k) {
				out = append(out, where+k)
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(top)) {
		if k != "$schema" && k != "permissions" && k != "repos" {
			out = append(out, k)
		}
	}
	object("permissions.", top["permissions"], "defaultMode", "allow", "ask", "deny")
	var repos map[string]json.RawMessage
	if json.Unmarshal(top["repos"], &repos) == nil {
		for _, k := range slices.Sorted(maps.Keys(repos)) {
			object("repos entry: ", repos[k], "allow", "ask", "deny")
		}
	}
	return out
}

// Load reads every source. A mistake in aggy's own file is FileErr, which a
// launch refuses on. A source that cannot be read is a Problem, and with one no
// allow from this layer goes out, as a deny in it may be missing.
func Load(in Input) Set {
	s := Set{Trusted: in.Trusted}
	if in.ClaudeDir == "" {
		// Joined, an empty folder would read the repo's own settings.json as theirs.
		s.Problems = append(s.Problems, Problem{Path: "~/.claude/settings.json", Why: "aggy cannot find Claude's config folder, as HOME is not set, so your rules there went unread"})
	} else {
		s.loadClaude(filepath.Join(in.ClaudeDir, "settings.json"), FromClaudeUser, in.Trusted)
	}
	if in.Main != "" {
		s.loadLocal(in)
	}
	s.loadAggy(in)
	s.loadGrok(in)
	s.loadCodex(in)
	if i := slices.IndexFunc(s.Problems, func(p Problem) bool { return !p.Repo }); i >= 0 {
		s.Withhold = fmt.Sprintf("aggy could not use all of %s, so a deny may be missing, and it sends no allow from its rules until that is fixed", s.Problems[i].Path)
	}
	return s
}

// AggyFileError reads aggy's own file alone, with the repo's section by repoKey as
// a launch does, for a switch to check before it closes the CLI on screen.
func AggyFileError(path, repoKey string) *FileError {
	var s Set
	s.loadAggy(Input{AggyFile: path, RepoKey: repoKey})
	return s.FileErr
}

// followHome resolves a link in the person's own Claude folder, as stow and
// chezmoi make: it is in the home, not the repo. RealPath refuses a credential
// file, and ReadConfig then takes only a regular file, not through a link.
func followHome(path string) (string, error) {
	if _, err := safefs.Lstat(path); err != nil {
		return path, nil //nolint:nilerr // read reports it, or that it is not there
	}
	return safefs.RealPath(path)
}

// read returns a config file, or nil when it is not there.
func (s *Set) read(path string) []byte {
	b, err := safefs.ReadConfig(path, MaxFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		s.Problems = append(s.Problems, Problem{Path: path, Why: err.Error()})
		return nil
	}
	return b
}

func (s *Set) loadClaude(path string, o Origin, trusted bool) {
	from := path
	if o == FromClaudeUser {
		real, err := followHome(path)
		if err != nil {
			s.Problems = append(s.Problems, Problem{Path: path, Why: err.Error()})
			return
		}
		from = real
	}
	b := s.read(from)
	if b == nil {
		return
	}
	var f claudeFile
	if err := json.Unmarshal(b, &f); err != nil {
		s.Problems = append(s.Problems, Problem{Path: path, Why: err.Error()})
		return
	}
	s.addObject(f.Permissions, o, path, trusted)
	if f.Permissions.DefaultMode != "" {
		// Claude's own mode is shown as set. Any value counts: it is the
		// person's, and Claude applies it itself.
		s.Mode, s.ModeFrom = f.Permissions.DefaultMode, o
	}
}

// loadLocal reads <main>/.claude/settings.local.json unless git tracks it. A clone
// can bring only a tracked file or a tracked link, so only those count as the
// repo's: their allows are named as skipped, and a problem with one is Repo. An
// untracked regular file is the person's, with every valid deny and ask kept.
func (s *Set) loadLocal(in Input) {
	path := filepath.Join(in.Main, ".claude", "settings.local.json")
	repos := func(why string) {
		s.Problems = append(s.Problems, Problem{Path: path, Why: why, Repo: true})
	}
	dir, err := safefs.Lstat(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	file, ferr := safefs.Lstat(path)
	if errors.Is(ferr, fs.ErrNotExist) {
		return
	}
	switch {
	case (err == nil && dir.Mode()&fs.ModeSymlink != 0) || (ferr == nil && file.Mode()&fs.ModeSymlink != 0) || errors.Is(ferr, safefs.ErrAuthPath):
		repos("this file or its folder is a link, which a clone could bring")
		return
	case err != nil:
		s.Problems = append(s.Problems, Problem{Path: path, Why: err.Error()})
		return
	case ferr != nil:
		s.Problems = append(s.Problems, Problem{Path: path, Why: ferr.Error()})
		return
	}
	why := ""
	if in.Tracked == nil {
		why = "aggy cannot tell whether git tracks this file, so it counts as committed"
	} else if tracked, err := in.Tracked(path); err != nil {
		why = "aggy cannot tell whether git tracks this file, so it counts as committed"
	} else if tracked {
		why = "git tracks this file, so a clone would bring it: committed rules never become allows"
	}
	if why == "" {
		s.loadClaude(path, FromClaudeLocal, in.Trusted)
		return
	}
	b, err := safefs.ReadConfig(path, MaxFile)
	if err != nil {
		repos(err.Error())
		return
	}
	var f claudeFile
	if err := json.Unmarshal(b, &f); err != nil {
		repos(err.Error())
		return
	}
	for _, r := range f.Permissions.Allow {
		s.Skipped = append(s.Skipped, Skip{Rule{Allow, r, FromClaudeLocal, path}, why})
	}
}

func (s *Set) loadAggy(in Input) {
	if in.AggyFile == "" {
		return
	}
	b, err := safefs.ReadConfig(in.AggyFile, MaxFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return
	case err != nil:
		s.FileErr = &FileError{in.AggyFile, 0, "aggy cannot read this file: " + err.Error()}
		return
	}
	f, err := ParseAggyFile(b)
	if err != nil {
		e := decodeError(b, err)
		s.FileErr = &FileError{in.AggyFile, e.Line, e.Msg}
		return
	}
	for _, k := range UnknownKeys(b) {
		s.Notes = append(s.Notes, fmt.Sprintf("%s: aggy does not use %s", FileName, k))
	}
	s.addObject(f.Permissions, FromAggy, in.AggyFile, in.Trusted)
	if m := f.Permissions.DefaultMode; m != "" {
		if mode, err := CheckMode(m); err != nil {
			s.Notes = append(s.Notes, FileName+": "+err.Error())
		} else {
			s.Mode, s.ModeFrom = mode, FromAggy
		}
	}
	if in.RepoKey == "" {
		return
	}
	// The file is edited by hand, so a key may be spelled another way than
	// the canonical path, such as with capitals on a file system that folds case.
	for _, key := range slices.Sorted(maps.Keys(f.Repos)) {
		if key == in.RepoKey || canonical(key) == in.RepoKey {
			r := f.Repos[key]
			s.addObject(Object{Allow: r.Allow, Ask: r.Ask, Deny: r.Deny}, FromAggyRepo, in.AggyFile, in.Trusted)
		}
	}
}

func canonical(p string) string {
	c, err := safefs.CanonicalPath(p)
	if err != nil {
		return ""
	}
	return c
}

// addObject adds a file's rules: denies and asks as they are, since they only
// narrow, and allows through the filter.
func (s *Set) addObject(o Object, from Origin, path string, trusted bool) {
	s.addNarrowing(Deny, o.Deny, from, path)
	s.addNarrowing(Ask, o.Ask, from, path)
	for _, r := range o.Allow {
		s.AddAllow(Rule{Allow, r, from, path}, trusted)
	}
}

// addNarrowing adds denies or asks. One that is not a rule is a person's intent
// aggy cannot carry out, so it counts like a file aggy could not read.
func (s *Set) addNarrowing(k Kind, rules []string, from Origin, path string) {
	for _, r := range rules {
		rule := Rule{k, r, from, path}
		if _, _, _, err := ParseRule(r); err != nil {
			s.Skipped = append(s.Skipped, Skip{rule, err.Error()})
			why := fmt.Sprintf("the %s rule %q is not a rule", k, r)
			if from == FromAggy || from == FromAggyRepo {
				if s.FileErr == nil {
					s.FileErr = &FileError{path, 0, why}
				}
			} else {
				s.Problems = append(s.Problems, Problem{Path: path, Why: why})
			}
			continue
		}
		s.add(rule)
	}
}

// AddAllow passes an allow through the filter. An allow from a repo's
// settings.local.json also waits for trust whatever it says: an archive with a
// planted .git, or a name git and the file system disagree on, can make a
// committed file look like the person's. Claude reads that file itself, behind
// its own trust step, and needs nothing from aggy for it.
func (s *Set) AddAllow(r Rule, trusted bool) {
	class, why := CheckAllow(r.Text)
	switch {
	case class == Drop:
		s.Skipped = append(s.Skipped, Skip{r, why})
	case r.Origin == FromClaudeLocal && !trusted:
		s.HeldLocal++
		s.Skipped = append(s.Skipped, Skip{r, "a repo's settings.local.json could have come with the repo, so its allows wait for trust (aggy trust <dir>)"})
	case class == RepoCode && !trusted:
		s.Skipped = append(s.Skipped, Skip{r, fmt.Sprintf("%s (aggy trust <dir>)", why)})
	default:
		s.add(r)
	}
}

func (s *Set) add(r Rule) {
	if !slices.Contains(s.Rules, r) {
		s.Rules = append(s.Rules, r)
	}
}
