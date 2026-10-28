// Package redact finds secret-shaped text: API keys and tokens, private
// keys, and secret-named assignments such as an env line. The fixture
// scrubber (internal/devfix) replaces Token matches; a handoff brief
// drops every line that holds one (Lines).
//
// TODO(T6672): run borrow prompts through Lines too, once the parallel
// work in internal/borrow lands.
package redact

import (
	"regexp"
	"strings"
)

// Token matches token-shaped strings.
var Token = regexp.MustCompile(strings.Join([]string{
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

// assignment matches a secret-named variable or key set to a value of 8
// or more characters, as in an env dump or a config file: API_KEY=...,
// "client_secret": "...", password: ....
var assignment = regexp.MustCompile(`(?i)[A-Z0-9_]*(?:API_?KEY|ACCESS_?KEY|SECRET|TOKEN|PASSWORD|PASSWD|CREDENTIAL)[A-Z0-9_]*["']?\s*[:=]\s*["']?[^\s"',]{8,}`)

var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

// Secret reports whether line holds secret-shaped text.
func Secret(line string) bool { return Token.MatchString(line) || assignment.MatchString(line) }

// Lines removes every line of text that holds secret-shaped text, and a
// PEM private key block whole (to the end of text when it has no END
// line). It returns the rest and how many lines it removed.
func Lines(text string) (string, int) {
	lines := strings.Split(text, "\n")
	out := lines[:0]
	removed := 0
	inKey := false
	for _, l := range lines {
		switch {
		case inKey:
			inKey = !pemEnd.MatchString(l)
		case pemBegin.MatchString(l):
			inKey = !pemEnd.MatchString(l)
		case Secret(l):
		default:
			out = append(out, l)
			continue
		}
		removed++
	}
	return strings.Join(out, "\n"), removed
}
