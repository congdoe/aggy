package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

func TestParseRepo(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    RepoConfig
		wantErr string
	}{
		{name: "empty", text: "", want: RepoConfig{}},
		{
			name: "full",
			text: `
allowed_vendors = ["claude", "codex"]
check = [["make", "lint"], ["go", "test", "./..."]]

[worktree]
copy = [".env", "config/local.json"]
setup = ["npm", "ci"]
`,
			want: RepoConfig{
				AllowedVendors: []adapter.Vendor{adapter.Claude, adapter.Codex},
				Check:          []Command{{"make", "lint"}, {"go", "test", "./..."}},
				Worktree:       Worktree{Copy: []string{".env", "config/local.json"}, Setup: Command{"npm", "ci"}},
			},
		},
		{name: "empty allow list allows none", text: `allowed_vendors = []`, want: RepoConfig{AllowedVendors: []adapter.Vendor{}}},
		{name: "unknown key", text: "allowed_vendors = []\nmodel = \"x\"\n", wantErr: "unknown key model"},
		{name: "unknown table", text: "[routing]\nimage = [\"grok\"]\n", wantErr: "unknown key routing"},
		{name: "user-only key", text: `default_vendor = "grok"`, wantErr: "unknown key default_vendor"},
		{name: "bad vendor", text: `allowed_vendors = ["gemini"]`, wantErr: "unknown vendor"},
		{name: "duplicate vendor", text: `allowed_vendors = ["grok", "grok"]`, wantErr: "listed twice"},
		{name: "shell string check", text: `check = ["make test"]`, wantErr: "check"},
		{name: "empty check", text: `check = [[]]`, wantErr: "check[0]"},
		{name: "empty setup program", text: "[worktree]\nsetup = [\"\"]\n", wantErr: "worktree.setup"},
		{name: "absolute copy", text: "[worktree]\ncopy = [\"/etc/passwd\"]\n", wantErr: "relative"},
		{name: "escaping copy", text: "[worktree]\ncopy = [\"../secrets\"]\n", wantErr: "inside the repo"},
		{name: "unclean copy", text: "[worktree]\ncopy = [\"a/../../b\"]\n", wantErr: "clean path"},
		{name: "git copy", text: "[worktree]\ncopy = [\".git/config\"]\n", wantErr: "inside .git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRepo(tt.text)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestLoadRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	rc, err := LoadRepo(repo)
	if err != nil || rc.AllowedVendors != nil {
		t.Fatalf("missing file = %+v, %v, want zero", rc, err)
	}
	if err := os.MkdirAll(filepath.Join(repo, RepoDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RepoPath(repo), []byte(`allowed_vendors = ["grok"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, err = LoadRepo(repo)
	if err != nil || !slices.Equal(rc.AllowedVendors, []adapter.Vendor{adapter.Grok}) {
		t.Errorf("LoadRepo = %+v, %v", rc, err)
	}
	if err := os.WriteFile(RepoPath(repo), []byte("bogus = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRepo(repo); err == nil || !strings.Contains(err.Error(), RepoPath(repo)) {
		t.Errorf("unknown key error = %v, want it to name the file", err)
	}

	// It reads through safefs: a repo config that is a link to a vendor
	// credential file is refused.
	auth := filepath.Join(home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(auth), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(RepoPath(repo)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(auth, RepoPath(repo)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRepo(repo); !errors.Is(err, safefs.ErrAuthPath) {
		t.Errorf("LoadRepo(link to credential) = %v, want ErrAuthPath", err)
	}
}

func TestMergeNarrowsNeverWidens(t *testing.T) {
	user := Default()
	user.DefaultVendor = adapter.Grok
	user.Routing.Image = Route{Vendors: []adapter.Vendor{adapter.Codex}}
	user.Vendors.Grok.Model = "m"
	userCopy := Default()
	userCopy.DefaultVendor = adapter.Grok
	userCopy.Routing.Image = Route{Vendors: []adapter.Vendor{adapter.Codex}}
	userCopy.Vendors.Grok.Model = "m"

	tests := []struct {
		name    string
		allowed []adapter.Vendor
		check   func(t *testing.T, e Effective)
	}{
		{"unset allows all", nil, func(t *testing.T, e Effective) {
			if !reflect.DeepEqual(e.Config, user) || len(e.Allowed) != 3 {
				t.Errorf("unset narrowed something: %+v", e)
			}
		}},
		{"claude and codex", []adapter.Vendor{adapter.Codex, adapter.Claude}, func(t *testing.T, e Effective) {
			if !slices.Equal(e.Allowed, []adapter.Vendor{adapter.Claude, adapter.Codex}) {
				t.Errorf("Allowed = %v", e.Allowed)
			}
			if e.DefaultVendor != "" {
				t.Errorf("default vendor %q kept though not allowed", e.DefaultVendor)
			}
			if !slices.Equal(e.Switch.Order, []adapter.Vendor{adapter.Claude, adapter.Codex}) {
				t.Errorf("switch order = %v", e.Switch.Order)
			}
			if !slices.Equal(e.Routing.News.Vendors, []adapter.Vendor{adapter.Codex}) {
				t.Errorf("news = %v", e.Routing.News.Vendors)
			}
			if len(e.Routing.Video.Vendors) != 0 {
				t.Errorf("video = %v, want none (grok not allowed)", e.Routing.Video.Vendors)
			}
			if !e.Routing.Review.NotAuthor || e.Allows(adapter.Grok) || !e.Allows(adapter.Codex) {
				t.Errorf("review or Allows wrong: %+v", e.Routing.Review)
			}
		}},
		// The repo allows grok, but the user routed image only to codex:
		// grok is not added.
		{"no widening", []adapter.Vendor{adapter.Grok}, func(t *testing.T, e Effective) {
			if len(e.Routing.Image.Vendors) != 0 {
				t.Errorf("image = %v, want none", e.Routing.Image.Vendors)
			}
			if e.DefaultVendor != adapter.Grok {
				t.Errorf("allowed default vendor cleared")
			}
		}},
		{"none allowed", []adapter.Vendor{}, func(t *testing.T, e Effective) {
			if len(e.Allowed) != 0 || len(e.Switch.Order) != 0 {
				t.Errorf("empty allow list kept vendors: %+v", e)
			}
			for _, k := range adapter.Kinds() {
				if len(e.Routing.For(k).Vendors) != 0 {
					t.Errorf("%s still routed: %v", k, e.Routing.For(k).Vendors)
				}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := Merge(user, RepoConfig{AllowedVendors: tt.allowed})
			tt.check(t, e)
			// Every merged list is a subset of the user's.
			for _, k := range adapter.Kinds() {
				for _, v := range e.Routing.For(k).Vendors {
					if !slices.Contains(user.Routing.For(k).Vendors, v) {
						t.Errorf("%s widened with %s", k, v)
					}
				}
			}
			if e.Budgets != user.Budgets || e.Vendors != user.Vendors {
				t.Error("models or budgets changed")
			}
			if !reflect.DeepEqual(user, userCopy) {
				t.Error("Merge modified the user config")
			}
		})
	}
}
