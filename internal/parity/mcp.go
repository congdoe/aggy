package parity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/redact"
	"github.com/congdoe/aggy/internal/safefs"
)

// MCP servers: aggy copies the public MCP servers of Claude's user scope
// into Codex and Grok. Sources, all at the tested versions:
//
//   - Claude keeps user-scope servers in ~/.claude.json (Claude docs,
//     mcp.md, "MCP installation scopes"), as a top-level "mcpServers"
//     object, and local-scope ones under "projects"/<path>/"mcpServers";
//     the key names are those Grok reads from that file (grok-build
//     2bdd1d6a, xai-grok-config/src/mcp_servers/json_config.rs:235-244,
//     :399-402). An entry has "type" (stdio when absent, "http" or its
//     alias "streamable-http", "sse", "ws", "sdk"), "command", "args",
//     "env", "url", "headers", "headersHelper", "oauth" (mcp.md). Where
//     Claude keeps them under CLAUDE_CONFIG_DIR is unverified, so aggy
//     reads nothing then. claude.ai connectors are not in the file.
//   - Codex: [mcp_servers.<name>] in $CODEX_HOME/config.toml or
//     ~/.codex/config.toml, with command and args (stdio) or url
//     (streamable HTTP; Codex has no SSE transport) (codex 6aaac69,
//     codex-rs/config/src/config_toml.rs#L299,
//     codex-rs/config/src/mcp_types.rs#L373-L389 and the TryFrom below
//     it). codex mcp login <name> runs Codex's own OAuth sign-in
//     (codex-rs/cli/src/mcp_cmd.rs#L62, #L79; and codex mcp --help).
//   - Grok: [mcp_servers.<name>] in $GROK_HOME/config.toml or
//     ~/.grok/config.toml, with command and args, or url, and type =
//     "sse" for SSE; a url ending in /sse is SSE whatever the type
//     (grok-build 2bdd1d6a, xai-grok-config/src/mcp_server_config.rs:28-59,
//     :499-502; xai-grok-pager/docs/user-guide/07-mcp-servers.md). Grok
//     also loads ~/.claude.json's servers itself unless [compat.claude]
//     mcps = false or GROK_CLAUDE_MCPS_ENABLED turns that off (same
//     guide, "Compatibility"); config.toml wins on a name clash.
//   - enabled = false makes Codex skip a server (mcp_types.rs#L240-L242,
//     default true) and Grok too (07-mcp-servers.md, stdio example).
//
// Only user-scope servers are copied: Codex and Grok config is global, so
// a local-scope server would reach every project, and a repo's .mcp.json
// must never start a server. Nothing that may hold a secret is copied:
// an entry with env, headers, a headers helper, or OAuth settings, an
// argument or URL that looks like a secret or names one, a URL with a
// query or user info, or a ${VAR} reference is listed for the user to add
// by hand. Server values are never printed.

// MCPServer is one MCP server aggy can copy.
type MCPServer struct {
	Name string
	// Type is "stdio", "http" (streamable HTTP), or "sse".
	Type    string
	Command string
	Args    []string
	URL     string
}

// MCPSkip is a server aggy does not copy, and why.
type MCPSkip struct {
	Name string
	Why  string
}

// maxClaudeJSON caps ~/.claude.json, which also holds Claude's history
// of projects.
const maxClaudeJSON = 32 << 20

// maxVendorTOML caps a Codex or Grok config.toml.
const maxVendorTOML = 4 << 20

var serverName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// secretWord marks an argument that may name or carry a secret.
var secretWord = regexp.MustCompile(`(?i)token|secret|passw|api[-_]?key|auth|bearer|credential|private[-_]?key|cookie|session`)

// ClaudeJSONPath is ~/.claude.json. With CLAUDE_CONFIG_DIR set it is an
// error: where Claude keeps MCP servers then is unverified.
func ClaudeJSONPath(getenv func(string) string) (string, error) {
	if getenv("CLAUDE_CONFIG_DIR") != "" {
		return "", errors.New("CLAUDE_CONFIG_DIR is set, and where Claude keeps MCP servers then is unverified")
	}
	h := getenv("HOME")
	if !filepath.IsAbs(h) {
		return "", errors.New("cannot find the home directory")
	}
	return filepath.Join(h, ".claude.json"), nil
}

// MCPConfigPath is the config.toml that holds Codex's or Grok's MCP
// servers: $CODEX_HOME or $GROK_HOME, else ~/.codex or ~/.grok.
func MCPConfigPath(v adapter.Vendor, getenv func(string) string) (string, error) {
	loc := map[adapter.Vendor][2]string{adapter.Codex: {"CODEX_HOME", ".codex"}, adapter.Grok: {"GROK_HOME", ".grok"}}[v]
	env, dir := loc[0], loc[1]
	if env == "" {
		return "", fmt.Errorf("aggy wires no MCP config for %s", v)
	}
	if d := getenv(env); d != "" {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("%s=%q is not an absolute path", env, d)
		}
		return filepath.Join(d, "config.toml"), nil
	}
	h := getenv("HOME")
	if !filepath.IsAbs(h) {
		return "", errors.New("cannot find the home directory")
	}
	return filepath.Join(h, dir, "config.toml"), nil
}

// claudeEntry is one mcpServers entry of ~/.claude.json.
type claudeEntry struct {
	Type          string            `json:"type"`
	Command       string            `json:"command"`
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env"`
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers"`
	HeadersHelper string            `json:"headersHelper"`
	OAuth         json.RawMessage   `json:"oauth"`
}

// claudeFile is the part of ~/.claude.json aggy reads.
type claudeFile struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
	Projects   map[string]struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	} `json:"projects"`
}

// readClaude reads ~/.claude.json: a regular file, not a symlink. A
// missing file is the zero claudeFile.
func readClaude(path string) (claudeFile, error) {
	var f claudeFile
	b, err := safefs.ReadFileLimit(path, maxClaudeJSON)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return f, nil
}

// ReadClaudeMCP returns Claude's user-scope MCP servers at path
// (ClaudeJSONPath) that aggy can copy, sorted by name, and the ones it
// will not copy, with why.
func ReadClaudeMCP(path string) ([]MCPServer, []MCPSkip, error) {
	f, err := readClaude(path)
	if err != nil {
		return nil, nil, err
	}
	var out []MCPServer
	var skips []MCPSkip
	for _, name := range slices.Sorted(maps.Keys(f.MCPServers)) {
		s, why := claudeServer(name, f.MCPServers[name])
		if why != "" {
			// A name that is not plain is not shown either.
			if !serverName.MatchString(name) {
				name = "(a server with an unusual name)"
			}
			skips = append(skips, MCPSkip{Name: name, Why: why})
			continue
		}
		out = append(out, s)
	}
	return out, skips, nil
}

// claudeServer checks one entry. A non-empty why means it is not copied.
func claudeServer(name string, raw json.RawMessage) (MCPServer, string) {
	if !serverName.MatchString(name) {
		return MCPServer{}, "its name is not letters, digits, - and _"
	}
	var e claudeEntry
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&e); err != nil {
		return MCPServer{}, "its entry is not understood"
	}
	switch {
	case len(e.Env) > 0:
		return MCPServer{}, "it sets environment variables, which may hold secrets"
	case len(e.Headers) > 0 || e.HeadersHelper != "":
		return MCPServer{}, "it sends headers, which may hold secrets"
	case len(e.OAuth) > 0 && string(e.OAuth) != "null":
		return MCPServer{}, "it has OAuth client settings"
	}
	s := MCPServer{Name: name}
	switch e.Type {
	case "", "stdio":
		s.Type = "stdio"
		if e.Command == "" || e.URL != "" {
			return MCPServer{}, "a stdio server needs a command and no url"
		}
		s.Command, s.Args = e.Command, e.Args
	case "http", "streamable-http", "sse":
		s.Type = map[string]string{"http": "http", "streamable-http": "http", "sse": "sse"}[e.Type]
		if e.URL == "" || e.Command != "" || len(e.Args) > 0 {
			return MCPServer{}, "a remote server needs a url and no command"
		}
		if why := checkURL(e.URL); why != "" {
			return MCPServer{}, why
		}
		s.URL = e.URL
	default:
		return MCPServer{}, "its transport is not one Codex or Grok takes"
	}
	for _, v := range append([]string{s.Command}, s.Args...) {
		if why := checkValue(v); why != "" {
			return MCPServer{}, why
		}
	}
	for _, a := range s.Args {
		if secretWord.MatchString(a) {
			return MCPServer{}, "an argument may name or carry a secret"
		}
	}
	return s, ""
}

// checkValue refuses text that cannot be copied as is.
func checkValue(v string) string {
	switch {
	case !utf8.ValidString(v) || strings.IndexFunc(v, func(r rune) bool { return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) }) >= 0:
		return "a value holds control characters"
	case strings.Contains(v, "${"):
		return "it uses ${VAR} expansion, which may not carry over"
	case redact.Secret(v):
		return "a value looks like a secret"
	}
	return ""
}

// checkURL takes an https URL, or http to the local machine, with no
// user info, query, or fragment, any of which may carry a token.
func checkURL(raw string) string {
	if why := checkValue(raw); why != "" {
		return why
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "its url is not understood"
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	switch {
	case u.Scheme != "https" && (u.Scheme != "http" || !local):
		return "its url is not https"
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "its url has user info, a query, or a fragment, which may carry a token"
	}
	// Capability URLs (Zapier, Composio) carry their key in the path, or
	// in a host label. A refusal here is final: --server only picks among
	// the servers aggy may copy, so the user adds such a server by hand.
	for _, label := range strings.Split(u.Hostname(), ".") {
		if uuidLike.MatchString(label) || redact.Secret(label) || len(label) >= 16 && spread(label) {
			return "its url has a host name part that looks like a key"
		}
	}
	if mixedRun(u.Hostname()) || oddRun(u.Hostname(), 12) {
		return "its url has a host name part that looks like a key"
	}
	if oddRun(u.EscapedPath(), 16) {
		return "its url has path text that looks like a key"
	}
	for _, seg := range strings.Split(u.EscapedPath(), "/") {
		if keyLike(seg) {
			return "its url has a path segment that looks like a key"
		}
	}
	if mixedRun(u.EscapedPath()) {
		return "its url has path text that looks like a key"
	}
	return ""
}

// mixedRun reports whether s holds key-like text that may be short or
// split (G2 re-review): a run of letter-and-digit words, each with a
// change of class in it (a1, Xp), taken together across any separators
// ("/ab12/CD34/ef56/GH78", "k7Xp2QwZ9a"), that is 8 or more characters
// long and changes class (lower, upper, digit; upper to lower, as in a
// capital, does not count) at a third or more of its places. Words such
// as v1, flux1, context7, or TypeScript change class too rarely.
func mixedRun(s string) bool {
	run := ""
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !isAlnum(r) }) {
		if changes(w) == 0 {
			run = ""
			continue
		}
		run += w
		if len(run) >= 8 && 3*changes(run) >= len(run) {
			return true
		}
	}
	return false
}

// oddRun reports whether s holds key-like text the other checks miss
// (I43-7): a run of the given number of digits or more (12 in a host name, I43-11;
// 16 in a path, which holds AWS account and GCP project numbers, N55-6);
// 4 or more digits with letters right before and after (abcd1234efgh); 10
// or more letters in a row of which under a fifth are vowels (y counts),
// which words and names almost never are (kqzvtwmrjxhplbn); or a word
// with lower case, a digit, and 6 or more capitals in a row, as short
// base64 has (QUJDREVGR0g, I43-11), and an acronym such as OPENAPI3 has
// not (N55-6).
// shortcut: a random lower-case run with more vowels (kaqizuvoteb) gets
// through, since words such as typescript look the same to these rules; a
// pronounceability model would catch more.
func oddRun(s string, digits int) bool {
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !isAlnum(r) }) {
		run, most := 0, 0
		for k := 0; k < len(w); k++ {
			if 'A' <= w[k] && w[k] <= 'Z' {
				run++
				most = max(most, run)
			} else {
				run = 0
			}
		}
		if most >= 6 && strings.ContainsAny(w, "0123456789") && strings.ContainsAny(w, "abcdefghijklmnopqrstuvwxyz") {
			return true
		}
	}
	for i := 0; i < len(s); {
		j := i
		switch c := s[i]; {
		case '0' <= c && c <= '9':
			for j < len(s) && '0' <= s[j] && s[j] <= '9' {
				j++
			}
			if j-i >= digits || j-i >= 4 && i > 0 && isLetter(s[i-1]) && j < len(s) && isLetter(s[j]) {
				return true
			}
		case isLetter(c):
			vowels := 0
			for j < len(s) && isLetter(s[j]) {
				if strings.IndexByte("aeiouyAEIOUY", s[j]) >= 0 {
					vowels++
				}
				j++
			}
			if j-i >= 10 && 5*vowels < j-i {
				return true
			}
		default:
			j++
		}
		i = j
	}
	return false
}

func isLetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

func isAlnum(r rune) bool { return r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)) }

// changes counts the places in w where one ASCII character class (lower,
// upper, digit) follows another, except upper to lower.
func changes(w string) int {
	class := func(c byte) int {
		switch {
		case 'a' <= c && c <= 'z':
			return 1
		case 'A' <= c && c <= 'Z':
			return 2
		}
		return 3
	}
	n := 0
	for i := 1; i < len(w); i++ {
		a, b := class(w[i-1]), class(w[i])
		if a != b && (a != 2 || b != 1) {
			n++
		}
	}
	return n
}

var uuidLike = regexp.MustCompile(`(?i)[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}`)

// keyLike reports whether a URL path segment may be a key: a UUID, text
// redact.Secret flags, or 16 or more characters that mix two of lower
// case, upper case, and digits, or whose characters are spread out
// enough (4 bits each or more) to be random.
func keyLike(seg string) bool {
	if uuidLike.MatchString(seg) || redact.Secret(seg) {
		return true
	}
	if len(seg) < 16 {
		return false
	}
	var lower, upper, digit bool
	for _, r := range seg {
		lower = lower || unicode.IsLower(r)
		upper = upper || unicode.IsUpper(r)
		digit = digit || unicode.IsDigit(r)
	}
	classes := 0
	for _, b := range []bool{lower, upper, digit} {
		if b {
			classes++
		}
	}
	return classes >= 2 || spread(seg)
}

// spread reports whether s's characters are spread out enough (4 bits
// each or more) to be random. A host label is checked only this way, as
// host names are lower case and often hold a digit (flux1-schnell).
func spread(s string) bool {
	count := map[rune]int{}
	for _, r := range s {
		count[r]++
	}
	n := float64(utf8.RuneCountInString(s))
	bits := 0.0
	for _, c := range count {
		p := float64(c) / n
		bits -= p * math.Log2(p)
	}
	return bits >= 4
}

// readTOML decodes a vendor config.toml; a missing file is nil, nil.
func readTOML(path string) (map[string]any, []byte, error) {
	b, err := safefs.ReadFileLimit(path, maxVendorTOML)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var doc map[string]any
	if _, err := toml.Decode(string(b), &doc); err != nil {
		return nil, nil, fmt.Errorf("%s is not valid TOML: %w", path, err)
	}
	return doc, b, nil
}

// tomlServers returns the mcp_servers table of doc.
func tomlServers(doc map[string]any) (map[string]any, error) {
	v, ok := doc["mcp_servers"]
	if !ok {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("mcp_servers is not a table")
	}
	return m, nil
}

// MCPWire is what wiring Claude's servers into one vendor would do.
type MCPWire struct {
	// Patch adds Add to the vendor's config.toml; it is the zero Patch
	// when Add is empty.
	Patch patch.Patch
	Add   []MCPServer
	// Have names servers the vendor already has, left as they are.
	Have []string
	Skip []MCPSkip
}

// PlanMCPWire plans adding servers to v's config.toml at path: each one
// v can take and does not already define by name. An existing entry is
// never changed. The edit appends a table per server and never
// re-encodes the file; the result is decoded and must equal the old
// document plus the new tables, or the patch is refused.
func PlanMCPWire(v adapter.Vendor, path string, servers []MCPServer) (MCPWire, error) {
	var w MCPWire
	doc, _, err := readTOML(path)
	if err != nil {
		return w, err
	}
	have, err := tomlServers(doc)
	if err != nil {
		return w, fmt.Errorf("%s: %w", path, err)
	}
	for _, s := range servers {
		if _, ok := have[s.Name]; ok {
			w.Have = append(w.Have, s.Name)
			continue
		}
		if why := takes(v, s); why != "" {
			w.Skip = append(w.Skip, MCPSkip{Name: s.Name, Why: why})
			continue
		}
		w.Add = append(w.Add, s)
	}
	if len(w.Add) == 0 {
		return w, nil
	}
	h, err := patch.HashFile(path)
	if err != nil {
		return w, err
	}
	add := w.Add
	w.Patch = patch.Patch{File: path, ExpectedHash: h, Mode: 0o600,
		Transform: func(src []byte) ([]byte, error) { return addServers(v, src, add) }}
	return w, nil
}

// takes says why v cannot take s, or "".
func takes(v adapter.Vendor, s MCPServer) string {
	switch {
	case v == adapter.Codex && s.Type == "sse":
		return "Codex has no SSE transport"
	case v == adapter.Grok && s.Type == "http" && strings.HasSuffix(s.URL, "/sse"):
		return "Grok takes a url ending in /sse as SSE"
	}
	return ""
}

// tomlTable is the table aggy writes for s, as Codex or Grok reads it.
func tomlTable(v adapter.Vendor, s MCPServer) map[string]any {
	t := map[string]any{}
	if s.Type == "stdio" {
		t["command"] = s.Command
		if len(s.Args) > 0 {
			args := make([]any, len(s.Args))
			for i, a := range s.Args {
				args[i] = a
			}
			t["args"] = args
		}
		return t
	}
	t["url"] = s.URL
	if v == adapter.Grok && s.Type == "sse" {
		t["type"] = "sse"
	}
	return t
}

// addServers is PlanMCPWire's transform. Servers already present are
// left alone, so the transform is idempotent.
func addServers(v adapter.Vendor, src []byte, servers []MCPServer) ([]byte, error) {
	var doc map[string]any
	if _, err := toml.Decode(string(src), &doc); err != nil {
		return nil, fmt.Errorf("the config is not valid TOML, so aggy leaves it alone: %w", err)
	}
	have, err := tomlServers(doc)
	if err != nil {
		return nil, err
	}
	// TOML forbids adding a table to an inline one, which the decoder
	// used here accepts but Codex's and Grok's may not.
	if inlineServers.Match(src) {
		return nil, errors.New("mcp_servers is written as an inline table, so aggy leaves the file alone")
	}
	nl := eol(src)
	out := slices.Clone(src)
	want := maps.Clone(doc)
	if want == nil {
		want = map[string]any{}
	}
	wantServers := maps.Clone(have)
	if wantServers == nil {
		wantServers = map[string]any{}
	}
	added := false
	for _, s := range servers {
		if _, ok := have[s.Name]; ok {
			continue
		}
		t := tomlTable(v, s)
		wantServers[s.Name] = t
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, nl...)
		}
		if len(out) > 0 {
			out = append(out, nl...)
		}
		out = append(out, "[mcp_servers."+s.Name+"]"+nl...)
		for _, k := range []string{"command", "args", "url", "type"} {
			switch x := t[k].(type) {
			case string:
				out = append(out, k+" = "+tomlString(x)+nl...)
			case []any:
				parts := make([]string, len(x))
				for i, a := range x {
					parts[i] = tomlString(a.(string))
				}
				out = append(out, k+" = ["+strings.Join(parts, ", ")+"]"+nl...)
			}
		}
		added = true
	}
	if !added {
		return src, nil
	}
	want["mcp_servers"] = wantServers
	var after map[string]any
	if _, err := toml.Decode(string(out), &after); err != nil {
		return nil, fmt.Errorf("cannot add MCP servers safely: %w", err)
	}
	if !reflect.DeepEqual(after, want) {
		return nil, errors.New("cannot add MCP servers safely: the edit would change other settings")
	}
	return out, nil
}

// inlineServers finds an "mcp_servers = ..." line, at any level.
var inlineServers = regexp.MustCompile(`(?m)^[ \t]*(mcp_servers|"mcp_servers"|'mcp_servers')[ \t]*=`)

// tomlString quotes s as a TOML basic string. checkValue has refused
// control characters, so only the quote and the backslash need escapes.
func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// MCPNames lists, sorted, the MCP servers v loads from its user config
// (and, for Claude, its local scope for the project at root), by name
// only. Codex and Grok servers set enabled = false are left out. For
// Grok it includes ~/.claude.json's servers unless Grok's Claude
// compatibility is off. Values are never read into the result.
func MCPNames(v adapter.Vendor, root string, getenv func(string) string) ([]string, error) {
	set := map[string]bool{}
	addClaude := func() error {
		path, err := ClaudeJSONPath(getenv)
		if err != nil {
			return err
		}
		f, err := readClaude(path)
		if err != nil {
			return err
		}
		for n := range f.MCPServers {
			set[n] = true
		}
		if p, ok := f.Projects[root]; ok && root != "" {
			for n := range p.MCPServers {
				set[n] = true
			}
		}
		return nil
	}
	if v == adapter.Claude {
		if err := addClaude(); err != nil {
			return nil, err
		}
		return slices.Sorted(maps.Keys(set)), nil
	}
	path, err := MCPConfigPath(v, getenv)
	if err != nil {
		return nil, err
	}
	doc, _, err := readTOML(path)
	if err != nil {
		return nil, err
	}
	servers, err := tomlServers(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for n, t := range servers {
		if m, ok := t.(map[string]any); ok && m["enabled"] == false {
			continue
		}
		set[n] = true
	}
	if v == adapter.Grok && grokLoadsClaude(doc, getenv) {
		if err := addClaude(); err != nil {
			return nil, err
		}
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// GrokClaudeStdio reports whether Grok would start stdio MCP servers
// from ~/.claude.json itself (user scope, or the local scope of the
// project at root): its Claude compatibility is not off and the file has
// such a server. A repo's allowed_vendors cannot stop that. Unlike
// MCPNames, a GROK_CLAUDE_MCPS_ENABLED value other than "0" or "false"
// counts as on, so an unverified spelling errs toward a warning.
func GrokClaudeStdio(root string, getenv func(string) string) (bool, error) {
	if e := getenv("GROK_CLAUDE_MCPS_ENABLED"); e == "0" || strings.EqualFold(e, "false") {
		return false, nil
	}
	path, err := MCPConfigPath(adapter.Grok, getenv)
	if err != nil {
		return false, err
	}
	doc, _, err := readTOML(path)
	if err != nil {
		return false, err
	}
	if !grokLoadsClaude(doc, func(string) string { return "" }) {
		return false, nil
	}
	if path, err = ClaudeJSONPath(getenv); err != nil {
		return false, err
	}
	f, err := readClaude(path)
	if err != nil {
		return false, err
	}
	entries := slices.Collect(maps.Values(f.MCPServers))
	if p, ok := f.Projects[root]; ok && root != "" {
		entries = append(entries, slices.Collect(maps.Values(p.MCPServers))...)
	}
	for _, raw := range entries {
		var e claudeEntry
		// An entry aggy cannot parse may still be one Grok starts.
		if json.Unmarshal(raw, &e) != nil || e.Type == "" || e.Type == "stdio" {
			return true, nil
		}
	}
	return false, nil
}

// grokLoadsClaude reports whether Grok loads ~/.claude.json's servers:
// [compat.claude] mcps is not false and GROK_CLAUDE_MCPS_ENABLED is unset
// or "1" or "true". Any other value of the variable counts as off, since
// how Grok parses it is unverified; a server then shows as missing.
func grokLoadsClaude(doc map[string]any, getenv func(string) string) bool {
	if e := getenv("GROK_CLAUDE_MCPS_ENABLED"); e != "" && e != "1" && !strings.EqualFold(e, "true") {
		return false
	}
	compat, _ := doc["compat"].(map[string]any)
	c, _ := compat["claude"].(map[string]any)
	return c["mcps"] != false
}
