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

// secretName is a key, variable, or header name with a secret word as
// one of its segments: API_KEY, x-api-key, client_secret, DB_PASSWORD,
// "token", SIGNING_KEY. "monkey" or "max_tokens" do not count.
const secretName = `(?i)(?:^|[^A-Za-z0-9])(?:[A-Za-z0-9]+[_.-])*(?:apikey|key|token|secret|password|passwd|pwd|passphrase|credentials?)["']?\s*[:=]`

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
	keyBegin = regexp.MustCompile(`^\s*["']?-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	keyEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	// keyBody is a line of an armored key: base64 of 16 or more
	// characters, a short last line padded with "=", or a header such as
	// Proc-Type: or Comment:. A short plain word does not count.
	keyBody = regexp.MustCompile(`^\s*(?:[A-Za-z0-9+/]{16,}={0,2}|[A-Za-z0-9+/]*={1,2}|[A-Za-z-]+: .*)\s*$`)
	// longBase64 is a line of 40 or more base64 characters.
	longBase64 = regexp.MustCompile(`^\s*[A-Za-z0-9+/=]{40,}\s*$`)
)

// Secret reports whether line holds secret-shaped text.
func Secret(line string) bool {
	if Token.MatchString(line) || other.MatchString(line) {
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

// Lines removes the secrets in one piece of text, such as one transcript
// turn, and returns the rest and how many lines it removed:
//
//   - every line Secret matches
//   - the value line after a secret name that ends its line
//   - a private key block whole, from a BEGIN line (at the start of a
//     line) to its END line, or to the end of text when there is none
//   - an END line with no BEGIN in view, with the key lines just before
//     it, and every line of 40 or more base64 characters in the same text
func Lines(text string) (string, int) {
	lines := strings.Split(text, "\n")
	drop := make([]bool, len(lines))
	orphanEnd := false
	inKey, pending := false, false
	for i, l := range lines {
		switch {
		case inKey:
			drop[i] = true
			inKey = !keyEnd.MatchString(l)
		case keyBegin.MatchString(l):
			drop[i] = true
			inKey = !keyEnd.MatchString(l)
		case keyEnd.MatchString(l):
			drop[i] = true
			orphanEnd = true
			for j := i - 1; j >= 0 && !drop[j] && keyBody.MatchString(lines[j]) && strings.TrimSpace(lines[j]) != ""; j-- {
				drop[j] = true
			}
		case pending && strings.TrimSpace(l) != "":
			drop[i] = true
		default:
			drop[i] = Secret(l)
		}
		if strings.TrimSpace(l) != "" || !pending {
			pending = !drop[i] && pendingKey.MatchString(l)
		}
	}
	out := lines[:0]
	removed := 0
	for i, l := range lines {
		if drop[i] || orphanEnd && longBase64.MatchString(l) {
			removed++
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), removed
}
