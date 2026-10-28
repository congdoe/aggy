// Package devfix records and cleans the vendor CLI fixtures under
// testdata/fixtures, for maintainers. Capture runs a real CLI once through
// the process runner (internal/execrun) and records what it printed and the text files of its
// session; Scrub replaces home paths (also URL-encoded and in Claude's
// dash form), emails, usernames, hostnames, session UUIDs, and
// token-shaped strings; Detect finds what must never be committed. The
// fixture check test runs Detect over testdata/fixtures.
package devfix

import (
	"bytes"
	"fmt"
	"net/url"
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
	// UUIDPrefix starts every UUID placeholder: UUIDs become
	// 00000000-0000-4000-8000-<n>, numbered by first appearance, so one
	// session id keeps one placeholder.
	UUIDPrefix = "00000000-0000-4000-8000-"
)

// Finding kinds.
const (
	KindHome   = "home path"
	KindEmail  = "email"
	KindToken  = "token"
	KindUser   = "user name"
	KindHost   = "host name"
	KindUUID   = "uuid"
	KindBinary = "binary file"
)

// BinaryLiteral is the allowlist literal that lets a binary file through
// the check.
const BinaryLiteral = "(binary)"

var (
	// homeRE matches a home directory in macOS, Linux, or Windows form,
	// with its user name.
	homeRE = regexp.MustCompile(`/Users/[A-Za-z0-9._-]+|/home/[A-Za-z0-9._-]+|/root\b|[A-Za-z]:\\\\?Users\\\\?[A-Za-z0-9._ -]+`)
	// encHomeRE matches a URL-encoded home, as in Grok's session folders.
	// The escape may itself be escaped: %252F.
	encHomeRE = regexp.MustCompile(`(?i)%(?:25)*2F(?:Users|home)%(?:25)*2F[A-Za-z0-9._-]+|%(?:25)*2Froot\b`)
	// jsonHomeRE matches a home with JSON-escaped slashes.
	jsonHomeRE = regexp.MustCompile(`\\/(?:Users|home)\\/[A-Za-z0-9._-]+|\\/root\b`)
	// dashHomeRE matches Claude's project-folder form of a home path,
	// "-Users-name-proj", at the start of a path element or word.
	dashHomeRE = regexp.MustCompile(`(^|[/"'\s=:])-(?:Users|home)-[A-Za-z0-9._]+`)
	emailRE    = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	// tokenRE matches token-shaped strings.
	tokenRE = regexp.MustCompile(strings.Join([]string{
		`\bsk-[A-Za-z0-9_-]{16,}`,                                            // OpenAI and Anthropic keys
		`\bxai-[A-Za-z0-9_-]{16,}`,                                           // xAI keys
		`\bgh[pousr]_[A-Za-z0-9]{20,}`,                                       // GitHub tokens
		`\bgithub_pat_[A-Za-z0-9_]{20,}`,                                     // GitHub fine-grained tokens
		`\bxox[abprs]-[A-Za-z0-9-]{10,}`,                                     // Slack tokens
		`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`,                                      // AWS access key ids
		`(?i)aws_?secret_?access_?key["']?\s*[:=]\s*["']?[A-Za-z0-9/+=]{40}`, // AWS secret keys
		`\bAIza[0-9A-Za-z_-]{35}`,                                            // Google API keys
		`\bya29\.[0-9A-Za-z_-]{10,}`,                                         // Google OAuth tokens
		`\b[rs]k_(?:live|test)_[0-9A-Za-z]{10,}`,                             // Stripe keys
		`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`,                              // PEM private keys
		`\beyJ[A-Za-z0-9_-]{2,}\.[A-Za-z0-9_-]{2,}\.[A-Za-z0-9_-]*`,          // JWTs, signed or not
		`(?i)\bauthorization\s*:\s*basic\s+[A-Za-z0-9+/=]{6,}`,               // HTTP basic auth
		`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{16,}=*`,                            // bearer tokens
		`(?i)["']?\b(?:access_token|refresh_token|id_token|api_key|apikey|secret|password|auth_token)["']?\s*[:=]\s*["']?[A-Za-z0-9._~+/-]{16,}`,
	}, "|"))
	uuidRE = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	// exampleEmail is a documentation address, never personal.
	exampleEmail = regexp.MustCompile(`(?i)@(?:[a-z0-9-]+\.)*example\.(?:com|org|net)$`)
)

// Identity is what Scrub replaces and Detect flags besides the pattern
// matches: the maintainer's own home, user name, and host name.
type Identity struct {
	Home string
	User string
	Host string
}

// dashRE matches what Claude turns into "-" in a project folder name.
var dashRE = regexp.MustCompile(`[^A-Za-z0-9]`)

// homeForms returns the identity's home, and /Users/<user> and
// /home/<user>, as written, URL-encoded, and in Claude's dash form (every
// non-alphanumeric becomes "-", so "/Users/john.doe" is
// "-Users-john-doe"), longest first.
func (id Identity) homeForms() []string {
	var homes []string
	if h := strings.TrimRight(id.Home, "/"); len(h) > 1 {
		homes = append(homes, h)
	}
	if id.User != "" && !strings.ContainsAny(id.User, "/\\") {
		homes = append(homes, "/Users/"+id.User, "/home/"+id.User)
	}
	var forms []string
	for _, h := range homes {
		for _, f := range []string{h, url.QueryEscape(h), dashRE.ReplaceAllString(h, "-")} {
			if !slices.Contains(forms, f) {
				forms = append(forms, f)
			}
		}
	}
	slices.SortStableFunc(forms, func(a, b string) int { return len(b) - len(a) })
	return forms
}

// genericNames are account and host names that are ordinary words or
// shared defaults (CI runners, containers, codespaces), so they identify
// no one on their own.
var genericNames = []string{"runner", "root", "ubuntu", "user", "admin", "vscode", "codespace", "build", "localhost"}

// identifying reports whether a user or host name identifies someone on
// its own: at least 3 characters and not a generic name.
func identifying(name string) bool {
	return len(name) >= 3 && !slices.ContainsFunc(genericNames, func(g string) bool { return strings.EqualFold(g, name) })
}

// words returns the user name and host names to match. The user name is
// matched only in identifying contexts (see userREs), never as a bare
// word, since many account names are ordinary words; the host names are
// matched as whole words. Generic and short names are left out.
func (id Identity) words() (user string, hosts []string) {
	if identifying(id.User) {
		user = id.User
	}
	for _, h := range []string{id.Host, strings.SplitN(id.Host, ".", 2)[0]} {
		if identifying(h) && !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	return user, hosts
}

// userREs match user in the forms that name an account rather than use
// a word: "user@host" or an email's local part, "~user" (a home in shell
// and path form), and "/Volumes/<user>" (an external disk named after the
// account; also URL-encoded and JSON-escaped). Each captures the text
// before and after the name, which a replacement keeps. A user name in prose ("ann wrote this") is not
// matched: many account names are ordinary words, so that is a known
// limit, and generic names are never matched at all (see identifying).
func userREs(user string) []*regexp.Regexp {
	u := regexp.QuoteMeta(user)
	// After the name: the end, a character no name holds, or a dot that
	// ends a sentence (followed by the end or by a character no name
	// holds). A dot followed by a name character continues another,
	// dotted name (macOS allows "ann.doe").
	end := `($|[^A-Za-z0-9_.-]|\.(?:$|[^A-Za-z0-9_-]))`
	return []*regexp.Regexp{
		regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.+-])` + u + `(@)`),
		regexp.MustCompile(`(?i)(^~|[^A-Za-z0-9_.+~-]~|%(?:25)*7E)` + u + end),
		regexp.MustCompile(`(?i)(/Volumes/|\\/Volumes\\/|%(?:25)*2FVolumes%(?:25)*2F)` + u + end),
	}
}

// matchesUser reports whether line names user in one of userREs' forms.
func matchesUser(user, line string) bool {
	return slices.ContainsFunc(userREs(user), func(re *regexp.Regexp) bool { return re.MatchString(line) })
}

// replaceUser replaces user in userREs' forms with with.
func replaceUser(text, user, with string) string {
	for _, re := range userREs(user) {
		// A match consumes its boundary, so adjacent names need another
		// pass.
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
	}
	return text
}

// decoded returns line with URL escapes decoded until stable (so %252F
// becomes "/") and JSON's escaped "\/" as "/".
func decoded(line string) string {
	for range 5 {
		d, err := url.PathUnescape(line)
		if err != nil || d == line {
			break
		}
		line = d
	}
	return strings.ReplaceAll(line, `\/`, "/")
}

// wordRE matches w case-insensitively as a whole word, not inside a
// longer name, capturing the characters around it.
func wordRE(w string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(w) + `($|[^A-Za-z0-9_-])`)
}

// replaceWord replaces w where it stands as a whole word.
func replaceWord(text, w, with string) string {
	re := wordRE(w)
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
	for _, f := range s.ID.homeForms() {
		text = replaceFold(text, f, HomePlaceholder)
	}
	text = homeRE.ReplaceAllLiteralString(text, HomePlaceholder)
	text = encHomeRE.ReplaceAllLiteralString(text, HomePlaceholder)
	text = jsonHomeRE.ReplaceAllLiteralString(text, HomePlaceholder)
	text = dashHomeRE.ReplaceAllStringFunc(text, func(m string) string {
		return dashHomeRE.FindStringSubmatch(m)[1] + HomePlaceholder
	})
	text = tokenRE.ReplaceAllLiteralString(text, TokenPlaceholder)
	text = emailRE.ReplaceAllStringFunc(text, func(m string) string {
		if exampleEmail.MatchString(m) {
			return m
		}
		return EmailPlaceholder
	})
	text = uuidRE.ReplaceAllStringFunc(text, s.uuid)
	user, hosts := s.ID.words()
	for _, h := range hosts {
		text = replaceWord(text, h, HostPlaceholder)
	}
	if user != "" {
		text = replaceUser(text, user, UserPlaceholder)
	}
	return text
}

// replaceFold replaces every case-insensitive occurrence of old.
func replaceFold(text, old, with string) string {
	return regexp.MustCompile(`(?i)`+regexp.QuoteMeta(old)).ReplaceAllLiteralString(text, with)
}

func (s *Scrubber) uuid(m string) string {
	k := strings.ToLower(m)
	if strings.HasPrefix(k, UUIDPrefix) {
		return m
	}
	if p, ok := s.uuids[k]; ok {
		return p
	}
	p := fmt.Sprintf("%s%012d", UUIDPrefix, len(s.uuids)+1)
	s.uuids[k] = p
	return p
}

// MaxFixtureFile caps the size of a fixture file Scrub and Detect read.
const MaxFixtureFile = 8 << 20

// IsText reports whether b looks like text: valid UTF-8 with no NUL.
func IsText(b []byte) bool { return utf8.Valid(b) && !bytes.Contains(b, []byte{0}) }

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
// path order, and returns the paths it changed. It reads and scrubs every
// file before writing any, so a refused or oversized file changes
// nothing. Binary files are left as they are, and Detect fails them.
func ScrubDir(dir string, s *Scrubber) ([]string, error) {
	names, err := files(dir)
	if err != nil {
		return nil, err
	}
	type change struct {
		name string
		data []byte
	}
	var changes []change
	for _, n := range names {
		b, err := safefs.ReadFileLimit(filepath.Join(dir, filepath.FromSlash(n)), MaxFixtureFile)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		if !IsText(b) {
			continue
		}
		if out := s.String(string(b)); out != string(b) {
			changes = append(changes, change{n, []byte(out)})
		}
	}
	var changed []string
	for _, c := range changes {
		if err := safefs.WriteFile(filepath.Join(dir, filepath.FromSlash(c.name)), c.data, 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, c.name)
	}
	return changed, nil
}

// Finding is one string a fixture must not hold.
type Finding struct {
	File  string // slash path relative to the fixtures root
	Line  int    // 1-based; 0 for a match in the file's path or a binary file
	Kind  string
	Match string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s %q", f.File, f.Line, f.Kind, f.Match)
}

// Allow is one documented false positive: a literal match, in files
// whose slash path matches the glob. The literal BinaryLiteral allows a
// binary file.
type Allow struct {
	Glob    string
	Literal string
}

// ParseAllowlist reads an allowlist: one "<path glob><TAB><literal>" per
// line. Blank lines and lines starting with # are ignored.
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

// DetectText returns what text must not hold, as findings for file: home
// paths (also URL-encoded, decoded until stable, JSON-escaped, and in
// Claude's dash form), emails, token-shaped strings, the identity's user
// name as user@host, an email local part, ~user, or /Volumes/<user>, its
// host names, and UUIDs
// other than the scrub placeholders. A token wrapped in base64 is not
// found: a known limit.
func DetectText(file, text string, id Identity, allow []Allow) []Finding {
	user, hosts := id.words()
	var out []Finding
	for i, line := range strings.Split(text, "\n") {
		n := i + 1
		add := func(kind string, ms ...string) {
			for _, m := range ms {
				if !allowed(allow, file, m) {
					out = append(out, Finding{File: file, Line: n, Kind: kind, Match: m})
				}
			}
		}
		views := []string{line}
		if d := decoded(line); d != line {
			views = append(views, d)
		}
		var homes []string
		for _, v := range views {
			homes = append(homes, homeRE.FindAllString(v, -1)...)
		}
		homes = append(homes, encHomeRE.FindAllString(line, -1)...)
		homes = append(homes, jsonHomeRE.FindAllString(line, -1)...)
		for _, m := range dashHomeRE.FindAllStringSubmatch(line, -1) {
			homes = append(homes, strings.TrimPrefix(m[0], m[1]))
		}
		for _, f := range id.homeForms() {
			if strings.Contains(strings.ToLower(line), strings.ToLower(f)) {
				homes = append(homes, f)
			}
		}
		add(KindHome, slices.Compact(homes)...)
		for _, m := range emailRE.FindAllString(line, -1) {
			if !exampleEmail.MatchString(m) {
				add(KindEmail, m)
			}
		}
		add(KindToken, tokenRE.FindAllString(line, -1)...)
		for _, m := range uuidRE.FindAllString(line, -1) {
			if !strings.HasPrefix(strings.ToLower(m), UUIDPrefix) {
				add(KindUUID, m)
			}
		}
		if user != "" && slices.ContainsFunc(views, func(v string) bool { return matchesUser(user, v) }) {
			add(KindUser, user)
		}
		for _, h := range hosts {
			if wordRE(h).MatchString(line) {
				add(KindHost, h)
			}
		}
	}
	return out
}

// Detect checks every file under root: its path, and its content when it
// is text. A binary file (a NUL byte, or not valid UTF-8) is itself a
// finding unless the allowlist names it with BinaryLiteral, since Scrub
// cannot clean it and its bytes are not checked.
func Detect(root string, id Identity, allow []Allow) ([]Finding, error) {
	names, err := files(root)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, n := range names {
		for _, f := range DetectText(n, n, id, allow) {
			f.Line = 0
			out = append(out, f)
		}
		b, err := safefs.ReadFileLimit(filepath.Join(root, filepath.FromSlash(n)), MaxFixtureFile)
		if err != nil {
			return out, fmt.Errorf("%s: %w", n, err)
		}
		if !IsText(b) {
			if !allowed(allow, n, BinaryLiteral) {
				out = append(out, Finding{File: n, Kind: KindBinary, Match: BinaryLiteral})
			}
			continue
		}
		out = append(out, DetectText(n, string(b), id, allow)...)
	}
	return out, nil
}
