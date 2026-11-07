package permissions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	FromClaudeUser  Origin = "claude-user"  // ~/.claude/settings.json
	FromClaudeLocal Origin = "claude-local" // <repo>/.claude/settings.local.json
	FromAggy        Origin = "aggy"         // permissions.json, every repo
	FromAggyRepo    Origin = "aggy-repo"    // permissions.json, this repo
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

// Problem is a source that could not be read, or a mode not passed on.
type Problem struct {
	Path string
	Why  string
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

// ParseAggyFile decodes the file, refusing a key aggy does not know: a
// misspelled "deny" must not leave someone thinking a rule holds.
func ParseAggyFile(b []byte) (AggyFile, error) {
	var f AggyFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return AggyFile{}, err
	}
	return f, nil
}

// Load reads every source. It never fails: what cannot be read is a Problem,
// and its rules do not apply.
func Load(in Input) Set {
	var s Set
	s.loadClaude(filepath.Join(in.ClaudeDir, "settings.json"), FromClaudeUser, in.Trusted)
	if in.Main != "" {
		s.loadLocal(in)
	}
	s.loadAggy(in)
	return s
}

// read returns a config file, or nil when it is not there.
func (s *Set) read(path string) []byte {
	b, err := safefs.ReadConfig(path, MaxFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		s.Problems = append(s.Problems, Problem{path, err.Error()})
		return nil
	}
	return b
}

func (s *Set) loadClaude(path string, o Origin, trusted bool) {
	b := s.read(path)
	if b == nil {
		return
	}
	var f claudeFile
	if err := json.Unmarshal(b, &f); err != nil {
		s.Problems = append(s.Problems, Problem{path, err.Error()})
		return
	}
	s.addObject(f.Permissions, o, path, trusted)
	if f.Permissions.DefaultMode != "" {
		// Claude's own mode is shown as set. Any value counts: it is the
		// person's, and Claude applies it itself.
		s.Mode, s.ModeFrom = f.Permissions.DefaultMode, o
	}
}

// loadLocal reads <main>/.claude/settings.local.json unless git tracks it. A
// tracked file's allows are named as skipped, so the person can see why.
func (s *Set) loadLocal(in Input) {
	path := filepath.Join(in.Main, ".claude", "settings.local.json")
	if _, err := safefs.Lstat(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.Problems = append(s.Problems, Problem{path, err.Error()})
		}
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
	b := s.read(path)
	if b == nil {
		return
	}
	var f claudeFile
	if err := json.Unmarshal(b, &f); err != nil {
		s.Problems = append(s.Problems, Problem{path, err.Error()})
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
	b := s.read(in.AggyFile)
	if b == nil {
		return
	}
	f, err := ParseAggyFile(b)
	if err != nil {
		s.Problems = append(s.Problems, Problem{in.AggyFile, err.Error()})
		return
	}
	s.addObject(f.Permissions, FromAggy, in.AggyFile, in.Trusted)
	if m := f.Permissions.DefaultMode; m != "" {
		if mode, err := CheckMode(m); err != nil {
			s.Problems = append(s.Problems, Problem{in.AggyFile, err.Error()})
		} else {
			s.Mode, s.ModeFrom = mode, FromAggy
		}
	}
	if r, ok := f.Repos[in.RepoKey]; ok && in.RepoKey != "" {
		s.addObject(Object{Allow: r.Allow, Ask: r.Ask, Deny: r.Deny}, FromAggyRepo, in.AggyFile, in.Trusted)
	}
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

func (s *Set) addNarrowing(k Kind, rules []string, from Origin, path string) {
	for _, r := range rules {
		rule := Rule{k, r, from, path}
		if _, _, _, err := ParseRule(r); err != nil {
			s.Skipped = append(s.Skipped, Skip{rule, err.Error()})
			continue
		}
		s.add(rule)
	}
}

// AddAllow passes an allow through the filter.
func (s *Set) AddAllow(r Rule, trusted bool) {
	switch class, why := CheckAllow(r.Text); {
	case class == Drop:
		s.Skipped = append(s.Skipped, Skip{r, why})
	case class == RepoCode && !trusted:
		s.Skipped = append(s.Skipped, Skip{r, fmt.Sprintf("%s, and this repo is not trusted (aggy trust <dir>)", why)})
	default:
		s.add(r)
	}
}

func (s *Set) add(r Rule) {
	if !slices.Contains(s.Rules, r) {
		s.Rules = append(s.Rules, r)
	}
}
