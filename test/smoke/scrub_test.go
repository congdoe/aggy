package smoke

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestScrub (G32-4) runs smoke.sh's scrub function on stderr-shaped
// lines: no secret or terminal escape survives, and plain text does.
func TestScrub(t *testing.T) {
	script, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?ms)^scrub\(\) \{\n.*?^\}\n`).Find(script)
	if fn == nil {
		t.Fatal("no scrub function in smoke.sh")
	}
	in := strings.Join([]string{
		"error: token sk-ant-api03-abcdef at /w/x/c-mcp/server.py",
		"Authorization: Bearer abc.def",
		"authorization: Token SHORT1",
		"OPENAI_API_KEY=foo123 and api_key: \"zzz\"",
		"{'api_key': 'q1w2e3', 'password': 'hunter2'}",
		`{"client_secret":"s3cr"}`,
		"run --token TOK9 --api-key=AK7 --password pw1 done",
		"id 0123456789abcdef0123456789abcdef",
		"path /Users/me/.grok/x",
		"clip \x1b]52;c;c2VjcmV0\x07 bell\x07 csi \x1b[31mred\x1b[0m",
		"error: a value is required for --single",
	}, "\n") + "\n"
	cmd := exec.Command("bash", "-c", string(fn)+"\nscrub")
	cmd.Env = []string{"WORK=/w/x", "HOME=/Users/me", "PATH=/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader(in)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, secret := range []string{"sk-ant", "abc.def", "SHORT1", "foo123", "zzz", "q1w2e3", "hunter2", "s3cr", "TOK9", "AK7", "pw1",
		"0123456789abcdef", "/Users/me", "/w/x", "\x1b", "\x07", "c2VjcmV0"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrub kept %q:\n%s", secret, got)
		}
	}
	for _, keep := range []string{"$WORK/c-mcp/server.py", "~/.grok/x", "error: a value is required for --single", "run --token [redacted]", "done"} {
		if !strings.Contains(got, keep) {
			t.Errorf("scrub lost %q:\n%s", keep, got)
		}
	}
}
