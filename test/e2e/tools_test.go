//go:build unix

package e2e

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/fakecli"
)

// aggyIn runs aggy (not on a terminal) in the world's home and dir.
func (w *world) aggyIn(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command(buildAggy(t), args...)
	cmd.Dir = w.cwd
	cmd.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("aggy %q: %v\n%s", args, err, out)
	}
}

// launchRecord runs aggy run <vendor> to its prompt, exits, and returns
// the CLI's record (the last interactive one).
func (w *world) launchRecord(t *testing.T, vendor string) fakecli.Record {
	t.Helper()
	_ = os.Remove(w.record)
	s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "run", vendor)
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	s.exit()
	return interactiveRecord(t, w)
}

// traces lists the files under the world's home, outside aggy's state
// dir and the fakes' record, that name the tool's server: a vendor config
// aggy wrote it into.
func (w *world) traces(t *testing.T, server string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(w.home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (p == filepath.Join(w.home, ".aggy") || p == w.bin) {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() && p != w.record {
			if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), server) {
				found = append(found, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestToolForOneSession: aggy run --tool wires an off tool for that
// session only.
func TestToolForOneSession(t *testing.T) {
	w := newWorld(t)
	s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "run", "--tool", "context7", "claude")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	s.exit()
	if f := interactiveRecord(t, w).Flags["--mcp-config"]; len(f) != 1 {
		t.Errorf("--tool: --mcp-config %q", f)
	}
	if r := w.launchRecord(t, "claude"); slices.Contains(r.Args, "--mcp-config") {
		t.Errorf("--tool outlived its session: %q", r.Args)
	}
}

// TestToolsWiredAtLaunch (T6708): a tool turned on reaches the CLI aggy
// starts on its command line only, for that session; turned off, it
// leaves no flag, no file, and nothing in any vendor config.
func TestToolsWiredAtLaunch(t *testing.T) {
	for _, v := range []string{"claude", "codex", "grok"} {
		t.Run(v, func(t *testing.T) {
			w := newWorld(t)
			w.aggyIn(t, "tool", "on", "context7", "--vendor", v)
			r := w.launchRecord(t, v)
			switch v {
			case "claude":
				f := r.Flags["--mcp-config"]
				if len(f) != 1 || !strings.HasPrefix(f[0], filepath.Join(w.home, ".aggy", "run")+"/") || slices.Contains(r.Args, "--strict-mcp-config") {
					t.Errorf("args %q", r.Args)
				} else if _, err := os.Stat(f[0]); !os.IsNotExist(err) {
					t.Errorf("%s left after the session: %v", f[0], err)
				}
			case "codex":
				if !slices.Contains(r.Flags["-c"], `mcp_servers.context7.command="context7-mcp"`) {
					t.Errorf("-c %q", r.Flags["-c"])
				}
			case "grok":
				if f := r.Flags["--agent"]; len(f) != 1 || !strings.HasSuffix(f[0], "/agent.md") {
					t.Errorf("args %q", r.Args)
				}
			}
			if tr := w.traces(t, "context7-mcp"); len(tr) != 0 {
				t.Errorf("vendor config written: %q", tr)
			}

			w.aggyIn(t, "tool", "off", "context7")
			r = w.launchRecord(t, v)
			for _, a := range r.Args {
				if strings.Contains(a, "context7") || a == "--mcp-config" || a == "--agent" {
					t.Errorf("an off tool reached %s: %q", v, r.Args)
				}
			}
			if tr := w.traces(t, "context7"); len(tr) != 0 {
				t.Errorf("an off tool left a trace: %q", tr)
			}
		})
	}
}
