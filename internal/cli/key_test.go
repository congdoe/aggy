package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/keyring"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/store"
)

// keyWorld is a temp HOME and state folder, a person at a terminal, and the
// fake keychain tool (of kind) keeping its items in a folder outside HOME. It
// returns that folder and HOME.
func keyWorld(t *testing.T, kind keyring.Kind) (chain, home string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("AGGY_HOME", filepath.Join(home, ".aggy"))
	for _, k := range []string{"AGGY_DELEGATED", "AGGY_SOCKET", "AGGY_PARENT", "AGGY_VENDOR"} {
		t.Setenv(k, "")
	}
	if err := os.MkdirAll(filepath.Join(home, ".aggy"), 0o700); err != nil {
		t.Fatal(err)
	}
	chain = t.TempDir()
	t.Setenv("AGGY_FAKE_KEYCHAIN", chain)
	bin := fakecli.Build(t).Keychain
	oldHelper, oldTerm, oldOffer, oldStdin, oldIn, oldAhead := keyHelper, keyTerminal, offerTerminal, stdinIsTerminal, offerIn, typeAhead
	t.Cleanup(func() {
		keyHelper, keyTerminal, offerTerminal, stdinIsTerminal, offerIn, typeAhead = oldHelper, oldTerm, oldOffer, oldStdin, oldIn, oldAhead
	})
	keyHelper = func() (keyring.Helper, error) { return keyring.Helper{Kind: kind, Path: bin}, nil }
	offerTerminal = func() bool { return true }
	stdinIsTerminal = func() bool { return true }
	typeAhead = 0
	offerIn = strings.NewReader("")
	return chain, home
}

// typed makes the keychain tool's "terminal" a pipe holding what the person
// types: the value, twice for security's retype prompt.
func typed(t *testing.T, kind keyring.Kind, value string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	in := value + "\n" + value + "\n"
	if kind == keyring.SecretTool {
		in = value
	}
	if _, err := w.WriteString(in); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	t.Cleanup(func() { _ = r.Close() })
	keyTerminal = func() *os.File { return r }
}

func runKeyCmd(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(append([]string{"key"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// treeHolds reports the files under root that hold b.
func treeHolds(t *testing.T, root string, b []byte, skip ...string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		for _, s := range skip {
			if p == s || strings.HasPrefix(p, s+string(filepath.Separator)) {
				return nil
			}
		}
		data, err := os.ReadFile(p)
		if err == nil && bytes.Contains(data, b) {
			hits = append(hits, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// TestKeyCycle (T6931): add, list, a refused second key for the provider, a
// replace, and remove, for both tools. The value reaches the keychain and
// nothing else: not stdout, stderr, the index, the diagnostic log, or any
// file under HOME.
func TestKeyCycle(t *testing.T) {
	for _, kind := range []keyring.Kind{keyring.Security, keyring.SecretTool} {
		t.Run(keyring.Helper{Kind: kind}.Name(), func(t *testing.T) {
			chain, home := keyWorld(t, kind)
			const canary = "sk-ant-canary-T6931-0123456789"
			var outs []string
			step := func(want int, args ...string) (string, string) {
				t.Helper()
				code, out, errOut := runKeyCmd(args...)
				outs = append(outs, out, errOut)
				if code != want {
					t.Fatalf("aggy key %q = %d, want %d\nstdout %s\nstderr %s", args, code, want, out, errOut)
				}
				return out, errOut
			}
			typed(t, kind, canary)
			out, _ := step(0, "add", "anth", "--provider", "anthropic")
			if !strings.Contains(out, "Stored key anth") {
				t.Errorf("add said %q", out)
			}
			if b, err := os.ReadFile(filepath.Join(chain, "item."+keyring.Service+".anth@anthropic")); err != nil || string(b) != canary {
				t.Fatalf("keychain item = %q, %v", b, err)
			}
			out, _ = step(0, "list")
			wantState := map[keyring.Kind]string{keyring.Security: "present", keyring.SecretTool: "not checked"}[kind]
			if !strings.Contains(out, "anth") || !strings.Contains(out, "anthropic") || !strings.Contains(out, wantState) {
				t.Errorf("list = %q", out)
			}
			_, errOut := step(1, "add", "other", "--provider", "anthropic")
			if !strings.Contains(errOut, "key.provider_taken") {
				t.Errorf("a second key for the provider: %q", errOut)
			}
			offerIn = strings.NewReader("n\n")
			step(1, "add", "anth", "--provider", "anthropic")
			offerIn = strings.NewReader("y\n")
			typed(t, kind, canary+"b")
			step(0, "add", "anth", "--provider", "anthropic")
			if b, _ := os.ReadFile(filepath.Join(chain, "item."+keyring.Service+".anth@anthropic")); string(b) != canary+"b" {
				t.Errorf("replaced item = %q", b)
			}
			offerIn = strings.NewReader("y\n")
			out, _ = step(0, "remove", "anth")
			if !strings.Contains(out, "Deleted key anth") {
				t.Errorf("remove said %q", out)
			}
			if _, err := os.Stat(filepath.Join(chain, "item."+keyring.Service+".anth@anthropic")); err == nil {
				t.Error("the keychain item outlived remove")
			}
			out, _ = step(0, "list")
			if !strings.Contains(out, "No keys") {
				t.Errorf("list after remove = %q", out)
			}
			for _, o := range outs {
				if strings.Contains(o, canary) {
					t.Errorf("printed the key: %q", o)
				}
			}
			if hits := treeHolds(t, home, []byte(canary)); len(hits) > 0 {
				t.Errorf("the key is in files under HOME: %q", hits)
			}
			calls, err := os.ReadFile(filepath.Join(chain, "calls.jsonl"))
			if err != nil || bytes.Contains(calls, []byte(canary)) {
				t.Errorf("the key reached the tool's arguments, or no calls: %v", err)
			}
		})
	}
}

// TestKeyGate: add and remove refuse anyone but a person at a terminal, and
// run no keychain tool; list runs anywhere and prints names only.
func TestKeyGate(t *testing.T) {
	cases := []struct {
		name  string
		env   string
		term  bool
		stdin bool
	}{
		{"borrow child", "AGGY_DELEGATED", true, true},
		{"session tool shell", "AGGY_SOCKET", true, true},
		{"session vendor", "AGGY_VENDOR", true, true},
		{"no terminal", "", false, false},
		{"stdin only", "", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chain, _ := keyWorld(t, keyring.Security)
			if c.env != "" {
				t.Setenv(c.env, "1")
			}
			offerTerminal = func() bool { return c.term }
			stdinIsTerminal = func() bool { return c.stdin }
			typed(t, keyring.Security, "sk-x")
			for _, args := range [][]string{{"add", "anth"}, {"remove", "anth"}} {
				code, _, errOut := runKeyCmd(args...)
				if code != 1 || !strings.Contains(errOut, "permissions.not_at_keyboard") {
					t.Errorf("aggy key %q = %d, %q", args, code, errOut)
				}
			}
			if _, err := os.Stat(filepath.Join(chain, "calls.jsonl")); err == nil {
				t.Error("the keychain tool ran")
			}
		})
	}
}

// TestKeyNoReader: there is no command that prints a key, and a word that
// could be a key is not printed back.
func TestKeyNoReader(t *testing.T) {
	chain, _ := keyWorld(t, keyring.Security)
	const canary = "sk-canary-ARGV-987654321"
	for _, args := range [][]string{{"show", "anth"}, {"get", "anth"}, {"export"}, {"cat", "anth"}, {"read", "anth"}, {canary}} {
		code, out, errOut := runKeyCmd(args...)
		if code != 2 || strings.Contains(out+errOut, canary) {
			t.Errorf("aggy key %q = %d\n%s%s", args, code, out, errOut)
		}
	}
	for _, args := range [][]string{{"add", canary}, {"add", "anth", "--provider", canary}, {"add", "anth", "--" + canary}, {"remove", canary}, {"add", "anth", canary}} {
		_, out, errOut := runKeyCmd(args...)
		if strings.Contains(out+errOut, canary) {
			t.Errorf("aggy key %q printed its word back:\n%s%s", args, out, errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(chain, "calls.jsonl")); err == nil {
		t.Error("the keychain tool ran")
	}
}

// TestKeyRefusals: an unsupported system, a never provider, and a tool that
// fails all refuse with their code, and record nothing in the index.
func TestKeyRefusals(t *testing.T) {
	chain, _ := keyWorld(t, keyring.Security)
	bin := filepath.Join(chain, "none")
	keyHelper = func() (keyring.Helper, error) {
		return keyring.Helper{Kind: keyring.Security, Path: bin}, keyring.ErrUnsupported
	}
	if code, _, errOut := runKeyCmd("add", "anth"); code != 1 || !strings.Contains(errOut, "key.unsupported") {
		t.Errorf("unsupported: %d %q", code, errOut)
	}
	keyWorld(t, keyring.Security)
	for _, p := range []string{"claude-agent", "opencode"} {
		if code, _, errOut := runKeyCmd("add", "k", "--provider", p); code != 1 || !strings.Contains(errOut, "key.bad_name") {
			t.Errorf("provider %s: %d %q", p, code, errOut)
		}
	}
	// The person types nothing: security's retype does not match, so it fails.
	typed(t, keyring.SecretTool, "")
	keyTerminal = func() *os.File { f, _ := os.Open(os.DevNull); return f }
	if code, _, errOut := runKeyCmd("add", "anth"); code != 1 || !strings.Contains(errOut, "key.helper_failed") {
		t.Errorf("tool failed: %d %q", code, errOut)
	}
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if keys, _ := s.Keys(context.Background()); len(keys) != 0 {
		t.Errorf("a refused add recorded %+v", keys)
	}
}

// TestKeyUnwireNote: aggy unwire --all never deletes a key; it names them and
// the command that does.
func TestKeyUnwireNote(t *testing.T) {
	keyWorld(t, keyring.Security)
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.PutKey(context.Background(), store.Key{Name: "anth", Provider: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	note := keysLeftNote(context.Background(), s)
	if !strings.Contains(note, "anth") || !strings.Contains(note, "aggy key remove") {
		t.Errorf("note = %q", note)
	}
}

// TestOpenCodeKeysLaunch (T6931): a stored key reaches OpenCode only for a
// launch a person started, only for a provider the repo lets in, and only as
// a 0600 file the config names; the config itself never holds the value.
func TestOpenCodeKeysLaunch(t *testing.T) {
	const canary = "sk-ant-canary-launch-55555"
	cases := []struct {
		name     string
		person   bool
		repo     config.RepoConfig
		readErr  error
		model    string
		wantRead bool
		wantKey  bool
		wantErr  string
		wantLine string
	}{
		{name: "passed", person: true, wantRead: true, wantKey: true, wantLine: "OpenCode gets your key anth for anthropic"},
		{name: "model on the key's provider", person: true, model: "anthropic/claude-x", wantRead: true, wantKey: true},
		{name: "not a person", person: false, wantLine: "only to a launch a person starts"},
		{name: "not a person, model needs it", person: false, model: "anthropic/claude-x", wantErr: "not on a provider allowed here"},
		{name: "repo leaves it out", person: true, repo: config.RepoConfig{AllowedVendors: []adapter.Vendor{"opencode", "codex"}}, wantLine: "allowed_vendors does not list anthropic"},
		{name: "repo lists it", person: true, repo: config.RepoConfig{AllowedVendors: []adapter.Vendor{"opencode", "codex", "anthropic"}}, wantRead: true, wantKey: true},
		{name: "keychain refuses", person: true, readErr: errors.New("security exited 51"), wantRead: true, wantLine: "key anth is not handed to OpenCode, so anthropic is not offered"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, dir := openCodeWorld(t)
			reads := 0
			in := openCodeInput{cfg: effective(config.Default(), c.repo), caps: openCodeCaps, cwd: repo, dir: dir, model: c.model,
				keys: []store.Key{{Name: "anth", Provider: "anthropic"}}, person: c.person,
				readKey: func(k store.Key) (*keyring.Value, error) {
					reads++
					if k.Name != "anth" || k.Provider != "anthropic" {
						t.Errorf("read key %+v", k)
					}
					if c.readErr != nil {
						return nil, c.readErr
					}
					return keyring.Parse([]byte(canary + "\n"))
				}}
			s, err := prepareOpenCode(in)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (reads > 0) != c.wantRead {
				t.Errorf("reads = %d, want read %v", reads, c.wantRead)
			}
			if c.wantLine != "" && !slices.ContainsFunc(s.warns, func(w string) bool { return strings.Contains(w, c.wantLine) }) {
				t.Errorf("warns %q lack %q", s.warns, c.wantLine)
			}
			b, err := os.ReadFile(s.launch.ConfigFile)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte(canary)) {
				t.Error("the config holds the key")
			}
			var doc struct {
				Provider map[string]struct {
					Options struct {
						APIKey string `json:"apiKey"`
					} `json:"options"`
				} `json:"provider"`
				Policies []struct{ Action, Resource, Effect string }
			}
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatal(err)
			}
			last := ""
			for _, p := range doc.Policies {
				if p.Action == "provider.use" && p.Resource == "anthropic" {
					last = p.Effect
				}
			}
			keyFile := filepath.Join(dir, "opencode", "keys", "anthropic")
			if !c.wantKey {
				if last != "deny" || len(doc.Provider) != 0 {
					t.Errorf("anthropic: last %q, provider %+v; want denied with no key", last, doc.Provider)
				}
				if _, err := os.Stat(keyFile); err == nil {
					t.Error("a key file was written")
				}
				return
			}
			if last != "allow" || doc.Provider["anthropic"].Options.APIKey != "{file:"+keyFile+"}" {
				t.Errorf("anthropic: last %q, provider %+v", last, doc.Provider)
			}
			fi, err := os.Stat(keyFile)
			if err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("key file: %v, %v", fi, err)
			}
			if kb, _ := os.ReadFile(keyFile); string(kb) != canary {
				t.Errorf("key file holds %q", kb)
			}
			if hits := treeHolds(t, filepath.Dir(dir), []byte(canary), keyFile); len(hits) > 0 {
				t.Errorf("the key is in other files: %q", hits)
			}
		})
	}
}

// TestKeyNeverReachesBorrows (T6931): with a key stored and the same value in
// aggy's own environment under each vendor's API-key variable, a borrow child
// of Claude Code, Codex, or Grok gets it nowhere (environment, arguments,
// stdin, prompt, files it is given), and no borrow runs the keychain tool.
func TestKeyNeverReachesBorrows(t *testing.T) {
	const canary = "sk-canary-borrow-7777777"
	cases := []struct {
		v        adapter.Vendor
		scenario string
		args     []string
	}{
		{adapter.Grok, `{"json":{"result":"ok"}}`, []string{"news", "what is new", "--vendor", "grok"}},
		{adapter.Claude, `{"json":{"result":"LGTM"}}`, []string{"review", "review the change", "--vendor", "claude", "--path", "a.go"}},
		{adapter.Codex, `{"events":[{"type":"item.completed","item":{"type":"agent_message","text":"LGTM"}}]}`, []string{"review", "review the change", "--vendor", "codex", "--path", "a.go"}},
	}
	for _, c := range cases {
		t.Run(string(c.v), func(t *testing.T) {
			f := newBorrowFixture(t, c.v)
			gitRepo(t, f.repo)
			f.env.gitPath = func() (string, error) { return probe.LookPath("git") }
			f.env.gitRun = execrun.ExecRunner(gitTimeout)
			chain := t.TempDir()
			t.Setenv("AGGY_FAKE_KEYCHAIN", chain)
			old := keyHelper
			t.Cleanup(func() { keyHelper = old })
			keyHelper = func() (keyring.Helper, error) {
				return keyring.Helper{Kind: keyring.Security, Path: f.fakes.Keychain}, nil
			}
			if err := os.WriteFile(filepath.Join(chain, "item."+keyring.Service+".anth@anthropic"), []byte(canary), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := store.OpenDefault(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.PutKey(context.Background(), store.Key{Name: "anth", Provider: "anthropic"}); err != nil {
				t.Fatal(err)
			}
			_ = s.Close()
			for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", "CODEX_API_KEY", "ANTH"} {
				t.Setenv(k, canary)
			}
			f.scenario[string(c.v)] = strings.Replace(c.scenario, "{", `{"canary":"`+canary+`",`, 1)
			code, out, errOut := f.run(t, c.args...)
			if code != 0 {
				t.Fatalf("borrow = %d\n%s\n%s", code, out, errOut)
			}
			recs := fakecli.Records(t, f.record)
			ran := 0
			for _, r := range recs {
				if r.Vendor != string(c.v) || r.Mode != "headless" {
					continue
				}
				ran++
				if len(r.CanaryIn) > 0 {
					t.Errorf("the key reached the %s child: %q", c.v, r.CanaryIn)
				}
			}
			if ran == 0 {
				t.Fatalf("no %s child ran: %+v", c.v, recs)
			}
			if _, err := os.Stat(filepath.Join(chain, "calls.jsonl")); err == nil {
				t.Error("a borrow ran the keychain tool")
			}
			if strings.Contains(out+errOut, canary) {
				t.Error("the borrow printed the key")
			}
		})
	}
}

// TestKeyTamperedIndex: the index is a file a model's shell can write. A row
// rebound to another provider names a keychain item that is not there, so the
// key never goes to that provider; a row bound to a provider aggy key never
// stores is not read at all.
func TestKeyTamperedIndex(t *testing.T) {
	chain, _ := keyWorld(t, keyring.Security)
	const canary = "sk-ant-canary-tamper-1234"
	if err := os.WriteFile(filepath.Join(chain, "item."+keyring.Service+".anth@anthropic"), []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if v, err := readKeychain(ctx, store.Key{Name: "anth", Provider: "anthropic"}); err != nil || string(v.Bytes()) != canary {
		t.Fatalf("the bound read = %v, %v", v, err)
	}
	if v, err := readKeychain(ctx, store.Key{Name: "anth", Provider: "openai"}); v != nil || err == nil || !strings.Contains(err.Error(), "not in") {
		t.Errorf("a rebound row read %v, %v", v, err)
	}
	reads := 0
	for _, p := range []string{"opencode", "claude-agent", "Bad", "x@y"} {
		in := openCodeInput{cfg: effective(config.Default(), config.RepoConfig{}), person: true,
			keys: []store.Key{{Name: "anth", Provider: p}},
			readKey: func(store.Key) (*keyring.Value, error) {
				reads++
				return nil, errors.New("read")
			}}
		got, warns := openCodeKeys(in)
		if len(got) != 0 || len(warns) != 1 || !strings.Contains(warns[0], "never stores") {
			t.Errorf("provider %q: got %v, warns %q", p, got, warns)
		}
	}
	if reads != 0 {
		t.Errorf("%d reads for rows aggy key never makes", reads)
	}
	// A new provider for a name is a new item: the old one goes first.
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutKey(ctx, store.Key{Name: "anth", Provider: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if code, _, errOut := runKeyCmd("add", "anth", "--provider", "openai"); code != 1 || !strings.Contains(errOut, "aggy key remove anth first") {
		t.Errorf("rebinding a name: %d %q", code, errOut)
	}
	// With no row, remove needs the provider the item is bound to.
	if code, _, errOut := runKeyCmd("remove", "ghost"); code != 1 || !strings.Contains(errOut, "--provider") {
		t.Errorf("remove with no row: %d %q", code, errOut)
	}
	if err := os.WriteFile(filepath.Join(chain, "item."+keyring.Service+".ghost@groq"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	offerIn = strings.NewReader("y\n")
	if code, out, errOut := runKeyCmd("remove", "ghost", "--provider", "groq"); code != 0 || !strings.Contains(out, "Deleted key ghost") {
		t.Errorf("remove --provider: %d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(chain, "item."+keyring.Service+".ghost@groq")); err == nil {
		t.Error("the orphan item is still there")
	}
}

// TestRepoKeysIgnored: a repo's [keys] table decodes, binds nothing, and the
// launch says it is ignored.
func TestRepoKeysIgnored(t *testing.T) {
	rc, err := config.ParseRepo("[keys]\nanthropic = \"sk-x\"\n[keys.binding]\nprovider = \"openai\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if !rc.SetsKeys() || !strings.Contains(repoKeysLine(rc), "ignores") {
		t.Errorf("SetsKeys %v, line %q", rc.SetsKeys(), repoKeysLine(rc))
	}
	if repoKeysLine(config.RepoConfig{}) != "" {
		t.Error("a line for a repo with no [keys]")
	}
}
