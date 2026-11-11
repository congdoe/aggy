//go:build unix

package execrun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/keyring"
)

// TestKeychainEnv: the keychain tool gets who and where the person is, and
// nothing else of aggy's: no API key, base URL, vendor home, or aggy state.
func TestKeychainEnv(t *testing.T) {
	base := []string{
		"HOME=/h", "USER=u", "PATH=/usr/bin:.:rel:/bin", "LANG=C", "DBUS_SESSION_BUS_ADDRESS=unix:path=/b",
		"ANTHROPIC_API_KEY=sk-canary", "OPENAI_API_KEY=x", "XAI_API_KEY=x", "ANTHROPIC_BASE_URL=http://x",
		"CLAUDE_CONFIG_DIR=/c", "CODEX_HOME=/c", "AGGY_HOME=/a", "AGGY_SOCKET=/s", "AGGY_VENDOR=claude",
		"OPENCODE_SERVER_PASSWORD=p", "NODE_OPTIONS=--require x", "AGGY_FAKE_KEYCHAIN=/k", "AGGY_FAKE_x=1",
	}
	got := KeychainEnv(base)
	want := []string{"HOME=/h", "USER=u", "PATH=/usr/bin:/bin", "LANG=C", "DBUS_SESSION_BUS_ADDRESS=unix:path=/b", "AGGY_FAKE_KEYCHAIN=/k"}
	if !slices.Equal(got, want) {
		t.Errorf("KeychainEnv = %q, want %q", got, want)
	}
}

// keychainWorld is a temp HOME and a fake keychain folder outside it.
func keychainWorld(t *testing.T) (bin, dir string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir = t.TempDir()
	t.Setenv("AGGY_FAKE_KEYCHAIN", dir)
	return fakecli.Build(t).Keychain, dir
}

func TestKeychainReadAndRun(t *testing.T) {
	bin, dir := keychainWorld(t)
	ctx := context.Background()
	for _, h := range []keyring.Helper{{Kind: keyring.Security, Path: bin}, {Kind: keyring.SecretTool, Path: bin}} {
		t.Run(h.Name(), func(t *testing.T) {
			const val = "sk-canary-read-0123"
			if err := os.WriteFile(filepath.Join(dir, "item."+keyring.Service+".k"), []byte(val), 0o600); err != nil {
				t.Fatal(err)
			}
			v, code, err := KeychainRead(ctx, h.Path, h.ReadArgs("k"), 10*time.Second)
			if err != nil || code != 0 || string(v.Bytes()) != val {
				t.Fatalf("read = %v, %d, %v", v, code, err)
			}
			v.Wipe()
			if args, ok := h.PresenceArgs("k"); ok {
				if code, err := KeychainRun(ctx, h.Path, args, 10*time.Second); err != nil || code != 0 {
					t.Errorf("presence = %d, %v", code, err)
				}
			}
			if code, err := KeychainRun(ctx, h.Path, h.RemoveArgs("k"), 10*time.Second); err != nil || code != 0 {
				t.Errorf("remove = %d, %v", code, err)
			}
			v, code, err = KeychainRead(ctx, h.Path, h.ReadArgs("k"), 10*time.Second)
			if v != nil || err != nil || code == 0 {
				t.Errorf("read of a removed key = %v, %d, %v", v, code, err)
			}
			if h.Kind == keyring.Security && !h.NotFound(code) {
				t.Errorf("security exit %d is not read as not found", code)
			}
		})
	}
}

// TestKeychainReadRefuses: a value too long, or one that is not a printable
// key, gives no Value and an error that does not quote it.
func TestKeychainReadRefuses(t *testing.T) {
	bin, dir := keychainWorld(t)
	h := keyring.Helper{Kind: keyring.SecretTool, Path: bin}
	for name, val := range map[string]string{"long": strings.Repeat("A", keyring.MaxValue+10), "space": "sk-has space"} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "item."+keyring.Service+".k"), []byte(val), 0o600); err != nil {
				t.Fatal(err)
			}
			v, _, err := KeychainRead(context.Background(), h.Path, h.ReadArgs("k"), 10*time.Second)
			if v != nil || err == nil || strings.Contains(err.Error(), val[:6]) {
				t.Errorf("read = %v, %v", v, err)
			}
		})
	}
}

// TestKeychainPrompt: the tool reads the value from the terminal it is given
// (a pipe stands in here), with the arguments holding none.
func TestKeychainPrompt(t *testing.T) {
	bin, dir := keychainWorld(t)
	for _, h := range []keyring.Helper{{Kind: keyring.Security, Path: bin}, {Kind: keyring.SecretTool, Path: bin}} {
		t.Run(h.Name(), func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			const val = "sk-canary-typed-42"
			in := val + "\n" + val + "\n"
			if h.Kind == keyring.SecretTool {
				in = val // secret-tool reads a pipe to its end
			}
			if _, err := w.WriteString(in); err != nil {
				t.Fatal(err)
			}
			_ = w.Close()
			code, err := KeychainPrompt(context.Background(), h.Path, h.AddArgs("typed"), r, 10*time.Second)
			_ = r.Close()
			if err != nil || code != 0 {
				t.Fatalf("prompt = %d, %v", code, err)
			}
			b, err := os.ReadFile(filepath.Join(dir, "item."+keyring.Service+".typed"))
			if err != nil || string(b) != val {
				t.Errorf("stored %q, %v", b, err)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls.jsonl"))
			if err != nil || strings.Contains(string(calls), val) {
				t.Errorf("the value reached the tool's arguments: %s, %v", calls, err)
			}
		})
	}
	if _, err := KeychainPrompt(context.Background(), bin, nil, nil, time.Second); err == nil {
		t.Error("a prompt with no terminal ran")
	}
}

func TestKeychainCommandChecks(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		path string
		args []string
	}{
		{"security", nil},
		{"/usr/bin/../bin/security", nil},
		{"/usr/bin/true", []string{"a\nb"}},
		{"/usr/bin/true", []string{strings.Repeat("a", 300)}},
	} {
		if _, err := KeychainRun(ctx, c.path, c.args, time.Second); err == nil {
			t.Errorf("KeychainRun(%q, %q) ran", c.path, c.args)
		}
	}
}
