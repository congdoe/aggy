package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRefusesHermesStores (T6933): Hermes' credential files and folders, and
// state.db, at its home and in every profile, relocated or not, existing or
// not.
func TestRefusesHermesStores(t *testing.T) {
	tests := []struct {
		name string
		env  string // HERMES_HOME's value, from the relocated dir
		path func(home, reloc string) string
	}{
		{"env", "", func(h, _ string) string { return filepath.Join(h, ".hermes", ".env") }},
		{"auth", "", func(h, _ string) string { return filepath.Join(h, ".hermes", "auth.json") }},
		{"google oauth", "", func(h, _ string) string { return filepath.Join(h, ".hermes", "auth", "google_oauth.json") }},
		{"bws cache", "", func(h, _ string) string { return filepath.Join(h, ".hermes", "cache", "bws_cache.json") }},
		{"mcp tokens folder", "", func(h, _ string) string { return filepath.Join(h, ".hermes", "mcp-tokens") }},
		{"in mcp tokens", "", func(h, _ string) string { return filepath.Join(h, ".hermes", "mcp-tokens", "server.json") }},
		{"browser profile", "", func(h, _ string) string { return filepath.Join(h, ".hermes", "browser-profile", "Cookies") }},
		{"state db", "", func(h, _ string) string { return filepath.Join(h, ".hermes", "state.db") }},
		{"state wal", "", func(h, _ string) string { return filepath.Join(h, ".hermes", "state.db-wal") }},
		{"profile env", "", func(h, _ string) string { return filepath.Join(h, ".hermes", "profiles", "work", ".env") }},
		{"profile auth", "", func(h, _ string) string {
			return filepath.Join(h, ".hermes", "profiles", "aggy-0123456789ab", "auth.json")
		}},
		{"profile state", "", func(h, _ string) string {
			return filepath.Join(h, ".hermes", "profiles", "aggy-0123456789ab", "state.db")
		}},
		{"profile vault", "", func(h, _ string) string { return filepath.Join(h, ".hermes", "profiles", "work", "vault", "x") }},
		{"unclean", "", func(h, _ string) string { return h + "/.hermes//./profiles/p/./auth.json" }},
		{"relocated", "root", func(_, r string) string { return filepath.Join(r, "auth.json") }},
		{"relocated profile", "root", func(_, r string) string { return filepath.Join(r, "profiles", "x", ".env") }},
		{"home is a profile: its root", "profile", func(_, r string) string { return filepath.Join(r, "auth.json") }},
		{"home is a profile: itself", "profile", func(_, r string) string { return filepath.Join(r, "profiles", "x", "state.db") }},
	}
	for _, tt := range tests {
		for _, exists := range []bool{false, true} {
			name := tt.name
			if exists {
				name += " existing"
			}
			t.Run(name, func(t *testing.T) {
				home := fakeHome(t)
				reloc := ""
				switch tt.env {
				case "root":
					reloc = t.TempDir()
					t.Setenv("HERMES_HOME", reloc)
				case "profile":
					reloc = t.TempDir()
					t.Setenv("HERMES_HOME", filepath.Join(reloc, "profiles", "x"))
				}
				p := tt.path(home, reloc)
				if exists {
					writeFake(t, p)
				}
				assertRefused(t, p)
			})
		}
	}
}

// TestHermesSuffixHome: HERMES_DATA_DIR_SUFFIX names a second home.
func TestHermesSuffixHome(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("HERMES_DATA_DIR_SUFFIX", "-dev")
	assertRefused(t, filepath.Join(home, ".hermes-dev", "auth.json"))
	assertRefused(t, filepath.Join(home, ".hermes-dev", "profiles", "p", ".env"))
}

// TestHermesRelativeHomeFailsClosed: a relative HERMES_HOME cannot be
// placed, so nothing is let through.
func TestHermesRelativeHomeFailsClosed(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("HERMES_HOME", "rel/hermes")
	p := filepath.Join(home, "notes.txt")
	writeFake(t, p)
	if _, err := ReadFile(p); !errors.Is(err, ErrLocate) {
		t.Fatalf("ReadFile = %v, want ErrLocate", err)
	}
}

// TestHermesFilesAggyUses: the profile's config.yaml and hook allowlist, and
// the person's root config.yaml, stay readable: aggy writes the first and
// reads the second.
func TestHermesFilesAggyUses(t *testing.T) {
	home := fakeHome(t)
	for _, p := range []string{
		filepath.Join(home, ".hermes", "profiles", "aggy-0123456789ab", "config.yaml"),
		filepath.Join(home, ".hermes", "profiles", "aggy-0123456789ab", "shell-hooks-allowlist.json"),
		filepath.Join(home, ".hermes", "config.yaml"),
		filepath.Join(home, ".hermes", "profiles", "work", "notes", "auth.json"),
		filepath.Join(home, "repo", "auth.json.txt"),
	} {
		writeFake(t, p)
		if _, err := ReadFile(p); err != nil {
			t.Errorf("ReadFile(%s) = %v", p, err)
		}
	}
}

// TestHermesHardLinkInProfile: a hard link to a profile's auth.json, made
// elsewhere, is refused by identity, and not only as a hard link.
func TestHermesHardLinkInProfile(t *testing.T) {
	home := fakeHome(t)
	cred := filepath.Join(home, ".hermes", "profiles", "work", "auth.json")
	writeFake(t, cred)
	link := filepath.Join(home, "elsewhere.txt")
	if err := os.Link(cred, link); err != nil {
		t.Skipf("hard links not supported here: %v", err)
	}
	if _, err := ReadFile(link); !errors.Is(err, ErrAuthPath) {
		t.Fatalf("ReadFile(link) = %v, want ErrAuthPath", err)
	}
}
