package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/keyring"
	"github.com/congdoe/aggy/internal/store"
)

// TestKeyPerson (#138 review L4): keys are read only for an on-screen launch a
// person started at a terminal; a borrow child reopened on screen
// (planHeadless) never gets one, even from a person's terminal.
func TestKeyPerson(t *testing.T) {
	old := offerTerminal
	t.Cleanup(func() { offerTerminal = old })
	none := func(string) string { return "" }
	cases := []struct {
		name   string
		who    planFor
		term   bool
		getenv func(string) string
		want   bool
	}{
		{"on screen, a person", planOnScreen, true, none, true},
		{"a borrow child reopened", planHeadless, true, none, false},
		{"on screen, no terminal", planOnScreen, false, none, false},
		{"on screen, a session's shell", planOnScreen, true, func(k string) string {
			if k == "AGGY_SOCKET" {
				return "/s"
			}
			return ""
		}, false},
	}
	for _, c := range cases {
		offerTerminal = func() bool { return c.term }
		if got := keyPerson(c.who, c.getenv); got != c.want {
			t.Errorf("%s: keyPerson = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestOpenCodeKeyFilePlanted (#138 review M1): the launch's key file is
// written only into a keys folder the launch made itself, never through a
// link and never over a file that is there; a plant refuses the key, leaves
// the plant as it was, and leaves no key anywhere. A folder of the launch's
// own that is there already (config, plugin, instructions) is refused too.
func TestOpenCodeKeyFilePlanted(t *testing.T) {
	cases := []struct {
		name    string
		plant   func(t *testing.T, base, elsewhere string)
		prepErr bool
	}{
		{name: "keys is a link out", plant: func(t *testing.T, base, elsewhere string) {
			if err := os.Symlink(elsewhere, filepath.Join(base, "keys")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a 0644 key file is there", plant: func(t *testing.T, base, _ string) {
			if err := os.Mkdir(filepath.Join(base, "keys"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(base, "keys", "anthropic"), []byte("planted"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "the key file is a link out", plant: func(t *testing.T, base, elsewhere string) {
			if err := os.Mkdir(filepath.Join(base, "keys"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(elsewhere, "anthropic"), filepath.Join(base, "keys", "anthropic")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, dir := openCodeWorld(t)
			elsewhere := t.TempDir()
			in := openCodeInput{cfg: effective(config.Default(), config.RepoConfig{}), caps: openCodeCaps, cwd: repo, dir: dir,
				keys: []store.Key{{Name: "anth", Provider: "anthropic"}}, person: true,
				readKey: func(store.Key) (*keyring.Value, error) { return keyring.Parse([]byte(hexCanary)) }}
			s, err := prepareOpenCode(in)
			if err != nil {
				t.Fatal(err)
			}
			defer s.wipeKeys()
			// Planted between the setup and the write, the window the
			// review named.
			c.plant(t, s.base, elsewhere)
			before := treeHolds(t, filepath.Dir(dir), "planted")
			if _, err := s.writeKeys(); err == nil {
				t.Fatal("writeKeys wrote into a planted folder")
			}
			if hits := treeHolds(t, filepath.Dir(dir), hexCanary); len(hits) > 0 {
				t.Errorf("the key is in %q", hits)
			}
			if hits := treeHolds(t, elsewhere, hexCanary); len(hits) > 0 {
				t.Errorf("the key went through the link to %q", hits)
			}
			if after := treeHolds(t, filepath.Dir(dir), "planted"); len(after) != len(before) {
				t.Errorf("the plant changed: %q, then %q", before, after)
			}
		})
	}
	for _, sub := range []string{"opencode", filepath.Join("opencode", "config"), filepath.Join("opencode", "plugin"), filepath.Join("opencode", "instructions")} {
		t.Run("planted "+sub, func(t *testing.T) {
			repo, dir := openCodeWorld(t)
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
				t.Fatal(err)
			}
			in := openCodeInput{cfg: effective(config.Default(), config.RepoConfig{}), caps: openCodeCaps, cwd: repo, dir: dir}
			if _, err := prepareOpenCode(in); err == nil || !strings.Contains(err.Error(), "launch's own folder") {
				t.Errorf("prepare over a planted %s: %v", sub, err)
			}
		})
	}
}
