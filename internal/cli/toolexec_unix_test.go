//go:build unix

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/tooldef"
	"github.com/congdoe/aggy/internal/toolwire"
)

// TestToolExec: W71-2. aggy tool exec runs a server only from aggy's own
// launch folder, only the definition's argv with {repo} filled in, from
// a definition whose digest still matches, with an environment built
// from scratch: the basics plus the tool's declared variables.
func TestToolExec(t *testing.T) {
	_, dest := wireHome(t)
	writeAt(t, dest, strings.Replace(demoTool, `"--stdio"`, `"--root", "{repo}"`, 1), 0o600)
	state := os.Getenv(home.Env)
	run := filepath.Join(state, "run")
	dir := filepath.Join(run, "p.12345678.memory")
	for _, d := range []string{run, dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	writeAt(t, filepath.Join(bin, "demo"), "#!/bin/sh\n", 0o755)
	t.Setenv("PATH", bin+":relative/bin")
	t.Setenv("DEMO_HOME", "/opt/demo")
	t.Setenv("DEMO_OTHER", "no")
	t.Setenv("ANTHROPIC_API_KEY", "sk-canary")
	t.Setenv("npm_config_registry", "https://evil.example")
	t.Setenv("NODE_OPTIONS", "--require /tmp/x.js")
	r, _, err := tooldef.Load(state)
	if err != nil {
		t.Fatal(err)
	}
	demo, _ := r.Get("demo")
	repo := "/r/app"
	argv := []string{"demo", "serve", "--root", repo}
	ok := toolwire.ExecArgs(dir, "demo", demo.Digest, repo, argv)[2:]

	var got struct {
		prog, dir string
		args, env []string
	}
	old := execTool
	t.Cleanup(func() { execTool = old })
	execTool = func(prog string, args []string, dir string, env []string) error {
		got.prog, got.args, got.dir, got.env = prog, args, dir, env
		return nil
	}
	exec := func(args []string) (int, string) {
		got.prog = ""
		var errb bytes.Buffer
		return runToolExec(args, &errb), errb.String()
	}
	if code, errOut := exec(ok); code != 0 || got.prog != filepath.Join(bin, "demo") || got.dir != dir ||
		!slices.Equal(got.args, argv[1:]) {
		t.Fatalf("exit %d %q: %+v", code, errOut, got)
	}
	basics := []string{"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE"}
	for _, kv := range got.env {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(basics, k) && kv != "DEMO_HOME=/opt/demo" && kv != "DO_NOT_TRACK=1" {
			t.Errorf("env has %q", kv)
		}
	}
	for _, want := range []string{"DEMO_HOME=/opt/demo", "HOME=" + os.Getenv("HOME"), "PATH=" + bin, "DO_NOT_TRACK=1"} {
		if !slices.Contains(got.env, want) {
			t.Errorf("env %q lacks %q", got.env, want)
		}
	}
	// T6822: the user config's [tools] allow_telemetry lets telemetry
	// through: aggy sets no DO_NOT_TRACK, and the person's own passes
	// (L1 of #97).
	cfg := filepath.Join(state, "config.toml")
	writeAt(t, cfg, "[tools]\nallow_telemetry = true\n", 0o600)
	t.Setenv("DO_NOT_TRACK", "")
	if code, errOut := exec(ok); code != 0 || slices.ContainsFunc(got.env, func(kv string) bool { return strings.HasPrefix(kv, "DO_NOT_TRACK=1") }) {
		t.Errorf("allowed: exit %d %q, env %q", code, errOut, got.env)
	}
	t.Setenv("DO_NOT_TRACK", "1")
	if code, errOut := exec(ok); code != 0 || !slices.Contains(got.env, "DO_NOT_TRACK=1") {
		t.Errorf("allowed, the person's own: exit %d %q, env %q", code, errOut, got.env)
	}
	t.Setenv("DO_NOT_TRACK", "0")
	// A config that cannot be read sets it all the same.
	writeAt(t, cfg, "[tools]\nallow_telemetry = true\nbogus = 1\n", 0o600)
	if code, errOut := exec(ok); code != 0 || !slices.Contains(got.env, "DO_NOT_TRACK=1") {
		t.Errorf("unreadable config: exit %d %q, env %q", code, errOut, got.env)
	}
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}

	with := func(i int, v string) []string {
		a := slices.Clone(ok)
		a[i] = v
		return a
	}
	link := filepath.Join(run, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(run, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	for _, c := range []struct {
		name  string
		args  []string
		check string
	}{
		{"usage", ok[:5], "usage"},
		{"no --", with(4, "-x"), "usage"},
		{"a folder outside run", with(0, elsewhere), "dir"},
		{"a relative folder", with(0, "run/p"), "dir"},
		{"a symlinked folder", with(0, link), "dir"},
		{"an open folder", with(0, open), "dir"},
		{"an unknown tool", with(1, "nope"), "definition"},
		{"a changed definition", with(2, strings.Repeat("0", toolwire.DigestLen)), "digest"},
		{"a short digest", with(2, demo.Digest[:8]), "digest"},
		{"another repo", with(3, "/r/other"), "argv"},
		{"no repo", with(3, toolwire.NoRepo), "argv"},
		{"another program", with(5, "/tmp/demo"), "argv"},
		{"an extra argument", append(slices.Clone(ok), "--evil"), "argv"},
	} {
		code, errOut := exec(c.args)
		if code != 1 || got.prog != "" || !strings.Contains(errOut, "tool.exec_refused") {
			t.Errorf("%s: exit %d %q, ran %q", c.name, code, errOut, got.prog)
		}
	}
	// The run folder itself must be private too.
	if err := os.Chmod(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _ := exec(ok); code != 1 || got.prog != "" {
		t.Error("an open run folder passed")
	}
}
