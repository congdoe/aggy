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
// "max_tokens" does not count. The separator may be URL-encoded
// (token%3D...).
const secretName = `(?i)[A-Za-z0-9_.-]*(?:apikey|key|token|secret|password|passwd|pwd|passphrase|credentials?)(?:[_.-][A-Za-z0-9]+)*["']?\s*(?:[:=]|%3[ad])`

// keyPrefix is what may stand before a key line: quoting, a comment
// marker, or a line number, up to 12 characters.
const keyPrefix = `^.{0,12}?`

var (
	// assign is a secret-named setting with a value: the quote before the
	// value, if any, and the value. literal decides whether it counts.
	assign = regexp.MustCompile(secretName + "\\s*([\"'`]?)([^\\s\"',;`]{8,})")
	// other matches secrets that are not token-shaped on their own.
	other = regexp.MustCompile(strings.Join([]string{
		`(?i)\b[a-z][a-z0-9+.-]*://[^/\s:@]*:[^/\s@]+@`,         // credentials in a URL
		`(?i)\bauthorization\s*[:=]\s*["']?[A-Za-z]+\s+\S{6,}`,  // Authorization: <scheme> <value>
		`(?i)\bauthorization\s*[:=]\s*["']?\S{16,}`,             // Authorization: <value>
		`\b(?:mysql|mysqldump|mysqladmin|mariadb)\b.*\s-p\S+`,   // mysql -pPASS
		`(?i)--pass(?:word)?[= ]\S+`,                            // --password=PASS
		`(?i)^\s*(?:machine\s+\S+|default)\b.*\bpassword\s+\S+`, // .netrc on one line
		`(?i)^\s*password\s+\S{6,}\s*$`,                         // .netrc, one token a line
	}, "|"))
	// pendingKey is a secret name whose value is on the next line, as in
	// JSON or YAML split across lines.
	pendingKey = regexp.MustCompile(secretName + `\s*$`)
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
	// dotted is a dotted identifier path, such as welcome.message.title.
	dotted = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)+$`)
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
	"_test_", "whsec_", "-----begin", "eyj", "authorization", "bearer",
	"key", "token", "secret", "password", "passwd", "pwd", "passphrase", "credential",
	"://", "mysql", "mariadb", "--pass",
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

// literal reports whether v, the text after a secret name, looks like a
// value rather than code: quoted, or a word that is not a call, an
// expression, a type, or a dotted path (generateToken(user),
// {item.id}, CancellationToken, welcome.message.title).
func literal(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	if strings.ContainsRune("\"'`", rune(v[0])) {
		return true
	}
	if i := strings.IndexAny(v, " \t,;"); i >= 0 {
		v = v[:i]
	}
	if strings.ContainsAny(v, "(){}[]<>") || dotted.MatchString(v) {
		return false
	}
	// A camelCase or PascalCase identifier: letters only, with a hump.
	hump := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return true
		}
		if i > 0 && 'a' <= v[i-1] && v[i-1] <= 'z' && 'A' <= c && c <= 'Z' {
			hump = true
		}
	}
	return !hump
}

// assigned reports whether line sets a secret name to a literal value.
func assigned(line string) bool {
	for _, m := range assign.FindAllStringSubmatch(line, -1) {
		if m[1] != "" || literal(m[2]) {
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
//   - the value line after a secret name that ends its line
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
	inKey, pending := false, false
	skip := -1 // lines up to skip are already dropped as one block
	for i, l := range lines {
		if i <= skip {
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
		case pending && strings.TrimSpace(l) != "":
			drop[i] = literal(l) || Secret(l)
		default:
			// A key block ends at the first line that is not key text.
			inKey = false
			drop[i] = Secret(l)
		}
		if strings.TrimSpace(l) != "" || !pending {
			pending = !drop[i] && hinted(l) && pendingKey.MatchString(l)
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
