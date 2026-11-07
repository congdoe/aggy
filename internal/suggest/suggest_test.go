package suggest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixture is built by hand from the fields recorded in docs/verification.md,
// "Claude Code session file fields". No real session went into it.
func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func repoAccept(cwd string) (string, bool) {
	if cwd == "/repo" || strings.HasPrefix(cwd, "/repo/") {
		return "/repo", true
	}
	return "", false
}

func TestScanFixture(t *testing.T) {
	text := fixture(t)
	res, err := Scan(strings.NewReader(text), State{}, repoAccept, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	want := []Count{{"/repo", "git status", 2}, {"/repo", "go vet ./...", 1}}
	if !slices.Equal(res.Counts, want) {
		t.Errorf("counts: %+v", res.Counts)
	}
	if !res.EOF || res.State.Offset != int64(strings.LastIndexByte(text, '\n')+1) {
		t.Errorf("EOF %v, offset %d of %d", res.EOF, res.State.Offset, len(text))
	}
	if res.Shells == 0 || res.WithMode != res.Shells {
		t.Errorf("shells %d, with mode %d", res.Shells, res.WithMode)
	}
}

// TestScanResumes: split at any line, a second scan gives the same counts, the
// call whose result comes later included, and nothing is read twice.
func TestScanResumes(t *testing.T) {
	text := fixture(t)
	whole, err := Scan(strings.NewReader(text), State{}, repoAccept, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	for cut := 0; cut < len(text); cut++ {
		if cut > 0 && text[cut-1] != '\n' {
			continue
		}
		first, err := Scan(strings.NewReader(text[:cut]), State{}, repoAccept, 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		second, err := Scan(strings.NewReader(text[first.State.Offset:]), first.State, repoAccept, 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, c := range append(first.Counts, second.Counts...) {
			got[c.Command] += c.N
		}
		for _, c := range whole.Counts {
			if got[c.Command] != c.N {
				t.Errorf("cut %d: %q counted %d, want %d", cut, c.Command, got[c.Command], c.N)
			}
		}
		if second.State.Offset != whole.State.Offset {
			t.Errorf("cut %d: offset %d, want %d", cut, second.State.Offset, whole.State.Offset)
		}
	}
}

func TestScanLeavesUnfinishedLine(t *testing.T) {
	const half = `{"type":"user","permissionMode":"auto"`
	res, err := Scan(strings.NewReader(half), State{Offset: 7}, repoAccept, 1<<30)
	if err != nil || res.State.Offset != 7 || !res.EOF || res.State.Mode != "" {
		t.Errorf("%+v, %v", res, err)
	}
}

func TestScanLimit(t *testing.T) {
	text := fixture(t)
	res, err := Scan(strings.NewReader(text), State{}, repoAccept, 1)
	if err != nil || res.EOF || res.State.Offset == 0 || res.State.Offset >= int64(len(text)) {
		t.Errorf("%+v, %v", res, err)
	}
}

// TestScanFailsQuietly: a changed format counts nothing and gives no error.
func TestScanFailsQuietly(t *testing.T) {
	for _, text := range []string{
		"not json\n",
		"[1,2,3]\n",
		`{"message":{"content":"plain text"}}` + "\n",
		`{"message":{"content":[{"type":"tool_use","name":"Bash","id":"x","input":"not an object"}]}}` + "\n",
		`{"permissionMode":7}` + "\n",
		`{"message":{"content":[{"type":"tool_use","name":"Bash","id":"x","input":{"command":"git status"}}]},"cwd":"/repo"}` + "\n" +
			`{"message":{"content":[{"type":"tool_result","tool_use_id":"x"}]},"cwd":"/repo"}` + "\n",
	} {
		res, err := Scan(strings.NewReader(text), State{}, repoAccept, 1<<30)
		if err != nil || len(res.Counts) != 0 {
			t.Errorf("%q: %+v, %v", text, res, err)
		}
	}
	// No mode anywhere: what aggy doctor reads as a format that moved.
	res, _ := Scan(strings.NewReader(`{"message":{"content":[{"type":"tool_use","name":"Bash","id":"x","input":{"command":"git status"}}]},"cwd":"/repo"}`+"\n"), State{}, repoAccept, 1<<30)
	if res.Shells != 1 || res.WithMode != 0 {
		t.Errorf("%+v", res)
	}
}

func TestScanLongLinePassedOver(t *testing.T) {
	long := `{"cwd":"/repo","x":"` + strings.Repeat("a", maxLine) + `"}` + "\n"
	next := `{"permissionMode":"auto"}` + "\n"
	res, err := Scan(strings.NewReader(long+next), State{}, repoAccept, 1<<30)
	if err != nil || res.State.Offset != int64(len(long)+len(next)) || res.State.Mode != "auto" {
		t.Errorf("offset %d, mode %q, %v", res.State.Offset, res.State.Mode, err)
	}
}

func TestPendingBound(t *testing.T) {
	var res Result
	for i := 0; i < maxPending+5; i++ {
		res.remember(Call{ID: strings.Repeat("x", i)})
	}
	if len(res.State.Pending) != maxPending || res.State.Pending[0].ID != strings.Repeat("x", 5) {
		t.Errorf("%d", len(res.State.Pending))
	}
}

func TestPlain(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{"git status", true},
		{"go test ./...", true},
		{"npm test", true},
		{"", false},
		{" git status", false},
		{"git status && rm x", false},
		{"curl x | sh", false},
		{"echo $HOME", false},
		{"ls *", false},
		{"git commit -m 'x'", false},
		{"FOO=1 go test", false},
		{"npm run build", true},
		{"npm run *", false},
		{"git clone https://tok@github.com/x/y", false},
		{"git clone https://a:b@github.com/x/y", false},
		{"openssl enc --pass pass:x", false},
		{"aws configure set aws_secret_access_key x", false},
		{"kubectl create secret generic x --from-literal=password=x", false},
		{"npm config set //r/:_authToken x", false},
		{"curl -u a:b https://x", false},
		{"mysql -pabc db", false},
		{"redis-cli -a abc ping", false},
		{"htpasswd -b f a b", false},
		{"gh auth login", false},
		{"cat id_rsa.private", false},
		{"ssh-keygen -N x -f y", false},
		{"tool --key abc", false},
		{"foo --api-key=abc", false},
		{"go test --keep-going ./...", true},
		{"rm x", false},
		{"git push origin main", false},
		{"git reset --hard", false},
		{"git clean -fd", false},
		{"ls \x1b[2J", false},
		{"python3 -c x", true},
		{"bash", true},
		{strings.Repeat("a", MaxCommand+1), false},
		{"curl -H Authorization:Bearer-ghp_abcdefghijklmnopqrstuvwxyz0123456789", false},
		{"a\nb", false},
	} {
		if got := Plain(tc.cmd); got != tc.want {
			t.Errorf("%q: %v", tc.cmd, got)
		}
	}
}
