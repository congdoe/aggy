// Package devfix records and cleans the vendor CLI fixtures under
// testdata/fixtures, for maintainers. Capture runs a real CLI once through
// the probe runner and records what it printed and wrote; Scrub replaces
// home paths, emails, usernames, hostnames, session UUIDs, and
// token-shaped strings; Detect finds what must never be committed. The
// fixture check test runs Detect over testdata/fixtures.
package devfix

import (
	"bytes"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/congdoe/aggy/internal/safefs"
)

// Placeholders that replace what Scrub removes.
const (
	HomePlaceholder  = "$HOME"
	EmailPlaceholder = "user@example.com"
	UserPlaceholder  = "fixture-user"
	HostPlaceholder  = "fixture-host"
	TokenPlaceholder = "REDACTED-TOKEN"
	// UUIDs become 00000000-0000-4000-8000-<n>, numbered by first
	// appearance, so one session id keeps one placeholder.
	uuidPrefix = "00000000-0000-4000-8000-"
)

// Detector kinds.
const (
	KindHome  = "home path"
	KindEmail = "email"
	KindToken = "token"
)

var (
	// homeRE matches a home directory in macOS, Linux, or Windows form,
	// with its user name.
	homeRE  = regexp.MustCompile(`/Users/[A-Za-z0-9._-]+|/home/[A-Za-z0-9._-]+|/root\b|[A-Za-z]:\\\\?Users\\\\?[A-Za-z0-9._ -]+`)
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	// tokenRE matches token-shaped strings: vendor key prefixes, GitHub,
	// Slack, and AWS tokens, JWTs, bearer tokens, and a secret-named
	// assignment.
	tokenRE = regexp.MustCompile(strings.Join([]string{
		`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}`,
		`\bxai-[A-Za-z0-9_-]{20,}`,
		`\bgh[pousr]_[A-Za-z0-9]{30,}`,
		`\bgithub_pat_[A-Za-z0-9_]{30,}`,
		`\bxox[abprs]-[A-Za-z0-9-]{10,}`,
		`\bAKIA[0-9A-Z]{16}\b`,
		`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`,
		`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{20,}=*`,
		`(?i)["']?\b(?:access_token|refresh_token|id_token|api_key|apikey|secret|password|auth_token)["']?\s*[:=]\s*["']?[A-Za-z0-9._~+/-]{16,}`,
	}, "|"))
	uuidRE = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	// exampleEmail is a documentation address, never personal.
	exampleEmail = regexp.MustCompile(`(?i)@(?:[a-z0-9-]+\.)*example\.(?:com|org|net)$`)
)

// Identity is what Scrub replaces besides the pattern matches: the
// maintainer's own home, user name, and host name.
type Identity struct {
	Home string
	User string
	Host string
}

// Scrubber rewrites fixture text. One Scrubber keeps its UUID numbering
// across every file it scrubs.
type Scrubber struct {
	ID    Identity
	uuids map[string]string
}

// NewScrubber returns a Scrubber for id.
func NewScrubber(id Identity) *Scrubber {
	return &Scrubber{ID: id, uuids: map[string]string{}}
}

// String scrubs one text.
func (s *Scrubber) String(text string) string {
	if h := strings.TrimRight(s.ID.Home, "/"); len(h) > 1 {
		text = strings.ReplaceAll(text, h, HomePlaceholder)
	}
	text = homeRE.ReplaceAllLiteralString(text, HomePlaceholder)
	text = tokenRE.ReplaceAllLiteralString(text, TokenPlaceholder)
	text = emailRE.ReplaceAllStringFunc(text, func(m string) string {
		if exampleEmail.MatchString(m) {
			return m
		}
		return EmailPlaceholder
	})
	text = uuidRE.ReplaceAllStringFunc(text, s.uuid)
	for _, h := range hostNames(s.ID.Host) {
		text = replaceWord(text, h, HostPlaceholder)
	}
	if len(s.ID.User) >= 3 {
		text = replaceWord(text, s.ID.User, UserPlaceholder)
	}
	return text
}

func (s *Scrubber) uuid(m string) string {
	k := strings.ToLower(m)
	if strings.HasPrefix(k, uuidPrefix) {
		return m
	}
	if p, ok := s.uuids[k]; ok {
		return p
	}
	p := fmt.Sprintf("%s%012d", uuidPrefix, len(s.uuids)+1)
	s.uuids[k] = p
	return p
}

// hostNames returns the host name and its short form, ignoring names too
// short or generic to replace safely.
func hostNames(host string) []string {
	var out []string
	for _, h := range []string{host, strings.SplitN(host, ".", 2)[0]} {
		if len(h) >= 3 && h != "localhost" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// replaceWord replaces w where it stands as a whole word, not inside a
// longer name, keeping the characters around it.
func replaceWord(text, w, with string) string {
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(w) + `($|[^A-Za-z0-9_-])`)
	// A match consumes its boundary, so adjacent words need another pass.
	for range 8 {
		out := re.ReplaceAllStringFunc(text, func(m string) string {
			sub := re.FindStringSubmatch(m)
			return sub[1] + with + sub[2]
		})
		if out == text {
			break
		}
		text = out
	}
	return text
}

// MaxFixtureFile caps the size of a fixture file Scrub and Detect read.
const MaxFixtureFile = 8 << 20

// isText reports whether b looks like text: valid UTF-8 with no NUL.
func isText(b []byte) bool { return utf8.Valid(b) && !bytes.Contains(b, []byte{0}) }

// files returns the regular files under root, recursively, as slash paths
// relative to root, sorted. Symlinks are neither followed nor returned.
func files(root string) ([]string, error) {
	var out []string
	var walk func(rel string) error
	walk = func(rel string) error {
		entries, err := safefs.ReadDir(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		for _, e := range entries {
			r := path.Join(rel, e.Name())
			switch {
			case e.IsDir():
				if err := walk(r); err != nil {
					return err
				}
			case e.Type().IsRegular():
				out = append(out, r)
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

// ScrubDir rewrites every text file under dir in place with s, in sorted
// path order, and returns the paths it changed. Binary files are left as
// they are; Detect still reports names that need scrubbing.
func ScrubDir(dir string, s *Scrubber) ([]string, error) {
	names, err := files(dir)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, n := range names {
		p := filepath.Join(dir, filepath.FromSlash(n))
		b, err := safefs.ReadFileLimit(p, MaxFixtureFile)
		if err != nil {
			return changed, err
		}
		if !isText(b) {
			continue
		}
		out := s.String(string(b))
		if out == string(b) {
			continue
		}
		if err := safefs.WriteFile(p, []byte(out), 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, n)
	}
	return changed, nil
}

// Finding is one string a fixture must not hold.
type Finding struct {
	File  string // slash path relative to the fixtures root
	Line  int    // 1-based; 0 for a match in the file's path
	Kind  string // KindHome, KindEmail, or KindToken
	Match string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s %q", f.File, f.Line, f.Kind, f.Match)
}

// Allow is one documented false positive: a literal match, in files
// whose slash path matches the glob.
type Allow struct {
	Glob    string
	Literal string
}

// ParseAllowlist reads an allowlist: one "<path glob><TAB><literal>" per
// line, after an optional "# reason" comment line. Blank lines and lines
// starting with # are ignored.
func ParseAllowlist(b []byte) ([]Allow, error) {
	var out []Allow
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		glob, lit, ok := strings.Cut(line, "\t")
		if !ok || glob == "" || lit == "" {
			return nil, fmt.Errorf("allowlist line %d: want <path glob><TAB><literal>", i+1)
		}
		if _, err := path.Match(glob, ""); err != nil {
			return nil, fmt.Errorf("allowlist line %d: %w", i+1, err)
		}
		out = append(out, Allow{Glob: glob, Literal: lit})
	}
	return out, nil
}

func allowed(allow []Allow, file, match string) bool {
	for _, a := range allow {
		if ok, _ := path.Match(a.Glob, file); ok && a.Literal == match {
			return true
		}
	}
	return false
}

// DetectText returns the home paths, emails, and token-shaped strings in
// text, as findings for file. home, when set, is also a home path.
func DetectText(file, text, home string, allow []Allow) []Finding {
	var out []Finding
	for i, line := range strings.Split(text, "\n") {
		n := i + 1
		add := func(kind string, ms []string) {
			for _, m := range ms {
				if !allowed(allow, file, m) {
					out = append(out, Finding{File: file, Line: n, Kind: kind, Match: m})
				}
			}
		}
		homes := homeRE.FindAllString(line, -1)
		if h := strings.TrimRight(home, "/"); len(h) > 1 && strings.Contains(line, h) {
			homes = append(homes, h)
		}
		add(KindHome, homes)
		var emails []string
		for _, m := range emailRE.FindAllString(line, -1) {
			if !exampleEmail.MatchString(m) {
				emails = append(emails, m)
			}
		}
		add(KindEmail, emails)
		add(KindToken, tokenRE.FindAllString(line, -1))
	}
	return out
}

// Detect checks every file under root: its path and, for text files, its
// content. Binary content is not read for matches, so capture never
// copies a binary that could carry one unscrubbed.
func Detect(root, home string, allow []Allow) ([]Finding, error) {
	names, err := files(root)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, n := range names {
		for _, f := range DetectText(n, n, home, allow) {
			f.Line = 0
			out = append(out, f)
		}
		b, err := safefs.ReadFileLimit(filepath.Join(root, filepath.FromSlash(n)), MaxFixtureFile)
		if err != nil {
			return out, fmt.Errorf("%s: %w", n, err)
		}
		if isText(b) {
			out = append(out, DetectText(n, string(b), home, allow)...)
		}
	}
	return out, nil
}
