package redact

import (
	"strings"
	"testing"
)

func TestLines(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		removed        int
	}{
		{"clean", "a\nb", "a\nb", 0},
		{"openai key", "x\nkey is sk-abcdefghijklmnop1234 ok\ny", "x\ny", 1},
		{"env line", "export ANTHROPIC_API_KEY=abcd1234efgh\nPATH=/bin", "PATH=/bin", 1},
		{"json secret", `{"client_secret": "hunter2hunter2"}` + "\nz", "z", 1},
		{"codex auth.json shape", "{\n  \"tokens\": {\n    \"id_token\": \"eyJhbGciOi.eyJzdWIi.sig\",\n    \"refresh_token\": \"rt_abcdefghijklmnopqrstu\"\n  }\n}", "{\n  \"tokens\": {\n  }\n}", 2},
		{"pem block", "a\n-----BEGIN RSA PRIVATE KEY-----\nMIIabc\ndef\n-----END RSA PRIVATE KEY-----\nb", "a\nb", 4},
		{"pem unterminated", "a\n-----BEGIN PRIVATE KEY-----\nMIIabc\nmore", "a", 3},
		{"short values stay", "max_tokens: 4096\npassword: no\nmonkey: bananas12345\nmax_tokens = 200000000", "max_tokens: 4096\npassword: no\nmonkey: bananas12345\nmax_tokens = 200000000", 0},
		{"bearer", "curl -H 'Authorization: Bearer abcdefghijklmnopqrst'", "", 1},

		// C16-3: a BEGIN marker inside prose drops that line only.
		{"begin in prose", "look at -----BEGIN RSA PRIVATE KEY----- in the docs\nkeep this\nand this", "keep this\nand this", 1},
		{"begin after quote", "x\n\"-----BEGIN PRIVATE KEY-----\nMIIabc\n-----END PRIVATE KEY-----\"\ny", "x\ny", 3},
		// C16-4: an END with no BEGIN in view takes the key lines before it,
		// and long base64 lines anywhere in the same text.
		{"orphan end", "earlier\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7\nabcDEF==\n-----END PRIVATE KEY-----\nafter", "earlier\nafter", 3},
		{"orphan end far body", "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7\nprose line\n-----END OPENSSH PRIVATE KEY-----", "prose line", 2},
	} {
		got, n := Lines(tc.in)
		if got != tc.want || n != tc.removed {
			t.Errorf("%s: got %q (%d removed), want %q (%d)", tc.name, got, n, tc.want, tc.removed)
		}
	}
}

// C16-1: each of these passed the old patterns.
func TestSecret(t *testing.T) {
	for _, line := range []string{
		"DATABASE_URL=postgres://admin:S3cr3tPassw0rd@db:5432/prod",
		"REDIS_URL=redis://:pw@cache",
		"git clone https://user:pass@github.com/o/r.git",
		"Authorization: Token 0123456789abcdef0123456789abcdef",
		"x-api-key: 0123456789abcdef0123",
		"X-Auth-Token: abcdefgh12345678",
		"PRIVATE_KEY=0123456789abcdef0123",
		"SIGNING_KEY=0123456789abcdef0123",
		"signing_secret = 'abcdefgh12345678'",
		"db_password: correcthorse",
		"-----BEGIN PGP PRIVATE KEY BLOCK-----",
		"glpat-abcdefghij0123456789",
		"npm_abcdefghijklmnopqrstuvwxyz0123456789",
		"hf_abcdefghijklmnopqrstuvwxyz0123456",
		"SG.abcdefghijklmnopqrstuv.abcdefghijklmnopqrstuvwxyz0123456789",
		"AGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ",
		"xoxb-1234567890-abcdefghij",
		"sk_live_abcdefghij1234",
		"github_pat_abcdefghijklmnopqrstuv_0123",
		"AIzaSyA0123456789abcdefghijklmnopqrstuv",
		"sk-ant-api03-abcdefghijklmnopqrstuv",
		"xai-abcdefghijklmnopqrstuv",
		"mysql -u root -pS3cretPass prod",
		"mysqldump --password=S3cretPass prod",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	} {
		if !Secret(line) {
			t.Errorf("Secret(%q) = false", line)
		}
	}
	for _, line := range []string{
		"commit 0123456789abcdef0123456789abcdef01234567",
		"git@github.com:o/r.git",
		"see https://example.com/a:b",
		"the key point is speed",
		"mysql -u root -P 3306",
	} {
		if Secret(line) {
			t.Errorf("Secret(%q) = true", line)
		}
	}
}

// C16-1: a secret name and its value split across lines.
func TestLinesSplitValue(t *testing.T) {
	in := "{\n  \"password\":\n    \"S3cretPassw0rd\",\n  \"user\": \"me\"\n}\napi_key:\n\n  abcdefgh12345678\nnext"
	got, n := Lines(in)
	if strings.Contains(got, "S3cret") || strings.Contains(got, "abcdefgh12345678") || n != 2 {
		t.Errorf("got %q (%d removed)", got, n)
	}
	if !strings.Contains(got, `"user": "me"`) || !strings.Contains(got, "next") {
		t.Errorf("removed too much: %q", got)
	}
}

func TestEach(t *testing.T) {
	got, n := Each("a\n-----BEGIN RSA PRIVATE KEY-----\nb\nsk-abcdefghijklmnop1234")
	if got != "a\nb" || n != 2 {
		t.Errorf("Each = %q, %d", got, n)
	}
}
