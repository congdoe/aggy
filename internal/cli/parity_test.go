package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/probe"
)

func TestAddMemory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".aggy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parity.MemoryPath(root), []byte("note"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(`developer_instructions = "mine"`), 0o600); err != nil {
		t.Fatal(err)
	}
	framed := parity.FrameMemory("note")

	t.Run("claude file", func(t *testing.T) {
		var req adapter.InteractiveRequest
		cleanup, warns := addMemory(adapter.Claude, nil, root, &req)
		if len(warns) > 0 || req.MemoryFile == "" || req.Memory != "" {
			t.Fatalf("req %+v, warns %v", req, warns)
		}
		b, err := os.ReadFile(req.MemoryFile)
		if err != nil || string(b) != framed {
			t.Errorf("memory file = %q, %v", b, err)
		}
		if fi, _ := os.Stat(req.MemoryFile); fi.Mode().Perm() != 0o600 {
			t.Errorf("memory file mode %v", fi.Mode())
		}
		cleanup()
		if _, err := os.Stat(filepath.Dir(req.MemoryFile)); !os.IsNotExist(err) {
			t.Errorf("cleanup left %s: %v", req.MemoryFile, err)
		}
	})
	t.Run("codex keeps the person's own instructions", func(t *testing.T) {
		var req adapter.InteractiveRequest
		_, warns := addMemory(adapter.Codex, []string{probe.CapConfigOverride}, root, &req)
		if len(warns) > 0 || req.Memory != "mine\n\n"+framed {
			t.Errorf("req %+v, warns %v", req, warns)
		}
	})
	for _, tc := range []struct {
		v    adapter.Vendor
		caps []string
	}{{adapter.Codex, nil}, {adapter.Grok, []string{probe.CapTools}}} {
		t.Run(string(tc.v)+" without the flag", func(t *testing.T) {
			var req adapter.InteractiveRequest
			_, warns := addMemory(tc.v, tc.caps, root, &req)
			if req.Memory != "" || len(warns) != 1 || !strings.Contains(warns[0], "not delivered") {
				t.Errorf("req %+v, warns %v", req, warns)
			}
		})
	}
	t.Run("grok", func(t *testing.T) {
		var req adapter.InteractiveRequest
		_, warns := addMemory(adapter.Grok, []string{probe.CapRules}, root, &req)
		if len(warns) > 0 || req.Memory != framed {
			t.Errorf("req %+v, warns %v", req, warns)
		}
	})
	t.Run("over the cap", func(t *testing.T) {
		big := t.TempDir()
		if err := os.MkdirAll(filepath.Join(big, ".aggy"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parity.MemoryPath(big), []byte(strings.Repeat("x", parity.MemoryCap+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		var req adapter.InteractiveRequest
		_, warns := addMemory(adapter.Grok, []string{probe.CapRules}, big, &req)
		if req.Memory != "" || len(warns) != 1 {
			t.Errorf("req %+v, warns %v", req, warns)
		}
	})
}
