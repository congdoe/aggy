package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/store"
)

// TestShouldOffer (T6966): the offer is made only to a person at a terminal on
// a machine where setup never ran and no answer is recorded; deciding writes
// nothing.
func TestShouldOffer(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		term bool
		seed func(t *testing.T, s *store.Store)
		want bool
	}{
		{"a fresh machine", nil, true, nil, true},
		{"an index with no setup", nil, true, func(*testing.T, *store.Store) {}, true},
		{"no terminal", nil, false, nil, false},
		{"a borrow child", map[string]string{"AGGY_DELEGATED": "1"}, true, nil, false},
		{"a session's tool shell", map[string]string{"AGGY_SOCKET": "/x"}, true, nil, false},
		{"turned off", map[string]string{setupOfferEnv: "0"}, true, nil, false},
		{"declined before", nil, true, func(t *testing.T, s *store.Store) {
			if err := s.SetNote(context.Background(), setupOfferNote, offerDeclined, time.Now()); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"setup ran", nil, true, func(t *testing.T, s *store.Store) {
			if err := s.PutVendorStatus(context.Background(), store.VendorStatus{Vendor: "claude", Connected: "unknown", CheckedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			old := offerTerminal
			offerTerminal = func() bool { return tt.term }
			t.Cleanup(func() { offerTerminal = old })
			path, err := store.DefaultPath()
			if err != nil {
				t.Fatal(err)
			}
			if tt.seed != nil {
				s, err := store.OpenDefault(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				tt.seed(t, s)
				_ = s.Close()
			}
			if got := shouldOffer(context.Background(), func(k string) string { return tt.env[k] }); got != tt.want {
				t.Errorf("shouldOffer = %v, want %v", got, tt.want)
			}
			if _, err := os.Stat(path); tt.seed == nil && err == nil {
				t.Error("deciding created the index")
			}
		})
	}
}

// TestOfferSetup (T6966): yes checks the vendors and writes the plan through
// the manifest; no records the answer and writes nothing else; either way the
// answer is recorded.
func TestOfferSetup(t *testing.T) {
	fakes := fakecli.Build(t)
	tests := []struct {
		answer, note string
		wired        bool
	}{
		{"y\n", offerAccepted, true},
		{"n\n", offerDeclined, false},
		{"\n", offerDeclined, false},
	}
	for _, tt := range tests {
		t.Run(strings.TrimSpace(tt.answer), func(t *testing.T) {
			home := isolate(t)
			bin := t.TempDir()
			for _, v := range []string{"codex"} {
				b, err := os.ReadFile(fakes.For(v))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(bin, v), b, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin)
			old := offerIn
			offerIn = strings.NewReader(tt.answer)
			t.Cleanup(func() { offerIn = old })
			w := newFakeWorld(t, fakes)
			var out bytes.Buffer
			offerSetup(context.Background(), w.env(home, "", false), &out)
			rules := filepath.Join(home, ".codex", "rules", "aggy.rules")
			if !strings.Contains(out.String(), "~/.codex/rules/aggy.rules") {
				t.Errorf("the screen does not name the rules file: %q", out.String())
			}
			if _, err := os.Stat(rules); (err == nil) != tt.wired {
				t.Errorf("rules file written %v, want %v; %q", err == nil, tt.wired, out.String())
			}
			s, err := store.OpenDefault(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			if v, ok, _ := s.Note(context.Background(), setupOfferNote); !ok || v != tt.note {
				t.Errorf("note = %q %v, want %q", v, ok, tt.note)
			}
			if got := len(vendorRows(t)) > 0; got != tt.wired {
				t.Errorf("vendors checked %v, want %v", got, tt.wired)
			}
		})
	}
}

// TestSetupWireDryRun (T6966): aggy setup --wire --dry-run shows the changes and
// makes none, with no index; --dry-run alone is a usage error.
func TestSetupWireDryRun(t *testing.T) {
	tests := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--wire", "--dry-run"}, 0, "aggy.rules"},
		{[]string{"--dry-run"}, 2, "goes with --wire"},
	}
	fakes := fakecli.Build(t)
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			home := isolate(t)
			bin := t.TempDir()
			b, err := os.ReadFile(fakes.For("codex"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "codex"), b, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			w := newFakeWorld(t, fakes)
			var out, errb bytes.Buffer
			if code := setupWith(tt.args, w.env(home, "", false), &out, &errb); code != tt.code {
				t.Fatalf("exit %d, want %d; %s %s", code, tt.code, out.String(), errb.String())
			}
			if !strings.Contains(out.String()+errb.String(), tt.want) {
				t.Errorf("output %q %q lacks %q", out.String(), errb.String(), tt.want)
			}
			path, _ := store.DefaultPath()
			if _, err := os.Stat(path); err == nil {
				t.Error("a dry run created the index")
			}
			if _, err := os.Stat(filepath.Join(home, ".codex", "rules", "aggy.rules")); err == nil {
				t.Error("a dry run wrote the rules file")
			}
		})
	}
}
