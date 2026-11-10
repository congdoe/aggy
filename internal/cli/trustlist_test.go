package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/claudetrust"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/trustlist"
)

// tickTrustRows records dir's trust list answered with ids ticked, as a
// person's answer would, straight into aggy's trust file.
func tickTrustRows(t *testing.T, dir string, ids ...string) {
	t.Helper()
	tg, err := trustTargetFor(dir)
	if err != nil {
		t.Fatal(err)
	}
	path, err := trustFile()
	if err != nil {
		t.Fatal(err)
	}
	f, err := readTrustRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Folders == nil {
		f.Folders = map[string]trustlist.Folder{}
	}
	f.Folders[tg.key] = trustlist.Folder{Shown: ids, On: ids}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// trustWorld is a person at a terminal before a start, in a new repo, with
// the CLIs in installed on PATH (as trustLookPath sees it).
func trustWorld(t *testing.T, installed ...string) (homeDir, repo string) {
	t.Helper()
	homeDir = isolate(t)
	repo = tempRepo(t)
	oldLook, oldTerm, oldA := trustLookPath, offerTerminal, typeAhead
	trustLookPath = func(bin string) bool { return slices.Contains(installed, bin) }
	offerTerminal, typeAhead = func() bool { return true }, 0
	t.Cleanup(func() { trustLookPath, offerTerminal, typeAhead = oldLook, oldTerm, oldA })
	for _, v := range []string{"AGGY_DELEGATED", "AGGY_SOCKET", "AGGY_PARENT", "AGGY_VENDOR", trustAskEnv} {
		t.Setenv(v, "")
	}
	return homeDir, repo
}

// startIn is one on-screen start in dir, answering with typed; it returns
// what the start printed and the note for the launch.
func startIn(dir string, cfg config.Config, typed string) (string, string) {
	var out bytes.Buffer
	note := trustAtStart(context.Background(), dir, cfg, os.Getenv, strings.NewReader(typed), &out)
	return out.String(), note
}

// folderAnswer is what aggy recorded for dir's list.
func folderAnswer(t *testing.T, dir string) (trustlist.Folder, bool) {
	t.Helper()
	tg, err := trustTargetFor(dir)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := trustFile()
	f, err := readTrustRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := f.Folders[tg.key]
	return a, ok
}

func freeOn() config.Config {
	c := config.Default()
	c.OpenCode.FreeModels = true
	return c
}

// TestTrustAtStart (T6977): the first start in a folder asks once; All ticks
// the harnesses, not a model source; a row added later asks once more with
// the earlier answers kept; not now asks again; never does not.
func TestTrustAtStart(t *testing.T) {
	t.Run("first start asks, second does not", func(t *testing.T) {
		_, repo := trustWorld(t, "claude", "opencode")
		out, note := startIn(repo, config.Default(), "1\n\n")
		if note != "" || !strings.Contains(out, "Claude Code") || !strings.Contains(out, "Saved this folder's answer") {
			t.Fatalf("first: note %q out %q", note, out)
		}
		a, _ := folderAnswer(t, repo)
		if !slices.Equal(a.On, []string{"claude", "opencode"}) || !slices.Equal(a.Shown, []string{"claude", "opencode"}) {
			t.Errorf("answer %+v", a)
		}
		key, _ := trustTargetFor(repo)
		if !repoTrusted(key.key) {
			t.Error("a harness ticked did not trust the repo for aggy's rules")
		}
		if on, _ := openCodeTrusted(repo); !on {
			t.Error("the OpenCode row ticked did not trust it for OpenCode")
		}
		if out, note = startIn(repo, config.Default(), "2\n\n"); out != "" || note != "" {
			t.Errorf("second start asked: %q %q", out, note)
		}
	})
	t.Run("All leaves a model source unticked", func(t *testing.T) {
		_, repo := trustWorld(t, "claude", "opencode")
		if out, _ := startIn(repo, freeOn(), "1\n\n"); !strings.Contains(out, "OpenCode free models") || !strings.Contains(out, "OpenCode's own service") {
			t.Fatalf("the source row or its warning is not shown: %q", out)
		}
		a, _ := folderAnswer(t, repo)
		if slices.Contains(a.On, trustRowOpenCodeFree) || !slices.Contains(a.Shown, trustRowOpenCodeFree) || openCodeFreeHere(repo) {
			t.Errorf("All ticked a source: %+v", a)
		}
	})
	t.Run("a new row asks once, earlier answers kept", func(t *testing.T) {
		_, repo := trustWorld(t, "claude", "opencode")
		startIn(repo, config.Default(), "2\n\n") // Claude only; OpenCode left unticked
		out, _ := startIn(repo, freeOn(), "\n")
		if !strings.Contains(out, "OpenCode free models   new") || !strings.Contains(out, "earlier answers are kept") || !strings.Contains(out, " 2 [x] Claude Code") {
			t.Fatalf("re-ask: %q", out)
		}
		a, _ := folderAnswer(t, repo)
		if !slices.Equal(a.On, []string{"claude"}) || !slices.Contains(a.Shown, trustRowOpenCodeFree) {
			t.Errorf("answer %+v", a)
		}
		if out, note := startIn(repo, freeOn(), "4\n\n"); out != "" || note != "" {
			t.Errorf("asked again for a row left unticked: %q %q", out, note)
		}
	})
	t.Run("not now asks again, never does not", func(t *testing.T) {
		_, repo := trustWorld(t, "claude")
		if out, _ := startIn(repo, config.Default(), "n\n"); !strings.Contains(out, "Not now") {
			t.Fatalf("not now: %q", out)
		}
		if _, ok := folderAnswer(t, repo); ok {
			t.Error("not now recorded an answer")
		}
		if out, _ := startIn(repo, config.Default(), "x\n"); !strings.Contains(out, "Never for this folder") {
			t.Fatalf("never: %q", out)
		}
		if a, _ := folderAnswer(t, repo); !a.Never || len(a.On) != 0 {
			t.Errorf("never: %+v", a)
		}
		if out, note := startIn(repo, freeOn(), "1\n\n"); out != "" || note != "" {
			t.Errorf("asked after never, a new row included: %q %q", out, note)
		}
	})
	t.Run("the end of input is not now", func(t *testing.T) {
		_, repo := trustWorld(t, "claude")
		if out, _ := startIn(repo, config.Default(), "1\n"); !strings.Contains(out, "Not now") {
			t.Errorf("%q", out)
		}
		if _, ok := folderAnswer(t, repo); ok {
			t.Error("a list never confirmed recorded an answer")
		}
	})
	t.Run("a paste picks nothing", func(t *testing.T) {
		_, repo := trustWorld(t, "claude")
		startIn(repo, config.Default(), "\x1b[200~1\n\n\x1b[201~\n\n")
		if a, _ := folderAnswer(t, repo); len(a.On) != 0 {
			t.Errorf("a paste ticked or confirmed: %+v", a)
		}
	})
}

// TestTrustNotAsked (T6977): with no person at a terminal, in a model's tool
// shell, in a borrow child, turned off, or at home or /, nothing is asked and
// nothing written; what is not answered counts as off, said in one line.
func TestTrustNotAsked(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		terminal bool
		dir      string // "home", "/", or the repo
		note     string // in the note, or "" for none
	}{
		{"no terminal", nil, false, "", "not answered"},
		{"a model's tool shell", map[string]string{"AGGY_SOCKET": "/s", "AGGY_PARENT": "p"}, true, "", "not answered"},
		{"the vendor's variable alone", map[string]string{"AGGY_VENDOR": "claude"}, true, "", "not answered"},
		{"a borrow child", map[string]string{"AGGY_DELEGATED": "1"}, true, "", ""},
		{"turned off", map[string]string{trustAskEnv: "0"}, true, "", ""},
		{"home", nil, true, "home", ""},
		{"/", nil, true, "/", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, repo := trustWorld(t, "claude", "opencode")
			offerTerminal = func() bool { return c.terminal }
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			dir := repo
			switch c.dir {
			case "home":
				dir = h
			case "/":
				dir = "/"
			}
			out, note := startIn(dir, config.Default(), "1\n\n")
			if out != "" {
				t.Errorf("asked: %q", out)
			}
			if c.note == "" && note != "" || c.note != "" && (!strings.Contains(note, c.note) || !strings.Contains(note, "Claude Code, OpenCode") || !strings.Contains(note, "count as off")) {
				t.Errorf("note %q, want %q", note, c.note)
			}
			aggy, _ := home.Dir()
			if _, err := os.Stat(aggy); err == nil {
				if entries, _ := os.ReadDir(aggy); len(entries) > 0 {
					t.Errorf("wrote %v", entries)
				}
			}
		})
	}
	// A model's tool shell cannot answer from the shell either.
	_, repo := trustWorld(t, "claude")
	atKeyboard(t, "1\n\n")
	t.Setenv("AGGY_SOCKET", "/s")
	if code, _, errb := run("trust", repo); code != 1 || !strings.Contains(errb, "tool shell") {
		t.Errorf("aggy trust from a tool shell: %d %q", code, errb)
	}
}

// claudeJSON is a Claude config with what aggy must keep and never copy.
const claudeJSON = `{
  "numStartups": 12345678901234567890,
  "oauthAccount": {"emailAddress": "SECRET-MARKER-ACCOUNT"},
  "projects": {
    "/elsewhere": {"history": ["SECRET-MARKER-HISTORY"], "hasTrustDialogAccepted": false}
  }
}
`

// TestTrustClaudeKey (T6977): the Claude Code row writes Claude's own trust
// key for that folder alone, through the manifest with no copy of the file,
// keeps every other key, and aggy unwire puts back just that key, keeping what
// Claude wrote since.
func TestTrustClaudeKey(t *testing.T) {
	h, repo := trustWorld(t, "claude")
	path := filepath.Join(h, ".claude.json")
	if err := os.WriteFile(path, []byte(claudeJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := startIn(repo, config.Default(), "2\n\n")
	if !strings.Contains(out, "gains "+claudetrust.Key) {
		t.Fatalf("%q", out)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := claudetrust.State(b, repo); st != claudetrust.WasTrue {
		t.Fatalf("the key is not set for the repo: %s", b)
	}
	var got, want map[string]any
	_ = json.Unmarshal(b, &got)
	_ = json.Unmarshal([]byte(claudeJSON), &want)
	delete(got["projects"].(map[string]any), repo)
	if !reflect.DeepEqual(got, want) || !strings.Contains(string(b), "12345678901234567890") {
		t.Errorf("something else changed:\n%s", b)
	}
	// Nothing of the file was copied into aggy's state, the diagnostic log
	// included.
	aggy, _ := home.Dir()
	_ = filepath.WalkDir(aggy, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if c, _ := os.ReadFile(p); bytes.Contains(c, []byte("SECRET-MARKER")) {
				t.Errorf("%s holds the file's content", p)
			}
		}
		return nil
	})

	// Claude rewrites the file its own way while it runs.
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	doc["lastSeen"] = "after"
	doc["projects"].(map[string]any)[repo].(map[string]any)["allowedTools"] = []any{}
	re, _ := json.MarshalIndent(doc, "", "    ")
	if err := os.WriteFile(path, re, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errb := run("unwire", "--all", "--dry-run"); code != 0 || strings.Contains(out, "SECRET-MARKER") || !strings.Contains(out, "put back "+claudetrust.Key) {
		t.Errorf("unwire dry run: %d %q %q", code, out, errb)
	}
	if code, out, errb := run("unwire", "--all"); code != 0 {
		t.Fatalf("unwire: %d %q %q", code, out, errb)
	}
	b, _ = os.ReadFile(path)
	if st, _ := claudetrust.State(b, repo); st != claudetrust.NoKey {
		t.Errorf("after unwire the key is %q: %s", st, b)
	}
	if !strings.Contains(string(b), `"lastSeen"`) || !strings.Contains(string(b), "allowedTools") || !strings.Contains(string(b), "SECRET-MARKER-HISTORY") {
		t.Errorf("unwire lost what Claude wrote: %s", b)
	}
	if repoTrusted(mustKey(t, repo)) {
		t.Error("aggy's record stayed after unwire")
	}
}

func mustKey(t *testing.T, dir string) string {
	t.Helper()
	tg, err := trustTargetFor(dir)
	if err != nil {
		t.Fatal(err)
	}
	return tg.key
}

// TestTrustClaudeFileRefusals (T6977): no file is not made; a file that does
// not parse is left and said; a file changed underneath is refused, and what
// was written before it is named.
func TestTrustClaudeFileRefusals(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		h, repo := trustWorld(t, "claude")
		out, _ := startIn(repo, config.Default(), "2\n\n")
		if !strings.Contains(out, "not there yet") {
			t.Errorf("%q", out)
		}
		if _, err := os.Stat(filepath.Join(h, ".claude.json")); !os.IsNotExist(err) {
			t.Error("~/.claude.json was made")
		}
		if a, _ := folderAnswer(t, repo); !slices.Equal(a.On, []string{"claude"}) {
			t.Errorf("the rest of the answer was not saved: %+v", a)
		}
	})
	t.Run("unparsable", func(t *testing.T) {
		h, repo := trustWorld(t, "claude")
		path := filepath.Join(h, ".claude.json")
		const broken = `{"projects": {SECRET-MARKER`
		if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
			t.Fatal(err)
		}
		out, _ := startIn(repo, config.Default(), "2\n\n")
		if !strings.Contains(out, "refused") || !strings.Contains(out, "trust.bad_file") || strings.Contains(out, "SECRET-MARKER") {
			t.Errorf("%q", out)
		}
		if b, _ := os.ReadFile(path); string(b) != broken {
			t.Error("a file that does not parse was changed")
		}
	})
	t.Run("changed underneath", func(t *testing.T) {
		h, repo := trustWorld(t, "claude")
		path := filepath.Join(h, ".claude.json")
		if err := os.WriteFile(path, []byte(claudeJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		restore := patch.SetTestHookBeforeWrite(func(file string) error {
			if filepath.Base(file) == "trust.json" { // written first, after Claude's file was read
				return os.WriteFile(path, []byte(`{"claude": "wrote this meanwhile"}`), 0o600)
			}
			return nil
		})
		defer restore()
		out, _ := startIn(repo, config.Default(), "2\n\n")
		if !strings.Contains(out, "changed while aggy was writing it") || !strings.Contains(out, "aggy unwire --run") {
			t.Errorf("%q", out)
		}
		if b, _ := os.ReadFile(path); string(b) != `{"claude": "wrote this meanwhile"}` {
			t.Errorf("Claude's newer file was overwritten: %s", b)
		}
	})
}

// TestTrustDryRun: aggy trust --dry-run shows what ticking every harness
// changes, never the content of Claude's file, and writes and creates
// nothing.
func TestTrustDryRun(t *testing.T) {
	h, repo := trustWorld(t, "claude", "opencode")
	path := filepath.Join(h, ".claude.json")
	if err := os.WriteFile(path, []byte(claudeJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errb := run("trust", "--dry-run", repo)
	if code != 0 || !strings.Contains(out, "set "+claudetrust.Key) || !strings.Contains(out, "opencode-trust.json") || strings.Contains(out, "SECRET-MARKER") {
		t.Errorf("%d %q %q", code, out, errb)
	}
	if b, _ := os.ReadFile(path); string(b) != claudeJSON {
		t.Error("the dry run changed Claude's file")
	}
	aggy, _ := home.Dir()
	if entries, _ := os.ReadDir(aggy); len(entries) > 0 {
		t.Errorf("the dry run wrote %v", entries)
	}
}

// TestTrustOpenCodeShortcut: aggy trust --opencode ticks the OpenCode row in
// the folder's answer, so a start asks only about the rest.
func TestTrustOpenCodeShortcut(t *testing.T) {
	_, repo := trustWorld(t, "claude", "opencode")
	atKeyboard(t, "y\n")
	if code, out, errb := run("trust", "--opencode", repo); code != 0 {
		t.Fatalf("%d %q %q", code, out, errb)
	}
	a, _ := folderAnswer(t, repo)
	if !slices.Equal(a.On, []string{trustRowOpenCode}) || !slices.Equal(a.Shown, []string{trustRowOpenCode}) {
		t.Errorf("answer %+v", a)
	}
	out, _ := startIn(repo, config.Default(), "\n")
	if !strings.Contains(out, " 3 [x] OpenCode") || !strings.Contains(out, " 2 [ ] Claude Code   new") {
		t.Errorf("the start's list: %q", out)
	}
}
