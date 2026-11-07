package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
)

// openCodeWorld is a temp HOME and state folder with a git repo in it.
func openCodeWorld(t *testing.T) (repo, dir string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", base)
	t.Setenv("AGGY_HOME", filepath.Join(base, ".aggy"))
	t.Setenv("XDG_DATA_HOME", "")
	oldBin, oldPath, oldCheck := hookBin, pathAggy, checkTrusted
	hookBin = func() (string, error) { return filepath.Join(base, "aggy"), nil }
	pathAggy = func() (string, error) { return "", os.ErrNotExist }
	checkTrusted = func(string) error { return nil }
	t.Cleanup(func() { hookBin, pathAggy, checkTrusted = oldBin, oldPath, oldCheck })
	repo = filepath.Join(base, "repo")
	for _, d := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, "sub"), filepath.Join(base, ".aggy")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "aggy"), []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	return repo, filepath.Join(base, "launch")
}

// trustRepo records repo as trusted, as aggy trust does.
func trustRepo(t *testing.T, repo string) {
	t.Helper()
	path, err := openCodeTrustFile()
	if err != nil {
		t.Fatal(err)
	}
	key, err := safefs.CanonicalPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	p, already, err := trustPatch(path, key)
	if err != nil || already {
		t.Fatalf("patch: %v, already %v", err, already)
	}
	b, err := p.Transform(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

var openCodeCaps = []string{probe.CapStandalone, probe.CapSession, probe.CapMiniSession, probe.CapAuto}

func readLaunchConfig(t *testing.T, path string) (doc struct {
	Model        string   `json:"model"`
	Instructions []string `json:"instructions"`
	Permissions  []struct{ Action, Resource, Effect string }
	Policies     []struct{ Action, Resource, Effect string }
	Plugins      []struct {
		Package string            `json:"package"`
		Options map[string]string `json:"options"`
	}
}) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func effective(user config.Config, repo config.RepoConfig) config.Effective {
	return config.Merge(user, repo)
}

// TestOpenCodeScreenSplit: trust decides the screen, and only trust gives
// reads and full auto.
func TestOpenCodeScreenSplit(t *testing.T) {
	cases := []struct {
		name               string
		trusted, auto      bool
		wantMini, wantAuto bool
		wantReadAllow      bool
		warn               string
	}{
		{"untrusted", false, false, true, false, false, ""},
		{"untrusted asking for auto", false, true, true, false, false, "only in a repo trusted"},
		{"trusted", true, false, false, false, true, ""},
		{"trusted with auto", true, true, false, true, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, dir := openCodeWorld(t)
			if c.trusted {
				trustRepo(t, repo)
			}
			user := config.Default()
			user.OpenCode.Auto = c.auto
			// A subfolder is the same repo: its launch still runs the root's plugins.
			s, err := prepareOpenCode(openCodeInput{cfg: effective(user, config.RepoConfig{}), caps: openCodeCaps, cwd: filepath.Join(repo, "sub"), dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			if s.launch.Mini != c.wantMini || s.launch.Auto != c.wantAuto {
				t.Fatalf("mini %v, auto %v", s.launch.Mini, s.launch.Auto)
			}
			doc := readLaunchConfig(t, s.launch.ConfigFile)
			reads := slices.ContainsFunc(doc.Permissions, func(r struct{ Action, Resource, Effect string }) bool {
				return r.Action == "read" && r.Effect == "allow"
			})
			if reads != c.wantReadAllow {
				t.Fatalf("read allowed = %v", reads)
			}
			if c.warn != "" && !strings.Contains(strings.Join(s.warns, " "), c.warn) {
				t.Fatalf("warns = %q", s.warns)
			}
			if len(doc.Plugins) != 1 || doc.Plugins[0].Options["root"] != repo {
				t.Fatalf("plugins = %+v: the guard's root is the repo root", doc.Plugins)
			}
		})
	}
}

// TestOpenCodeLaunchFolder: what the launch leaves in its folder, and that
// the config folder is empty, so nothing planted carries over.
func TestOpenCodeLaunchFolder(t *testing.T) {
	repo, dir := openCodeWorld(t)
	s, err := prepareOpenCode(openCodeInput{cfg: effective(config.Default(), config.RepoConfig{}), caps: openCodeCaps, cwd: repo, dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.launch.ConfigFile, dir) || !strings.HasPrefix(s.launch.ConfigDir, dir) {
		t.Fatalf("launch = %+v, want both under %s", s.launch, dir)
	}
	if es, err := os.ReadDir(s.launch.ConfigDir); err != nil || len(es) != 0 {
		t.Fatalf("config folder: %v, %v", es, err)
	}
	for _, n := range []string{"package.json", "server.js"} {
		fi, err := os.Stat(filepath.Join(dir, "opencode", "plugin", n))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("plugin %s: %v, %v", n, fi, err)
		}
	}
	if fi, err := os.Stat(s.launch.ConfigFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config: %v, %v", fi, err)
	}
}

// TestOpenCodeRefusals: a launch aggy's rules cannot reach never starts.
func TestOpenCodeRefusals(t *testing.T) {
	cases := []struct {
		name    string
		caps    []string
		repo    config.RepoConfig
		model   string
		wantErr string
	}{
		{"no --standalone", []string{probe.CapMiniSession}, config.RepoConfig{}, "", "--standalone"},
		{"no probe", nil, config.RepoConfig{}, "", "--standalone"},
		{"no provider left", openCodeCaps, config.RepoConfig{AllowedVendors: []adapter.Vendor{"opencode"}}, "", "no OpenCode provider"},
		{"a model on a denied provider", openCodeCaps, config.RepoConfig{}, "anthropic/claude", "not on a provider allowed"},
		{"a model with no provider", openCodeCaps, config.RepoConfig{}, "grok-build-0.1", "not on a provider allowed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, dir := openCodeWorld(t)
			_, err := prepareOpenCode(openCodeInput{cfg: effective(config.Default(), c.repo), caps: c.caps, cwd: repo, dir: dir, model: c.model})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
		})
	}
	t.Run("no guard binary", func(t *testing.T) {
		repo, dir := openCodeWorld(t)
		hookBin = func() (string, error) { return "", os.ErrNotExist }
		if _, err := prepareOpenCode(openCodeInput{cfg: effective(config.Default(), config.RepoConfig{}), caps: openCodeCaps, cwd: repo, dir: dir}); err == nil {
			t.Fatal("started with no guard")
		}
	})
}

// TestOpenCodeInstructions: copies, refusing a symlink, in a known order.
func TestOpenCodeInstructions(t *testing.T) {
	repo, dir := openCodeWorld(t)
	base := filepath.Dir(repo)
	canary := filepath.Join(base, "canary")
	if err := os.WriteFile(canary, []byte("CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("agents text\x1b[31m"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canary, filepath.Join(repo, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	s, err := prepareOpenCode(openCodeInput{cfg: effective(config.Default(), config.RepoConfig{}), caps: openCodeCaps, cwd: repo, dir: dir,
		memory: "framed memory", brief: "brief text"})
	if err != nil {
		t.Fatal(err)
	}
	doc := readLaunchConfig(t, s.launch.ConfigFile)
	var texts []string
	for _, p := range doc.Instructions {
		if !strings.HasPrefix(p, dir) {
			t.Fatalf("%s is not a copy in the launch's folder", p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, string(b))
	}
	all := strings.Join(texts, "\n")
	if strings.Contains(all, "CANARY") {
		t.Fatal("a symlinked CLAUDE.md was followed")
	}
	if !strings.Contains(strings.Join(s.warns, " "), "CLAUDE.md not passed") {
		t.Fatalf("no warning for the symlink: %q", s.warns)
	}
	if len(texts) != 3 || texts[0] != "agents text" || texts[1] != "framed memory" || !strings.Contains(texts[2], "brief text") {
		t.Fatalf("texts = %q", texts)
	}
}

func TestOpenCodeInstructionTooBig(t *testing.T) {
	repo, dir := openCodeWorld(t)
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), bytes.Repeat([]byte("x"), maxInstruction+1), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := prepareOpenCode(openCodeInput{cfg: effective(config.Default(), config.RepoConfig{}), caps: openCodeCaps, cwd: repo, dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(s.warns, " "), "CLAUDE.md not passed") {
		t.Fatalf("warns = %q", s.warns)
	}
}

func TestOpenCodeTrusted(t *testing.T) {
	repo, _ := openCodeWorld(t)
	if ok, w := openCodeTrusted(repo); ok || w != "" {
		t.Fatalf("no file: %v, %q", ok, w)
	}
	trustRepo(t, repo)
	if ok, _ := openCodeTrusted(repo); !ok {
		t.Fatal("not trusted after trust")
	}
	if ok, _ := openCodeTrusted(filepath.Dir(repo)); ok {
		t.Fatal("the parent counted as trusted")
	}
	path, _ := openCodeTrustFile()
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, w := openCodeTrusted(repo); ok || w == "" {
		t.Fatalf("a bad file must trust nothing and warn: %v, %q", ok, w)
	}
}

// TestTrustPatch: the change goes through internal/patch, keeps the others,
// and undoes byte for byte.
func TestTrustPatch(t *testing.T) {
	repo, _ := openCodeWorld(t)
	path, _ := openCodeTrustFile()
	p, already, err := trustPatch(path, repo)
	if err != nil || already || p.ExpectedHash != patch.NoFile {
		t.Fatalf("first: %+v, %v, %v", p, already, err)
	}
	b, err := p.Transform(nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.Transform(b)
	if err != nil || !bytes.Equal(again, b) {
		t.Fatalf("a repeat apply changed the file: %q, %v", again, err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, already, err := trustPatch(path, repo); err != nil || !already {
		t.Fatalf("second: %v, %v", already, err)
	}
	p2, _, err := trustPatch(path, "/other")
	if err != nil || p2.ExpectedHash != patch.Hash(b) {
		t.Fatalf("other: %+v, %v", p2, err)
	}
	b2, _ := p2.Transform(b)
	var doc openCodeTrust
	if err := json.Unmarshal(b2, &doc); err != nil || !slices.Contains(doc.Repos, repo) || !slices.Contains(doc.Repos, "/other") {
		t.Fatalf("%s, %v", b2, err)
	}
	if err := os.WriteFile(path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := trustPatch(path, repo); err == nil {
		t.Fatal("a bad file was overwritten")
	}
}

func TestTrustUsage(t *testing.T) {
	repo, _ := openCodeWorld(t)
	var out, errb bytes.Buffer
	for _, args := range [][]string{{}, {repo}, {"--opencode"}, {"--opencode", repo, "extra"}, {"--bogus", repo}} {
		if code := runTrust(args, &out, &errb); code != 2 {
			t.Errorf("%q: code %d", args, code)
		}
	}
	if code := runTrust([]string{"--opencode", filepath.Join(repo, "missing")}, &out, &errb); code != 1 {
		t.Errorf("missing: code %d", code)
	}
	if !strings.Contains(errb.String(), "[cli.usage · run ") || !strings.Contains(errb.String(), "[trust.bad_dir · run ") {
		t.Errorf("refusals must end with their code and run: %q", errb.String())
	}
	// go test's stdin is no terminal: nothing is asked, nothing changes, so a
	// model's tool shell cannot trust its own repo.
	errb.Reset()
	if code := runTrust([]string{"--opencode", repo}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "terminal") {
		t.Errorf("no terminal: code %d, %q", code, errb.String())
	}
	path, _ := openCodeTrustFile()
	if _, err := os.Stat(path); err == nil {
		t.Error("the trust file was written without a terminal")
	}
	// The flag after the folder, too.
	out.Reset()
	if code := runTrust([]string{repo, "--opencode", "--dry-run"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "opencode-trust.json") {
		t.Errorf("dry run: code %d, %q", code, out.String())
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a dry run wrote the file")
	}
}

// TestOpenCodeManagedFindings: doctor warns about config it cannot remove,
// by name, and reads nothing of it.
func TestOpenCodeManagedFindings(t *testing.T) {
	openCodeWorld(t)
	dir := t.TempDir()
	present := filepath.Join(dir, "opencode")
	if err := os.Mkdir(present, 0o700); err != nil {
		t.Fatal(err)
	}
	old := openCodeManagedPaths
	t.Cleanup(func() { openCodeManagedPaths = old })
	t.Setenv("USER", "")
	openCodeManagedPaths = []string{filepath.Join(dir, "absent")}
	if fs := openCodeManagedFindings(); len(fs) != 0 {
		t.Fatalf("absent: %+v", fs)
	}
	openCodeManagedPaths = []string{present}
	fs := openCodeManagedFindings()
	if len(fs) != 1 || fs[0].Path != present || fs[0].Code != codeOpenCodeManaged {
		t.Fatalf("present: %+v", fs)
	}
}
