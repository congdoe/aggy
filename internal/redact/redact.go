// Package redact finds secret-shaped text: API keys and tokens, private
// keys, credentials in URLs, headers, and command lines, and
// secret-named settings such as an env line. The fixture scrubber
// (internal/devfix) replaces Token matches; a handoff brief drops every
// line that holds a secret (Lines, Each).
//
// TODO(T6672): run borrow prompts through Lines too, once the parallel
// work in internal/borrow lands.
package redact

import (
	"regexp"
	"strings"
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
// "max_tokens" does not count.
const secretName = `(?i)[A-Za-z0-9_.-]*(?:apikey|key|token|secret|password|passwd|pwd|passphrase|credentials?)(?:[_.-][A-Za-z0-9]+)*["']?\s*[:=]`

// keyPrefix is what may stand before a key line: quoting, a comment
// marker, or a line number, up to 12 characters.
const keyPrefix = `^.{0,12}?`

var (
	// other matches secrets that are not token-shaped on their own.
	other = regexp.MustCompile(strings.Join([]string{
		secretName + `\s*["']?[^\s"',;]{8,}`,                   // a secret-named setting with a value
		`(?i)\b[a-z][a-z0-9+.-]*://[^/\s:@]*:[^/\s@]+@`,        // credentials in a URL
		`(?i)\bauthorization\s*[:=]\s*["']?[A-Za-z]+\s+\S{6,}`, // Authorization: <scheme> <value>
		`(?i)\bauthorization\s*[:=]\s*["']?\S{16,}`,            // Authorization: <value>
		`\b(?:mysql|mysqldump|mysqladmin|mariadb)\b.*\s-p\S+`,  // mysql -pPASS
		`(?i)--pass(?:word)?[= ]\S+`,                           // --password=PASS
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
	keyBody = regexp.MustCompile(keyPrefix + `(?:[A-Za-z0-9+/]{16,}={0,2}|[A-Za-z0-9+/]*={1,2})[\s"',+;\\n]*$|^\s*(?:Proc-Type|DEK-Info|Comment|Version|Hash|Charset): `)
	// longBase64 is a line whose rest, after a short prefix, is 40 or
	// more base64 characters.
	longBase64 = regexp.MustCompile(keyPrefix + `[A-Za-z0-9+/]{40,}={0,2}[\s"',+;\\n]*$`)
)

// hints are lowercase substrings at least one of which every pattern in
// Token, other, and pendingKey needs; a line with none skips the
// regexps, which are slow on long text. TestHints checks that every
// match holds one.
var hints = []string{
	"sk-", "xai-", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "gl", "npm_", "hf_",
	"sg.", "age-secret-key-", "xox", "xapp-", "akia", "asia", "aws", "aiza", "ya29.", "_live_",
	"_test_", "whsec_", "-----begin", "eyj", "authorization", "bearer",
	"key", "token", "secret", "password", "passwd", "pwd", "passphrase", "credential",
	"://", "mysql", "mariadb", "--pass",
}

// hinted reports whether line holds one of hints.
func hinted(line string) bool {
	l := strings.ToLower(line)
	for _, h := range hints {
		if strings.Contains(l, h) {
			return true
		}
	}
	return false
}

// hasRun40 reports whether line holds 40 base64 characters in a row.
func hasRun40(line string) bool {
	run := 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '+' || c == '/' {
			if run++; run >= 40 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// Secret reports whether line holds secret-shaped text.
func Secret(line string) bool {
	if hinted(line) && (Token.MatchString(line) || other.MatchString(line)) {
		return true
	}
	if !hasRun40(line) {
		return false
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
			drop[i] = true
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
		if drop[i] || unpaired && hasRun40(l) && longBase64.MatchString(l) {
			removed++
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), removed
}
