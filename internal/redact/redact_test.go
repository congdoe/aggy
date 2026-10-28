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
	// F29-2: a lone YAML key's value is dropped even when call-shaped.
	{"pending pair call", "password:\n  Summer(Fun)\nnext", "password:\nnext", 1},
	// G34-1: k8s and compose value lines, block scalars, a comment
	// between a key and its value, and a call with a literal.
	{"k8s env", "env:\n  - name: API_TOKEN\n    value: hunter2hunter2\n  - name: MODE\n    value: prod", "env:\n  - name: API_TOKEN\n  - name: MODE\n    value: prod", 1},
	{"k8s env quoted", "- name: \"DB_PASSWORD\"\n  # set by ops\n  value: \"hunter2hunter2\"\nnext", "- name: \"DB_PASSWORD\"\n  # set by ops\nnext", 1},
	{"block literal", "db:\n  password: |\n    hunter2hunter2\n    more\n\n  user: me", "db:\n  password: |\n\n  user: me", 2},
	{"block folded", "password: >-\n  hunter2hunter2\nuser: me", "password: >-\nuser: me", 1},
	{"comment between", "password:\n# the prod one\n  hunter2hunter2\nnext", "password:\n# the prod one\nnext", 1},
	{"pending call literal", "const password =\n  getPass(\"hunter2hunter2\");\nnext", "const password =\nnext", 1},
	// I43-3: JSON env, a block scalar with a comment or a tag, and a
	// triple-quoted value over several lines.
	{"json env", "[\n  {\n    \"name\": \"DB_PASSWORD\",\n    \"value\": \"hunter2hunter2\"\n  }\n]", "[\n  {\n    \"name\": \"DB_PASSWORD\",\n  }\n]", 1},
	{"block comment", "password: | # set by ops\n  hunter2hunter2\nuser: me", "password: | # set by ops\nuser: me", 1},
	{"block tag", "api_key: !!binary |\n  aHVudGVyMmh1bnRlcjI=\nnext", "next", 2},
	{"triple single", "token = '''\nhunter2hunter2\nsecond line\n'''\nnext", "token = '''\nnext", 3},
	{"triple double", "password = \"\"\"\nhunter2hunter2\"\"\"\nnext", "password = \"\"\"\nnext", 1},
	// I43-4: a secret flag's value on the next line of a list or array.
	{"yaml flag list", "args:\n  - --token\n  - hunter2hunter2\n  - --verbose", "args:\n  - --token\n  - --verbose", 1},
	{"json flag array", "\"args\": [\n  \"--api-key\",\n  \"hunter2hunter2\"\n]", "\"args\": [\n  \"--api-key\",\n]", 1},
	{"flag bare word", "--token\nhunter2hunter2\nnext", "--token\nnext", 1},
	{"flag quoted words", "- --password\n- \"correct horse battery\"\nnext", "- --password\nnext", 1},
	// I43-11: the line after a lone flag is kept when it is not its value.
	// N55-1: any other line after it is its value, help text too.
	{"flag help dropped", "  --token\n      The API token to use.\n  --verbose", "  --token\n  --verbose", 1},
	{"flag words", "- --password\n- correct horse battery staple\nnext", "- --password\nnext", 1},
	{"flag words digits", "- --password\n- Tr0ub4dor and 3 more\nnext", "- --password\nnext", 1},
	{"flag then flag", "args:\n  - --token\n  - --verbose", "args:\n  - --token\n  - --verbose", 0},
	// N55-7: a bare $word is a value unless it is env-style upper case.
	{"flag dollar value", "- --password\n- $Xsecret\nnext", "- --password\nnext", 1},
	{"flag env refs", "- --password\n- $API_KEY\n- --token\n- ${api_key}\n- --api-key\n- $(TOKEN)", "- --password\n- $API_KEY\n- --token\n- ${api_key}\n- --api-key\n- $(TOKEN)", 0},
	{"flag env ref", "- --api-key\n- $(API_KEY)\n- --token\n- \"${TOKEN}\"", "- --api-key\n- $(API_KEY)\n- --token\n- \"${TOKEN}\"", 0},
	{"flag word", "  --monkey\n  banana\n", "  --monkey\n  banana\n", 0},
	// I43-5: a value in parentheses over several lines.
	{"paren value", "API_KEY = (\n    \"hunter2\"\n    \"hunter2\"\n)\nnext = 1", "API_KEY = (\n)\nnext = 1", 2},
	{"paren value close", "API_KEY = (\n    \"hunter2\"\n    \"hunter2\")\nnext = 1", "API_KEY = (\nnext = 1", 2},
	// N55-5: a ")" inside a literal does not end the value.
	{"paren in literal", "API_KEY = (\n    \"ab)cd\"\n    \"hunter2\"\n)\nnext", "API_KEY = (\n)\nnext", 2},
	{"paren code", "password = (\n    hashPassword(raw)\n)\nnext", "password = (\n    hashPassword(raw)\n)\nnext", 0},
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
	// F28-1: env-file and YAML or INI values that look like identifiers,
	// dotted paths, or calls are still secrets.
	"DB_PASSWORD=CorrectHorseBatteryStaple",
	"password: SuperSecretPass",
	"  db_password: MyDogRexIsGreat",
	"STRIPE_SECRET=TopSecretValue",
	"client_secret=SomeLongMixedCaseValue",
	"SESSION_KEY=OnlyLettersHereAbcdef",
	"passphrase=CorrectHorse",
	"SECRET_KEY=Xk.Pq.Zr.Tv.Wm",
	"api_key = abc(def)ghi123",
	"export API_TOKEN=generateToken(x)",
	"key: welcome.message.title", // C16-12 negative row dropped: a YAML pair is redacted (F28-1)
	// F29-1: NAME=value with no space around "=" is a shell or env
	// assignment, whatever ends the line; and a header is not a statement.
	"DB_PASSWORD=CorrectHorseBatteryStaple ./run.sh;",
	"docker run -e DB_PASSWORD=CorrectHorseBatteryStaple app;",
	`curl -H "X-Api-Key: CorrectHorseBatteryStaple" https://x;`,
	`environment: ["A=b", "DB_PASSWORD=CorrectHorseBatteryStaple"],`,
	// F29-2: a YAML or INI pair is redacted before the call shape is
	// looked at. The old C16-12 code row "password = hashPassword(raw)" is
	// such a pair (an INI line reads the same), so it is redacted now.
	"password: Summer(Fun)",
	"password: ENC(...)",
	"password = hashPassword(raw)",
	// F29-3: Perl's "=>" and a PowerShell variable.
	"mysql_password => CorrectHorseBatteryStaple,",
	"$secret_key = CorrectHorseBatteryStaple;",
	// C16-17: a JSON-escaped key body with no BEGIN or END.
	`{"k": "x\n` + body + `\n` + body + `\n"}`,
	// G34-1: each of these passed redact.Lines unchanged.
	`password := "hunter2hunter2"`,
	`req.Header.Set("X-Api-Key", "hunter2hunter2")`,
	`req.Header.Set("Authorization", "Token hunter2hunter2")`,
	`req.SetBasicAuth("bob","hunter2hunter2")`,
	`  "Authorization": "Token hunter2hunter2",`,
	`{"pass":"hunter2hunter2"}`,
	"DB_PASS=hunter2hunter2",
	"SMTP_PASS=hunter2hunter2",
	"pass = hunter2hunter2",
	"PASS_WORD=hunter2hunter2",
	`os.environ["DB_PASSWORD"] = "hunter2hunter2"`,
	`ENV["API_KEY"] ||= "hunter2hunter2"`,
	"curl -u admin:hunter2hunter2 https://api.example.com",
	"docker login -u bob -p hunter2hunter2 registry.example.com",
	"sshpass -p hunter2hunter2 ssh bob@host",
	"redis-server --requirepass hunter2hunter2",
	"htpasswd -b .htpasswd bob hunter2hunter2",
	"some-cli --token hunter2hunter2",
	"some-cli --api-key hunter2hunter2 --verbose",
	"gh secret set API_TOKEN --body hunter2hunter2",
	`curl -H "Cookie: session=hunter2hunter2" https://example.com`,
	`$pw = ConvertTo-SecureString "hunter2hunter2" -AsPlainText -Force`,
	"| DB_PASSWORD | hunter2hunter2 |",
	`token = """hunter2hunter2"""`,
	`requests.get(url, auth=("bob","hunter2hunter2"))`,
	`const password = getPass("hunter2hunter2");`,
	"redis-cli -a hunter2hunter2 ping",
	// I43-2: a flag value that starts with "-", and the --pass rows.
	"some-cli --password -Hunter2Hunter2",
	"some-cli --pass -Hunter2Hunter2",
	"some-cli --password=S3cretPass",
	`some-cli --api-key "-abcdefghij123456"`,
	// I43-3: a flow env entry, in either order.
	"- {name: DB_PASSWORD, value: hunter2hunter2}",
	`{"value": "hunter2hunter2", "name": "DB_PASSWORD"}`,
	// I43-4: an args array, and run-together flag names.
	`"args": ["--api-key", "hunter2hunter2"]`,
	"some-cli --apikey hunter2hunter2",
	"some-cli --authtoken hunter2hunter2",
	"some-cli --clientsecret hunter2hunter2",
	// I43-5: byte strings, typed declarations, an Authorization subscript,
	// XML, Docker and npm auth, and webhook URLs.
	`var jwtKey = []byte("hunter2hunter2")`,
	`jwtSecret := []byte("hunter2hunter2")`,
	`api_key = b"hunter2hunter2"`,
	`password = r"hunter2\hunter2"`,
	`TOKEN = ("hunter2hunter2")`,
	`const apiKey: string = "hunter2hunter2";`,
	`apiKey?: string = "hunter2hunter2"`,
	`const API_KEY: &str = "hunter2hunter2";`,
	`static API_TOKEN: &'static str = "hunter2hunter2";`,
	`val apiKey: String = "hunter2hunter2"`,
	`api_key: str = "hunter2hunter2"`,
	`var apiKey string = "hunter2hunter2"`,
	`headers["Authorization"] = "Token hunter2hunter2"`,
	`headers['Authorization'] = 'Bearer hunter2hunter2'`,
	"<password>hunter2hunter2</password>",
	`<apiKey type="prod">hunter2hunter2</apiKey>`,
	`<add key="ApiKey" value="hunter2hunter2" />`,
	`<add key="Stripe:SecretKey" value="hunter2hunter2"/>`,
	`"auth": "` + base64.StdEncoding.EncodeToString([]byte("bob:hunter2")) + `"`,
	"//registry.npmjs.org/:_auth=" + base64.StdEncoding.EncodeToString([]byte("bob:hunter2")),
	"_auth = " + base64.StdEncoding.EncodeToString([]byte("bob:hunter2hunter2")),
	// Built here, so no scanner takes them for live webhooks.
	"SLACK_HOOK=https://hooks.slack.com/services/" + "T0000000/B0000000/" + strings.Repeat("x", 24),
	"post to hooks.slack.com/services/" + "T0000000/B0000000/" + strings.Repeat("x", 24),
	"https://discord.com/api/webhooks/" + "123456789012345678/" + strings.Repeat("y", 30),
	// I43-6: a long setenv value, and a flag value that is no prose word.
	`monkeypatch.setenv("API_KEY", "hunter2hunter2")`,
	"some-cli --token toTheMoon99",
	"some-cli --token=to",
	"some-cli --api-key flagged123",
	// N55-2: a quoted flag value is never prose.
	`some-cli --passphrase "the cat sat on the mat 42"`,
	`some-cli --password 'a Zx9!pQ2#r'`,
	// N55-3: short literals count; only placeholders are kept.
	`os.Setenv("DB_PASSWORD", "s3cr3t!")`,
	`env["API_TOKEN"] = "abc1234"`,
	// N55-4: a pass name with a word that is not a count.
	`DB_PASS_MODE = "Zk29fjQ0aLm3ppQ7"`,
	// N55-7: a "$" password is not a reference.
	`os.Setenv("DB_PASSWORD", "$uperS3cret")`,
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
		if got, _ := Lines(line); got != "" {
			t.Errorf("Lines(%q) kept it", line)
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
		"this.token = session.accessToken;",
		"const token = generateToken(user);",
		"password := hashPassword(raw)",
		"let apiKey = config.apiKey;",
		"tokens.map(token => token.trim())",
		"$token = $request->token;",
		"apiKey := loadKey(cfg)",
		"login with the password reset link",
		// G34-1: lines near the new patterns that are not secrets.
		"generateToken(user)",
		"CancellationToken;",
		"max_tokens",
		"model --max-tokens 4096",
		"bypass_mode: enabled_always",
		"docker run -p 8080:80 -u 1000:1000 app",
		"git push -u origin main",
		"docker login --password-stdin registry.example.com",
		"htpasswd -B -c .htpasswd bob",
		"os.environ[\"API_KEY\"] = load()",
		`if env["API_KEY"] == "" {`,
		`os.Getenv("API_KEY", "")`,
		"| Name | Description |",
		"the cookie jar is empty",
		"some-cli --token --verbose",
		"codex --dangerously-bypass-approvals-and-sandbox exec",
		"- {name: MODE, value: production}",
		`"args": ["--verbose", "hunter2hunter2"]`,
		"data: " + base64.StdEncoding.EncodeToString([]byte("just some ordinary text here")),
		// I43-6 and I43-11: prose about a flag, a short setenv value, a
		// bare scheme, a count, and words that only end in "key".
		"Pass the --token flag to authenticate",
		"Use --token to pass the key.",
		"set --api-key or --password, which take precedence",
		`monkeypatch.setenv("API_KEY","test")`,
		`req.Header.Set("Authorization","Bearer "+token)`,
		`req.Header.Set("Authorization", "Bearer " + token)`,
		`headers["Authorization"] = "Bearer " + token`,
		"pass_count = 12345678",
		"PASS_MAX=12345678",
		// N55-3: placeholders and references stay.
		`monkeypatch.setenv("API_KEY", "dummy")`,
		`os.Setenv("API_KEY", "${API_KEY}")`,
		`os.environ["API_KEY"] = ""`,
		// N55-7: env-style references.
		`os.Setenv("API_KEY", "$API_KEY")`,
		`os.Setenv("API_KEY", "${api_key}")`,
		`os.Setenv("API_KEY", "$(TOKEN)")`,
		"some-cli --monkey 3",
		"xdotool --hotkey ctrlaltdel",
		`"args": ["--monkey", "banana"]`,
		`"args": ["--token", "--verbose"]`,
		"<key>CFBundleIdentifier</key>",
		`"auth": "required"`,
		"auth: anonymous",
		`const apiKey: string = getKey();`,
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
		for i, l := range strings.Split(string(b), "\n") {
			// Under -race, where regexps run far slower, every tenth source
			// line; the test rows above are always checked in full, and
			// go test without -race checks every source line (I43-10).
			if !raceOn || i%10 == 0 {
				lines = append(lines, l)
			}
		}
	}
	// Only unhinted lines can fail, and each is checked once (I43-10: the
	// package must stay fast under -race).
	seen := map[string]bool{}
	for _, l := range lines {
		if seen[l] || hinted(l) {
			continue
		}
		seen[l] = true
		if Token.MatchString(l) || assign.MatchString(l) || other.MatchString(l) || pendingKey.MatchString(l) || nameKey.MatchString(l) || flagKey.MatchString(l) || flagArg.MatchString(l) || authB64.MatchString(l) || setLit.MatchString(l) {
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

// TestTripleCap (I43-3): a triple-quoted secret value with no closing
// quotes drops at most maxTriple lines.
func TestTripleCap(t *testing.T) {
	in := "token = '''\n" + strings.Repeat("x\n", maxTriple+50) + "end"
	got, n := Lines(in)
	if n != maxTriple || !strings.HasSuffix(got, "end") {
		t.Errorf("removed %d lines, want %d", n, maxTriple)
	}
}

// TestLinear (I43-1, I43-10): on lines built to have many assign
// matches, factsOf runs at most once a line, and the bytes factsOf and
// the assign loop examine stay within a small multiple of the input; the
// old code rescanned the rest of the line once per match. The counts do
// not depend on the line's length, and rescanning would examine some n/2
// times the input, so 100 repeats show it as well as more would; fewer
// keep the package fast under -race (T6749).
func TestLinear(t *testing.T) {
	defer func() { work = nil }()
	for _, unit := range []string{
		"{key:f(bbbbbbbb),", "token := f(x) ", "name: DB_PASSWORD, value: ",
		`"--token", `, "key: !a ", `password = ("`, "--api-key -x ", "token = '''x ",
		// T6749: the new shapes.
		`k = []byte("`, "apiKey: string = f(x) ", "--token to ", "--monkey x ", `"--hotkey", "x", `,
		`"auth": "Ym9iOmh1bnRlcjI=", `, `_auth=YWJj `, "<password>x ", `headers["Authorization"] = "Bearer " + `,
		`pass_count = 12345678 `, "API_KEY = (",
		// N55: placeholders and quoted flag values.
		`f("API_KEY", "test", `, `env["API_KEY"] = "", `, `--password "the `,
	} {
		for _, n := range []int{25, 100} {
			in := strings.Repeat(unit, n)
			in += "\n" + in
			work = &workCount{}
			Lines(in)
			if work.facts > 2 || work.bytes > 4*len(in) {
				t.Errorf("%q x%d: %d factsOf calls, %d bytes examined for %d bytes", unit, n, work.facts, work.bytes, len(in))
			}
		}
	}
}
