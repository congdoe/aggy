// Package redact finds secret-shaped text: API keys and tokens, private
// keys, credentials in URLs, headers, and command lines, and
// secret-named settings such as an env line. The fixture scrubber
// (internal/devfix) replaces Token matches; a handoff brief drops every
// line that holds a secret (Lines, Each), and so do borrow prompts and
// research reports (internal/borrow).
package redact

import (
	"encoding/base64"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Token matches token-shaped strings: provider key formats and the like.
var Token = regexp.MustCompile(strings.Join([]string{
	`\bsk-[A-Za-z0-9_-]{16,}`,                                            // OpenAI and Anthropic keys
	`\bxai-[A-Za-z0-9_-]{16,}`,                                           // xAI keys
	`\bgh[pousr]_[A-Za-z0-9]{20,}`,                                       // GitHub tokens
	`\bgithub_pat_[A-Za-z0-9_]{20,}`,                                     // GitHub fine-grained tokens
	`\bgl[a-z]{2,4}-[A-Za-z0-9_-]{20,}`,                                  // GitLab tokens (glpat-, gldt-, ...)
	`\bnpm_[A-Za-z0-9]{30,}`,                                             // npm tokens
	`\bhf_[A-Za-z0-9]{30,}`,                                              // Hugging Face tokens
	`\bSG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`,                       // SendGrid keys
	`\bAGE-SECRET-KEY-1[0-9A-Z]{20,}`,                                    // age secret keys
	`\bxox[abposr]-[A-Za-z0-9-]{10,}`,                                    // Slack tokens
	`\bxapp-[A-Za-z0-9-]{10,}`,                                           // Slack app tokens
	`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`,                                      // AWS access key ids
	`(?i)aws_?secret_?access_?key["']?\s*[:=]\s*["']?[A-Za-z0-9/+=]{40}`, // AWS secret keys
	`\bAIza[0-9A-Za-z_-]{35}`,                                            // Google API keys
	`\bya29\.[0-9A-Za-z_-]{10,}`,                                         // Google OAuth tokens
	`\b(?:sk|rk|pk)_(?:live|test)_[0-9A-Za-z]{10,}`,                      // Stripe keys
	`\bwhsec_[A-Za-z0-9]{20,}`,                                           // Stripe webhook secrets
	`(?-i:\bSK[0-9a-fA-F]{32}\b)`,                                        // Twilio API keys
	`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`,                   // PEM and PGP private keys
	`\beyJ[A-Za-z0-9_-]{2,}\.[A-Za-z0-9_-]{2,}\.[A-Za-z0-9_-]*`,          // JWTs, signed or not
	`(?i)\bauthorization\s*:\s*basic\s+[A-Za-z0-9+/=]{6,}`,               // HTTP basic auth
	`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{16,}=*`,                            // bearer tokens
	`(?i)["']?\b(?:access_token|refresh_token|id_token|api_key|apikey|secret|password|auth_token)["']?\s*[:=]\s*["']?[A-Za-z0-9._~+/-]{16,}`,
}, "|"))

// secretName is a key, variable, or header name that holds a secret
// word, run together or not, in any case: API_KEY, x-api-key,
// clientSecret, PGPASSWORD, GITHUBTOKEN, AccountKey, "token". The word
// may have suffix segments (API_KEY_ID) but not a bare suffix, so
// "max_tokens" does not count. "pass" counts when it starts the name or
// a segment of it (DB_PASS, PASS_WORD), not inside a word (bypass). The
// separator may be URL-encoded (token%3D...).
const secretName = `(?i)` + nameRe + `["']?\s*` + sepRe

// nameRe is the name part of secretName, and sepRe its separator: ":",
// "=", Go's ":=", Ruby's "||=", Perl's and Ruby's "=>", or a URL-encoded
// ":" or "=".
const (
	nameRe = `(?:[A-Za-z0-9_.-]*(?:apikey|key|token|secret|password|passwd|pwd|passphrase|credentials?)|\b(?:[A-Za-z0-9_.-]*[_.-])?pass)(?:[_.-][A-Za-z0-9]+)*`
	sepRe  = `(?:=>|:=|\|\|=|[:=]|%3[ad])`
)

// q is a quote character; lit is a quoted literal of one character or
// more.
const (
	q   = "[\"'`]"
	lit = q + "[^\"'`\\s]"
	// blockMark ends a key line whose value follows on the next lines: a
	// YAML block scalar indicator (|, >-, |2) or a TOML triple quote.
	blockMark = `[|>][-+0-9]*|"""|'''`
)

// keyPrefix is what may stand before a key line: quoting, a comment
// marker, or a line number, up to 12 characters.
const keyPrefix = `^.{0,12}?`

var (
	// assign is a secret-named setting with a value. Its groups: the
	// name, the space before the separator, the separator, the space
	// after it, the quotes before the value if any (three for a TOML or
	// Python triple quote), and the value. codeValue decides whether an
	// unquoted value counts.
	assign = regexp.MustCompile(`(?i)(` + nameRe + `)["']?(\s*)(` + sepRe + `)(\s*)([\"'` + "`" + `]{0,3})([^\s\"',;` + "`" + `]{8,})`)
	// other matches secrets that are not token-shaped on their own.
	other = regexp.MustCompile(strings.Join([]string{
		`(?i)\b[a-z][a-z0-9+.-]*://[^/\s:@]*:[^/\s@]+@`,             // credentials in a URL
		`(?i)\bauthorization["']?\s*[:=]\s*["']?[A-Za-z]+\s+\S{6,}`, // Authorization: <scheme> <value>
		`(?i)\bauthorization["']?\s*[:=]\s*["']?\S{16,}`,            // Authorization: <value>
		`\b(?:mysql|mysqldump|mysqladmin|mariadb)\b.*\s-p\S+`,       // mysql -pPASS
		`(?i)^\s*(?:machine\s+\S+|default)\b.*\bpassword\s+\S+`,     // .netrc on one line
		`(?i)^\s*password\s+\S{6,}\s*$`,                             // .netrc, one token a line
		// G34-1: a secret name as a call's first argument with a literal
		// second, as a header setter takes it: Header.Set("X-Api-Key", "...").
		`(?i)\(\s*` + q + `(?:` + nameRe + `|authorization|cookie)` + q + `\s*,\s*` + lit,
		// A literal set at a secret-named subscript: os.environ["API_KEY"] = "...".
		`(?i)\[\s*` + q + nameRe + q + `\s*\]\s*(?:\|\||&&|\?\?)?=\s*` + lit,
		`(?i)basic_?auth\s*\(\s*[^,()]*,\s*` + lit,                                                                             // SetBasicAuth("bob", "...")
		`(?i)\bauth\s*=\s*\(\s*` + q + "[^\"'`]*" + q + `\s*,\s*` + lit,                                                        // auth=("bob", "...")
		`(?i)\|\s*` + q + `*` + nameRe + q + `*\s*\|\s*[^\s|]{8,}\s*(?:\||$)`,                                                  // | DB_PASSWORD | value |
		`(?i)\bcookie["']?\s*[:=,]\s*["']?[^\s"';=]+=[^\s"';]`,                                                                 // Cookie: session=...
		`(?i)--(?:[A-Za-z0-9]+[-_])*(?:key|token|secret|pass(?:word|wd|phrase)?|requirepass|masterauth)(?:=|\s+)["']?[^\s"'-]`, // --token X
		`(?i)\bcurl\b.*\s(?:-u|--user)(?:=|\s*)["']?[^\s:"']*:[^\s"']`,                                                         // curl -u user:pw
		`(?i)\b(?:(?:docker|podman|nerdctl|helm\s+registry)\s+login|sshpass)\b.*\s-p(?:=|\s*)[^\s-]`,                           // docker login -p pw
		`(?i)\bredis-cli\b.*\s-a\s+\S`,                                                                                         // redis-cli -a pw
		`(?-i:\bhtpasswd\b.*\s-[A-Za-z]*b[A-Za-z]*\s+\S)`,                                                                      // htpasswd -b file user pw
		`(?i)\bgh\s+secret\s+set\b.*\s(?:-b|--body)(?:=|\s+)\S`,                                                                // gh secret set --body X
		`(?i)\bConvertTo-SecureString\b.*\bAsPlainText\b|\bAsPlainText\b.*\bConvertTo-SecureString\b`,
	}, "|"))
	// pendingKey is a secret name whose value is on the next line, as in
	// JSON or YAML split across lines.
	// A YAML block scalar indicator (password: |) or a TOML triple quote
	// may end the line too.
	pendingKey = regexp.MustCompile(secretName + `\s*(?:` + blockMark + `)?\s*$`)
	// nameKey is a list entry's name line whose value line follows, as in
	// Kubernetes env: "- name: API_TOKEN", then "value: ...".
	nameKey = regexp.MustCompile(`(?i)^\s*(?:-\s+)?["']?name["']?\s*:\s*["']?` + nameRe + `["']?\s*$`)
	// blockKey is a key line that opens a YAML block scalar.
	blockKey = regexp.MustCompile(`[:=]\s*[|>][-+0-9]*\s*$`)
	// comment is a comment line, which may stand between a key and its
	// value.
	comment = regexp.MustCompile(`^\s*(?:#|//|;|<!--)`)
	// bare40 is a candidate AWS secret access key: 40 base64 characters
	// standing alone.
	bare40   = regexp.MustCompile(`(?:^|[^A-Za-z0-9/+=])([A-Za-z0-9/+]{40})(?:$|[^A-Za-z0-9/+=])`)
	keyBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	keyEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	// keyBody is a line of an armored key, after an optional short
	// prefix: base64 of 16 or more characters, or a short last line padded
	// with "=", with trailing quotes, commas, or a written "\n"; or an
	// armor header. A short plain word does not count.
	keyBody = regexp.MustCompile(keyPrefix + `(?:[A-Za-z0-9+/]{16,}={0,2}|[A-Za-z0-9+/]*={1,2})[\s"',+;|\\n]*$|^\s*(?:Proc-Type|DEK-Info|Comment|Version|Hash|Charset): `)
	// longBase64 is a line that ends in 40 or more base64 characters
	// after any prefix, such as a Markdown table's cells, and before
	// trailing quotes, commas, pipes, or a written "\n".
	longBase64 = regexp.MustCompile(`(?:^|[^A-Za-z0-9+/])[A-Za-z0-9+/]{40,}={0,2}[\s"',+;|\\n]*$`)
	// escapedKey is key text inside one escaped string: 60 or more base64
	// characters between written "\n"s, as a JSON-escaped PEM body has.
	escapedKey = regexp.MustCompile(`\\n[A-Za-z0-9+/]{60,}={0,2}\\n`)
	// envLine is an env-file line: NAME=value, nothing else.
	envLine = regexp.MustCompile(`^\s*(?:export\s+)?[A-Za-z_][\w.-]*=\S+\s*$`)
	// pairLine is a one-pair YAML or INI line: key: value or key = value,
	// with no trailing "," or ";".
	pairLine = regexp.MustCompile(`^\s*(?:-\s+)?["']?[\w.-]+["']?\s*[:=]\s*\S*[^\s,;]\s*$`)
	// call is a value that is a function call, such as hashPassword(raw).
	call = regexp.MustCompile(`^[A-Za-z_$][\w$.]*\(.*\)$`)
	// keyword starts a line of code.
	keyword = regexp.MustCompile(`^(?:const|let|var|val|final|def|func|fn|return|private|public|protected|static|readonly)\b`)
	// dotted is a dotted identifier path, such as welcome.message.title.
	dotted = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)+$`)
	// ident is a name that can stand on the left of a statement: an
	// identifier or a dotted path of them (this.token).
	ident = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)*$`)
	// pairKey is a YAML, INI, or JSON key alone on its line, whose value
	// is on the next line.
	pairKey = regexp.MustCompile(`^\s*(?:-\s+)?["']?[\w.-]+["']?\s*(?:=>|[:=])\s*(?:` + blockMark + `)?\s*$`)
	// b64 is a run of base64 text (either alphabet) long enough to wrap a
	// secret.
	b64 = regexp.MustCompile(`[A-Za-z0-9+/_-]{24,}={0,2}`)
)

// hints are lowercase substrings at least one of which every pattern in
// Token, assign, other, and pendingKey needs (Twilio's "SK" is checked
// as is); a line with none skips the regexps, which are slow on long
// text. TestHints checks that every match holds one.
var hints = []string{
	"sk-", "xai-", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "gl", "npm_", "hf_",
	"sg.", "age-secret-key-", "xox", "xapp-", "akia", "asia", "aws", "aiza", "ya29.", "_live_",
	"_test_", "whsec_", "-----begin", "eyj", "auth", "bearer",
	"key", "token", "secret", "pass", "pwd", "credential",
	"://", "mysql", "mariadb", "cookie", "curl", "login", "redis-cli", "asplaintext",
}

// hinted reports whether line holds one of hints. The patterns' (?i)
// also folds the long s (U+017F) to s and the Kelvin sign to k; ToLower
// does the Kelvin sign, and the long s is mapped here.
func hinted(line string) bool {
	if strings.Contains(line, "SK") {
		return true
	}
	l := strings.ToLower(line)
	if strings.ContainsRune(l, 'ſ') {
		l = strings.ReplaceAll(l, "ſ", "s")
	}
	for _, h := range hints {
		if strings.Contains(l, h) {
			return true
		}
	}
	return false
}

// hasRun reports whether line holds n base64 characters in a row; with
// url, "-" and "_" count too.
func hasRun(line string, n int, url bool) bool {
	run := 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '+' || c == '/' || url && (c == '-' || c == '_') {
			if run++; run >= n {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// codeValue reports whether v, the unquoted value after a secret name in
// line, is code rather than a secret, and so is kept (C16-12). m is the
// assign match: name, the spaces around the separator, and the separator;
// dollar is true when a "$" sigil stands before the name. It fails toward
// redaction: never for an env-file line, a value right after NAME= with
// no space around the "=" (a shell assignment, F29-1), or a one-pair YAML
// or INI line (F28-1), which is checked before the call shape (F29-2).
// Otherwise it keeps a call (generateToken(user)), or a value in a line
// that is clearly code (":=", a trailing ";" or ",", a keyword, or a value
// that opens with a bracket) whose name is an identifier, not a header
// such as X-Api-Key, and whose value is code-shaped: brackets, a dotted
// path, or a camelCase or PascalCase identifier ({item.id},
// CancellationToken). A bare identifier is not kept after "=>" or a
// "$"-sigiled name (Perl, PowerShell: F29-3).
func codeValue(line, rest string, m []string, dollar bool) bool {
	name, before, sep, after, v := m[1], m[2], m[3], m[4], m[6]
	if envLine.MatchString(line) {
		return false
	}
	tight := before == "" && after == "" && !strings.HasPrefix(v, "{")
	if tight && (sep == "=" || strings.EqualFold(sep, "%3d")) {
		return false
	}
	if pairLine.MatchString(line) || quotedArg(rest) {
		return false
	}
	if call.MatchString(v) {
		return true
	}
	if !ident.MatchString(name) {
		return false
	}
	t := strings.TrimSpace(line)
	code := strings.Contains(t, ":=") || strings.HasSuffix(t, ";") || strings.HasSuffix(t, ",") ||
		keyword.MatchString(t) || strings.ContainsAny(v[:1], "{[<(")
	if !code {
		return false
	}
	if strings.ContainsAny(v, "(){}[]<>") || dotted.MatchString(v) {
		return true
	}
	return sep != "=>" && !dollar && camel(v)
}

// quotedArg reports whether a value, from its start to the end of its
// statement, calls something with a quoted literal among the arguments,
// as getPass("hunter2hunter2") does (G34-1); such a call is not code to
// keep.
func quotedArg(rest string) bool {
	if i := strings.IndexByte(rest, ';'); i >= 0 {
		rest = rest[:i]
	}
	i := strings.IndexByte(rest, '(')
	return i >= 0 && strings.ContainsAny(rest[i:], "\"'`")
}

// camel reports whether v is a camelCase or PascalCase identifier.
func camel(v string) bool {
	hump := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
		if i > 0 && 'a' <= v[i-1] && v[i-1] <= 'z' && 'A' <= c && c <= 'Z' {
			hump = true
		}
	}
	return hump
}

// assigned reports whether line sets a secret name to a value that is
// quoted or not code.
func assigned(line string) bool {
	for _, ix := range assign.FindAllStringSubmatchIndex(line, -1) {
		m := make([]string, len(ix)/2)
		for i := range m {
			m[i] = line[ix[2*i]:ix[2*i+1]]
		}
		if m[5] != "" || !codeValue(line, line[ix[12]:], m, ix[2] > 0 && line[ix[2]-1] == '$') {
			return true
		}
	}
	return false
}

// Secret reports whether line holds secret-shaped text.
func Secret(line string) bool { return secret(line, true) }

func secret(line string, decode bool) bool {
	if hinted(line) && (Token.MatchString(line) || assigned(line) || other.MatchString(line)) {
		return true
	}
	if decode && hasRun(line, 24, true) && wrapped(line) {
		return true
	}
	if !hasRun(line, 40, false) {
		return false
	}
	if strings.Contains(line, `\n`) && escapedKey.MatchString(line) {
		return true
	}
	for _, m := range bare40.FindAllStringSubmatch(line, -1) {
		// A git SHA is 40 hex characters; a secret key mixes cases and digits.
		s := m[1]
		if strings.ContainsAny(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") && strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyz") && strings.ContainsAny(s, "0123456789") {
			return true
		}
	}
	return false
}

// wrapped reports whether a base64 run in line decodes to text that holds
// a secret, such as a base64-encoded .env file or key.
func wrapped(line string) bool {
	for _, m := range b64.FindAllString(line, -1) {
		s := strings.TrimRight(m, "=")
		enc := base64.RawStdEncoding
		if strings.ContainsAny(s, "-_") {
			if strings.ContainsAny(s, "+/") {
				continue
			}
			enc = base64.RawURLEncoding
		}
		if len(s)%4 == 1 {
			s = s[:len(s)-1]
		}
		b, err := enc.DecodeString(s)
		if err != nil || !utf8.Valid(b) || strings.ContainsFunc(string(b), func(r rune) bool {
			return r < ' ' && r != '\n' && r != '\r' && r != '\t' || r == 0x7f
		}) {
			continue
		}
		for _, l := range strings.Split(string(b), "\n") {
			if secret(l, false) {
				return true
			}
		}
	}
	return false
}

// Each removes every line of text that Secret matches, with no state
// across lines. It returns the rest and how many lines it removed.
func Each(text string) (string, int) {
	lines := strings.Split(text, "\n")
	out := lines[:0]
	for _, l := range lines {
		if !Secret(l) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n"), len(lines) - len(out)
}

// indent is the width of l's leading spaces and tabs.
func indent(l string) int {
	return len(l) - len(strings.TrimLeft(l, " \t"))
}

// endAfter returns the index of the first END line after line i, or -1.
func endAfter(lines []string, i int) int {
	for j := i + 1; j < len(lines); j++ {
		if strings.Contains(lines[j], "-----") && keyEnd.MatchString(lines[j]) {
			return j
		}
	}
	return -1
}

// Lines removes the secrets in one piece of text, such as one transcript
// turn, and returns the rest and how many lines it removed:
//
//   - every line Secret matches
//   - the value line after a secret name that ends its line, or after a
//     list entry's secret name ("- name: API_TOKEN", then "value: ..."),
//     with any comment lines between them
//   - every line of a YAML block scalar under a secret name (password: |)
//   - a private key: from a BEGIN line (the marker anywhere on the line)
//     to the next END line in text; with no END, the BEGIN line and the
//     key lines after it, which may carry a short prefix such as "> ",
//     "# ", "// ", a quote, or a line number
//   - an END line with no BEGIN in view, with the key lines just before it
//   - with a BEGIN that has no END, or an END that has no BEGIN, every
//     line of 40 or more base64 characters in the same text
func Lines(text string) (string, int) {
	lines := strings.Split(text, "\n")
	drop := make([]bool, len(lines))
	unpaired := false // a BEGIN with no END, or an END with no BEGIN
	// pending is a secret name ending the line before; pair, that it was
	// a lone key (pairKey), whose value is never kept (F29-2).
	inKey, pending, pair := false, false, false
	skip := -1  // lines up to skip are already dropped as one block
	block := -1 // the indent of a secret key opening a YAML block scalar
	for i, l := range lines {
		if i <= skip {
			continue
		}
		blank := strings.TrimSpace(l) == ""
		if block >= 0 {
			// The block scalar's value is every line indented past its key.
			if blank {
				continue
			}
			if indent(l) > block {
				drop[i] = true
				continue
			}
			block = -1
		}
		if pending && comment.MatchString(l) {
			// A comment between a key and its value keeps the key pending.
			drop[i] = Secret(l)
			continue
		}
		dashes := strings.Contains(l, "-----")
		switch {
		case inKey && dashes && keyEnd.MatchString(l):
			drop[i], inKey, unpaired = true, false, false
		case inKey && (strings.TrimSpace(l) == "" || keyBody.MatchString(l)):
			drop[i] = true
		case dashes && keyBegin.MatchString(l):
			drop[i] = true
			if keyEnd.MatchString(l) {
				break
			}
			// With its END further on in this text, the whole block goes;
			// without one, only the key-looking lines that follow.
			if end := endAfter(lines, i); end > 0 {
				for j := i + 1; j <= end; j++ {
					drop[j] = true
				}
				skip = end
				break
			}
			inKey, unpaired = true, true
		case dashes && keyEnd.MatchString(l):
			drop[i], unpaired = true, true
			for j := i - 1; j >= 0 && !drop[j] && strings.TrimSpace(lines[j]) != "" && keyBody.MatchString(lines[j]); j-- {
				drop[j] = true
			}
		case pending && !blank:
			// The value on its own line is kept only when it is a call
			// continuing a statement, with no quoted literal in it.
			v := strings.TrimRight(strings.TrimSpace(l), ",;")
			drop[i] = pair || !call.MatchString(v) || strings.ContainsAny(v, "\"'`") || Secret(l)
		default:
			// A key block ends at the first line that is not key text.
			inKey = false
			drop[i] = Secret(l)
		}
		if !blank || !pending {
			key := !drop[i] && hinted(l)
			named := key && nameKey.MatchString(l)
			pending = key && (named || pendingKey.MatchString(l))
			pair = pending && (named || pairKey.MatchString(l))
			if pending && !named && blockKey.MatchString(l) {
				block, pending = indent(l), false
			}
		}
	}
	out := lines[:0]
	removed := 0
	for i, l := range lines {
		if drop[i] || unpaired && hasRun(l, 40, false) && longBase64.MatchString(l) {
			removed++
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), removed
}
