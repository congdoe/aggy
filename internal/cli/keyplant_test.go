package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/keyring"
	"github.com/congdoe/aggy/internal/store"
)

// keychainCalls are the fake keychain tool's calls so far, by first argument.
func keychainCalls(t *testing.T, chain string) [][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(chain, "calls.jsonl"))
	if err != nil {
		return nil
	}
	var out [][]string
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var c struct{ Args []string }
		if err := json.Unmarshal(line, &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c.Args)
	}
	return out
}

// TestKeyAddPlantedItem (#138 review H1): an item already in the keychain is
// never updated in place, which may keep the programs it trusts. One planted
// with a program trusted, which aggy did not record, is named as such; on yes
// it is deleted and a fresh item that trusts no program is added; on no, or
// when the delete fails, nothing is stored. aggy never passes -U.
func TestKeyAddPlantedItem(t *testing.T) {
	item := "item." + keyring.Service + ".anth@anthropic"
	acl := "acl." + keyring.Service + ".anth@anthropic"
	cases := []struct {
		name     string
		answer   string
		noDelete bool
		code     int
		wantACL  string
		wantVal  string
		wantText string
	}{
		{name: "yes", answer: "y\n", code: 0, wantACL: "empty", wantVal: keyCanary, wantText: "aggy did not record it"},
		{name: "no", answer: "n\n", code: 1, wantACL: "trusted", wantVal: "planted", wantText: "Nothing changed"},
		{name: "delete fails", answer: "y\n", noDelete: true, code: 1, wantACL: "trusted", wantVal: "planted", wantText: "key.helper_failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chain, _ := keyWorld(t, keyring.Security)
			if c.noDelete {
				t.Setenv("AGGY_FAKE_KEYCHAIN_NODELETE", "1")
			}
			if err := os.WriteFile(filepath.Join(chain, item), []byte("planted"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(chain, acl), []byte("trusted"), 0o600); err != nil {
				t.Fatal(err)
			}
			offerIn = strings.NewReader(c.answer)
			typed(t, keyring.Security, keyCanary)
			code, out, errOut := runKeyCmd("add", "anth", "--provider", "anthropic")
			if code != c.code || !strings.Contains(out+errOut, c.wantText) {
				t.Fatalf("add = %d\nstdout %s\nstderr %s", code, out, errOut)
			}
			if b, _ := os.ReadFile(filepath.Join(chain, acl)); string(b) != c.wantACL {
				t.Errorf("item's trusted programs = %q, want %q", b, c.wantACL)
			}
			if b, _ := os.ReadFile(filepath.Join(chain, item)); string(b) != c.wantVal {
				t.Errorf("item = %q, want %q", b, c.wantVal)
			}
			for _, args := range keychainCalls(t, chain) {
				if slices.Contains(args, "-U") {
					t.Errorf("aggy passed -U: %q", args)
				}
			}
			s, err := store.OpenDefault(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			if keys, _ := s.Keys(context.Background()); (len(keys) == 1) != (c.code == 0) {
				t.Errorf("index = %+v", keys)
			}
		})
	}
}

// TestKeyAddClearsFirst (#138 review H1): with secret-tool, which has no look
// that does not print the value, aggy clears the item before every store, and
// a clear that fails stores nothing. An indexed key on macOS is deleted and
// added fresh too, its old list gone.
func TestKeyAddClearsFirst(t *testing.T) {
	t.Run("secret-tool", func(t *testing.T) {
		chain, _ := keyWorld(t, keyring.SecretTool)
		typed(t, keyring.SecretTool, keyCanary)
		if code, out, errOut := runKeyCmd("add", "anth", "--provider", "anthropic"); code != 0 {
			t.Fatalf("add = %d\n%s%s", code, out, errOut)
		}
		var cmds []string
		for _, a := range keychainCalls(t, chain) {
			cmds = append(cmds, a[0])
		}
		if !slices.Equal(cmds, []string{"clear", "store"}) {
			t.Errorf("calls = %q, want clear then store", cmds)
		}
		t.Setenv("AGGY_FAKE_KEYCHAIN_NODELETE", "1")
		offerIn = strings.NewReader("y\n")
		typed(t, keyring.SecretTool, keyCanary+"b")
		if code, _, errOut := runKeyCmd("add", "anth", "--provider", "anthropic"); code != 1 || !strings.Contains(errOut, "key.helper_failed") {
			t.Errorf("add over a failed clear = %d %q", code, errOut)
		}
		if b, _ := os.ReadFile(filepath.Join(chain, "item."+keyring.Service+".anth@anthropic")); string(b) != keyCanary {
			t.Errorf("item = %q", b)
		}
	})
	t.Run("security, indexed", func(t *testing.T) {
		chain, _ := keyWorld(t, keyring.Security)
		typed(t, keyring.Security, keyCanary)
		if code, out, errOut := runKeyCmd("add", "anth", "--provider", "anthropic"); code != 0 {
			t.Fatalf("add = %d\n%s%s", code, out, errOut)
		}
		// Someone loosens the item's list after aggy stored it.
		acl := filepath.Join(chain, "acl."+keyring.Service+".anth@anthropic")
		if err := os.WriteFile(acl, []byte("trusted"), 0o600); err != nil {
			t.Fatal(err)
		}
		offerIn = strings.NewReader("y\n")
		typed(t, keyring.Security, keyCanary+"b")
		if code, out, errOut := runKeyCmd("add", "anth", "--provider", "anthropic"); code != 0 {
			t.Fatalf("replace = %d\n%s%s", code, out, errOut)
		}
		if b, _ := os.ReadFile(acl); string(b) != "empty" {
			t.Errorf("replaced item's list = %q", b)
		}
	})
}
