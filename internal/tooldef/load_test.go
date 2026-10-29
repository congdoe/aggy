package tooldef

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/home"
)

// TestBuiltinsValid fails when any built-in definition fails the
// validator: the built-ins use the format they define.
func TestBuiltinsValid(t *testing.T) {
	r, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range r.List() {
		names = append(names, tl.Name)
		if !tl.BuiltIn || tl.Source != "built-in:"+tl.Name+".toml" {
			t.Errorf("%s: built-in %v source %q", tl.Name, tl.BuiltIn, tl.Source)
		}
		// No vendor fact about these tools is in docs/verification.md
		// yet, and none is pinned.
		if tl.Version == "unpinned" && !slices.Contains(tl.Unverified, "version") {
			t.Errorf("%s: an unpinned version must be marked unverified", tl.Name)
		}
		if tl.Default {
			t.Errorf("%s: every built-in is off by default (M5)", tl.Name)
		}
	}
	want := []string{"caveman", "codegraph", "context7", "headroom", "playwright", "rtk"}
	if !slices.Equal(names, want) {
		t.Errorf("built-ins = %v, want %v", names, want)
	}
	// aggy never installs: each built-in tells the person how to install
	// its pinned version by hand (codegraph's comes with T6711).
	for _, name := range []string{"caveman", "context7", "headroom", "playwright", "rtk"} {
		tl, _ := r.Get(name)
		if !strings.Contains(tl.Install.Hint, tl.Version) {
			t.Errorf("%s: install.hint %q does not name the pinned version %s", name, tl.Install.Hint, tl.Version)
		}
	}
	// T6716: playwright is per repo and off by default; has-ui is never
	// checked (the person turns it on per repo).
	if pw, _ := r.Get("playwright"); pw.Scope != "repo" || pw.Default {
		t.Errorf("playwright scope %q default %v, want repo and off", pw.Scope, pw.Default)
	}
	// T6712: RTK is wired into Claude only (its Codex hook approves, and
	// Grok has no subcommand), as a rewriting PreToolUse hook.
	rtk, _ := r.Get("rtk")
	if len(rtk.Hooks) != 1 || !slices.Equal(rtk.Hooks[0].Vendors, []adapter.Vendor{adapter.Claude}) ||
		rtk.Hooks[0].Event != "PreToolUse" || !rtk.Hooks[0].Rewrites || !slices.Equal(rtk.Hooks[0].Argv, []string{"rtk", "hook", "claude"}) {
		t.Errorf("rtk hooks = %+v", rtk.Hooks)
	}
	// T6713: Headroom is its MCP server only, never wrap or proxy.
	if hr, _ := r.Get("headroom"); len(hr.MCP) != 1 || !slices.Equal(hr.MCP[0].Argv, []string{"headroom", "mcp", "serve"}) || len(hr.Hooks)+len(hr.Init) > 0 || !hr.Network {
		t.Errorf("headroom = %+v", hr.Def)
	}
	// W71-6: the server runs in aggy's launch folder, so it is told the
	// repo; codegraph's own form is serve --mcp --path <dir>.
	cg, _ := r.Get("codegraph")
	if len(cg.MCP) != 1 || !slices.Equal(cg.MCP[0].Argv, []string{"codegraph", "serve", "--mcp", "--path", "{repo}"}) {
		t.Errorf("codegraph mcp = %+v", cg.MCP)
	}
}

func writeDef(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // past the umask
		t.Fatal(err)
	}
}

func userNames(r *Registry) []string {
	var out []string
	for _, tl := range r.List() {
		if !tl.BuiltIn {
			out = append(out, tl.Name)
		}
	}
	return out
}

func TestLoadUser(t *testing.T) {
	state := t.TempDir()
	dir := Dir(state)
	writeDef(t, filepath.Join(dir, "demo.toml"), base, 0o600)
	writeDef(t, filepath.Join(dir, "README.md"), "not a definition", 0o600)
	r, refused, err := Load(state)
	if err != nil || len(refused) != 0 {
		t.Fatalf("Load: %v %v", err, refused)
	}
	tl, ok := r.Get("demo")
	if !ok || tl.BuiltIn || tl.Source != filepath.Join(dir, "demo.toml") {
		t.Fatalf("demo = %+v, %v", tl, ok)
	}
	if _, ok := r.Get("codegraph"); !ok {
		t.Error("the built-ins are missing")
	}
}

func TestLoadNoDir(t *testing.T) {
	r, refused, err := Load(t.TempDir())
	if err != nil || len(refused) != 0 || len(r.List()) != 6 {
		t.Fatalf("Load: %d tools, %v, %v", len(r.List()), refused, err)
	}
}

// TestLoadRefuses: each refused file is left out and named, and the
// other user definitions still load.
func TestLoadRefuses(t *testing.T) {
	other := strings.NewReplacer(`name = "demo"`, `name = "other"`, "DEMO_HOME", "OTHER_HOME").Replace(base)
	for _, c := range []struct {
		name      string
		setup     func(t *testing.T, dir string)
		untrusted bool
		want      string
	}{
		{"symlinked file", func(t *testing.T, dir string) {
			real := filepath.Join(t.TempDir(), "demo.toml")
			writeDef(t, real, base, 0o600)
			if err := os.Symlink(real, filepath.Join(dir, "demo.toml")); err != nil {
				t.Fatal(err)
			}
		}, true, "symlink"},
		{"group-writable file", func(t *testing.T, dir string) {
			writeDef(t, filepath.Join(dir, "demo.toml"), base, 0o620)
		}, true, "writable"},
		{"world-writable file", func(t *testing.T, dir string) {
			writeDef(t, filepath.Join(dir, "demo.toml"), base, 0o602)
		}, true, "writable"},
		{"directory named .toml", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "demo.toml"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, true, "regular"},
		{"invalid", func(t *testing.T, dir string) {
			writeDef(t, filepath.Join(dir, "demo.toml"), strings.Replace(base, `"demo", "serve", "{repo}"`, `"sh", "-c", "demo"`, 1), 0o600)
		}, false, "mcp[0].argv"},
		{"name and file differ", func(t *testing.T, dir string) {
			writeDef(t, filepath.Join(dir, "demo.toml"), strings.NewReplacer(`name = "demo"`, `name = "dmeo"`, "DEMO_HOME", "DMEO_HOME").Replace(base), 0o600)
		}, false, "dmeo.toml"},
		{"shadows a built-in", func(t *testing.T, dir string) {
			writeDef(t, filepath.Join(dir, "codegraph.toml"), strings.NewReplacer(`name = "demo"`, `name = "codegraph"`, "DEMO_HOME", "CODEGRAPH_HOME").Replace(base), 0o600)
		}, false, "shadowed"},
		{"user tool default on", func(t *testing.T, dir string) {
			writeDef(t, filepath.Join(dir, "demo.toml"), "default = true\n"+base, 0o600)
		}, false, "default"},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := t.TempDir()
			dir := Dir(state)
			writeDef(t, filepath.Join(dir, "other.toml"), other, 0o600)
			c.setup(t, dir)
			r, refused, err := Load(state)
			if err != nil {
				t.Fatal(err)
			}
			if len(refused) != 1 || !strings.Contains(refused[0].Error(), c.want) || errors.Is(refused[0], ErrUntrusted) != c.untrusted {
				t.Fatalf("refused = %v, want one naming %q (untrusted %v)", refused, c.want, c.untrusted)
			}
			if got := userNames(r); !slices.Equal(got, []string{"other"}) {
				t.Errorf("user tools = %v, want [other]", got)
			}
			if tl, _ := r.Get("codegraph"); !tl.BuiltIn {
				t.Error("a user file replaced a built-in")
			}
		})
	}
}

// TestLoadRefusesDir: a tools folder that is a symlink, or that group or
// others can write, loads no user tool at all.
func TestLoadRefusesDir(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, state string)
	}{
		{"symlinked folder", func(t *testing.T, state string) {
			real := filepath.Join(t.TempDir(), "tools")
			writeDef(t, filepath.Join(real, "demo.toml"), base, 0o600)
			if err := os.Symlink(real, Dir(state)); err != nil {
				t.Fatal(err)
			}
		}},
		{"world-writable folder", func(t *testing.T, state string) {
			writeDef(t, filepath.Join(Dir(state), "demo.toml"), base, 0o600)
			if err := os.Chmod(Dir(state), 0o777); err != nil {
				t.Fatal(err)
			}
		}},
		{"group-writable folder", func(t *testing.T, state string) {
			writeDef(t, filepath.Join(Dir(state), "demo.toml"), base, 0o600)
			if err := os.Chmod(Dir(state), 0o770); err != nil {
				t.Fatal(err)
			}
		}},
		// T59-15: whoever can write the state dir can swap the folder.
		{"world-writable state dir", func(t *testing.T, state string) {
			writeDef(t, filepath.Join(Dir(state), "demo.toml"), base, 0o600)
			if err := os.Chmod(state, 0o777); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := t.TempDir()
			c.setup(t, state)
			r, refused, err := Load(state)
			if err != nil {
				t.Fatal(err)
			}
			if len(refused) != 1 || !errors.Is(refused[0], ErrUntrusted) {
				t.Fatalf("refused = %v, want the folder refused as untrusted", refused)
			}
			if got := userNames(r); len(got) != 0 {
				t.Errorf("user tools = %v, want none", got)
			}
		})
	}
}

// TestLoadNeverReadsRepo: a definition in a repo, even one in the
// working directory laid out like the state directory, is never loaded.
func TestLoadNeverReadsRepo(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".aggy/tools/demo.toml", "tools/demo.toml", "demo.toml"} {
		writeDef(t, filepath.Join(repo, p), base, 0o600)
	}
	t.Chdir(repo)
	t.Setenv(home.Env, t.TempDir())
	r, refused, err := LoadDefault()
	if err != nil || len(refused) != 0 {
		t.Fatalf("LoadDefault: %v %v", err, refused)
	}
	if _, ok := r.Get("demo"); ok {
		t.Fatal("a definition was loaded from the repo")
	}
}

// TestBuiltinsPinnedForRelease runs only for a release (AGGY_RELEASE=1).
func TestBuiltinsPinnedForRelease(t *testing.T) { requirePinnedForRelease(t) }

// requirePinnedForRelease fails on any built-in whose version is not
// pinned (a version starting with a digit). Built-ins may stay unpinned
// until their tool's task pins them, but never in a release.
func requirePinnedForRelease(t *testing.T) {
	t.Helper()
	if os.Getenv("AGGY_RELEASE") != "1" {
		t.Skip("AGGY_RELEASE=1 checks that every built-in is pinned")
	}
	r, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range r.List() {
		if !semverRE.MatchString(tl.Version) {
			t.Errorf("%s: version %q is not pinned", tl.Name, tl.Version)
		}
	}
}
