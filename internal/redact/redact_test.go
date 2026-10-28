package redact

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// body is one 64-character line of a PEM key body.
const body = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"

// linesTests are Lines cases: input, output, and lines removed.
var linesTests = []struct {
	name, in, want string
	removed        int
}{
	{"clean", "a\nb", "a\nb", 0},
	{"openai key", "x\nkey is sk-abcdefghijklmnop1234 ok\ny", "x\ny", 1},
	{"env line", "export ANTHROPIC_API_KEY=abcd1234efgh\nPATH=/bin", "PATH=/bin", 1},
	{"json secret", `{"client_secret": "hunter2hunter2"}` + "\nz", "z", 1},
	{"codex auth.json shape", "{\n  \"tokens\": {\n    \"id_token\": \"eyJhbGciOi.eyJzdWIi.sig\",\n    \"refresh_token\": \"rt_abcdefghijklmnopqrstu\"\n  }\n}", "{\n  \"tokens\": {\n  }\n}", 2},
	{"pem block", "a\n-----BEGIN RSA PRIVATE KEY-----\nMIIabc\ndef\n-----END RSA PRIVATE KEY-----\nb", "a\nb", 4},
	{"pem unterminated", "a\n-----BEGIN PRIVATE KEY-----\n" + body + "\n" + body + "\nmore", "a\nmore", 3},
	{"short values stay", "max_tokens: 4096\npassword: no\nmax_tokens = 200000000", "max_tokens: 4096\npassword: no\nmax_tokens = 200000000", 0},
	{"bearer", "curl -H 'Authorization: Bearer abcdefghijklmnopqrst'", "", 1},

	// C16-3: a BEGIN marker inside prose drops that line only.
	{"begin in prose", "look at -----BEGIN RSA PRIVATE KEY----- in the docs\nkeep this\nand this", "keep this\nand this", 1},
	{"begin after quote", "x\n\"-----BEGIN PRIVATE KEY-----\nMIIabc\n-----END PRIVATE KEY-----\"\ny", "x\ny", 3},
	// C16-4: an END with no BEGIN in view takes the key lines before it,
	// and long base64 lines anywhere in the same text.
	{"orphan end", "earlier\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7\nabcDEF==\n-----END PRIVATE KEY-----\nafter", "earlier\nafter", 3},
	// C16-10: key lines with a prefix, and a BEGIN mid-line with no END.
	{"quoted key", "> -----BEGIN RSA PRIVATE KEY-----\n> " + body + "\n> " + body + "\n> -----END RSA PRIVATE KEY-----\nok", "ok", 4},
	{"commented key no end", "# -----BEGIN RSA PRIVATE KEY-----\n# " + body + "\n// " + body + "\nnext", "next", 3},
	{"numbered key", "   11\t-----BEGIN RSA PRIVATE KEY-----\n   12\t" + body + "\n   13\t" + body + "\n   14\tdone", "   14\tdone", 3},
	{"python key no end", "key = \"\"\"-----BEGIN RSA PRIVATE KEY-----\n" + body + "\n" + body, "", 3},
	{"json key no end", "\"pem\": \"-----BEGIN RSA PRIVATE KEY-----\\n\"\n  \"" + body + "\\n\" +\nprose\n  \"" + body + "\\n\"", "prose", 3},
	{"prefixed orphan end", "> " + body + "\n> " + body + "\n> -----END RSA PRIVATE KEY-----\nthanks", "thanks", 3},
	{"orphan end far body", "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7\nprose line\n-----END OPENSSH PRIVATE KEY-----", "prose line", 2},
	// C16-17: a key with no END whose lines have a long prefix, as in a
	// Markdown table.
	{"table key no end", "| a | -----BEGIN RSA PRIVATE KEY----- |\n| the private key | " + body + " |\n| the private key | " + body + " |\nok", "ok", 3},
	// C16-12: a value on the next line that is code is kept.
	{"pending code", "const token =\n  generateToken(user)\nnext", "const token =\n  generateToken(user)\nnext", 0},
}

func TestLines(t *testing.T) {
	for _, tc := range linesTests {
		got, n := Lines(tc.in)
		if got != tc.want || n != tc.removed {
			t.Errorf("%s: got %q (%d removed), want %q (%d)", tc.name, got, n, tc.want, tc.removed)
		}
	}
}

// secretLines are lines Secret must catch. C16-1: each of these passed
// the old patterns.
var secretLines = []string{
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
	// C16-9: secret names with no separator, camelCase or run together.
	`clientSecret: "Zx9QwErTyUiOp1234"`,
	`accessToken = "Zx9QwErTyUiOp1234"`,
	`"dbPassword": "Zx9QwErTyUiOp1234"`,
	`privateKey: "Zx9QwErTyUiOp1234"`,
	"GITHUBTOKEN=Zx9QwErTyUiOp1234",
	"DefaultEndpointsProtocol=https;AccountName=acct;AccountKey=Zx9QwErTyUiOp1234abcd==",
	"PGPASSWORD=Zx9QwErTyUiOp1234",
	"export PGPASSWORD=Zx9QwErTyUiOp1234",
	// C16-11: .netrc, URL-encoded, and Twilio.
	"machine api.example.com login me password hunter2hunter2",
	"  password hunter2hunter2",
	"https://x.example/cb?access_token%3DZx9QwErTyUiOp1234&a=b",
	"redirect=token%3dZx9QwErTyUiOp1234",
	"SK" + strings.Repeat("0123456789abcdef", 2), // built here, so no scanner takes it for a key
	// C16-15: the long s, which (?i) folds to s.
	"paſſword=Zx9QwErTyUiOp1234",
	"ſecret=Zx9QwErTyUiOp1234",
	// C16-17: a JSON-escaped key body with no BEGIN or END.
	`{"k": "x\n` + body + `\n` + body + `\n"}`,
}

// C16-11: base64-wrapped secrets, in either alphabet.
func wrappedLines() []string {
	env := "DEBUG=1\nOPENAI_API_KEY=sk-abcdefghijklmnop1234\n"
	return []string{
		"data: " + base64.StdEncoding.EncodeToString([]byte(env)),
		"echo " + base64.RawURLEncoding.EncodeToString([]byte("token: Zx9QwErTyUiOp1234~~~")) + " | base64 -d",
	}
}

func TestSecret(t *testing.T) {
	for _, line := range append(slices.Clone(secretLines), wrappedLines()...) {
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
		// C16-12: code, not secrets.
		"<li key={item.id}>",
		"const token = generateToken(user)",
		"token: CancellationToken,",
		"password = hashPassword(raw)",
		"key: welcome.message.title",
		"login with the password reset link",
		"data: " + base64.StdEncoding.EncodeToString([]byte("just some ordinary text here")),
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

// TestHints (C16-16): every line a pattern matches holds a hint, so the
// prefilter never hides a match. Checked on the positive rows of
// TestSecret and TestLines, each also in upper and lower case and with
// the long s and the Kelvin sign that (?i) folds to s and k, and on
// aggy's sources.
func TestHints(t *testing.T) {
	var lines []string
	for _, l := range secretLines {
		lines = append(lines, folds(l)...)
	}
	for _, tc := range linesTests {
		for _, l := range strings.Split(tc.in, "\n") {
			lines = append(lines, folds(l)...)
		}
	}
	files, _ := filepath.Glob("../*/*.go")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, strings.Split(string(b), "\n")...)
	}
	for _, l := range lines {
		if (Token.MatchString(l) || assign.MatchString(l) || other.MatchString(l) || pendingKey.MatchString(l)) && !hinted(l) {
			t.Errorf("match without a hint: %q", l)
		}
	}
}

// folds returns l and the spellings (?i) treats as l.
func folds(l string) []string {
	return []string{
		l, strings.ToUpper(l), strings.ToLower(l),
		strings.NewReplacer("s", "ſ", "S", "ſ").Replace(l),
		strings.NewReplacer("k", "\u212a", "K", "\u212a").Replace(l),
	}
}
