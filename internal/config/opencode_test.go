package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

func TestOpenCodeDefaults(t *testing.T) {
	c := Default()
	if c.OpenCode.Auto {
		t.Error("full auto must be off until the person sets it")
	}
	if c.OpenCode.FreeModels {
		t.Error("OpenCode's free models must be off until the person turns them on")
	}
	if got := Merge(c, RepoConfig{}).OpenCodeProviders(); slices.Contains(got, OpenCodeFreeProvider) {
		t.Errorf("default providers %q hold OpenCode's own", got)
	}
	if !slices.Equal(c.OpenCode.Providers, []string{"openai", "xai"}) {
		t.Errorf("providers = %q", c.OpenCode.Providers)
	}
	// TODO(T6937): the plan table and first-run questions add it; until then
	// the person does, by naming it in [switch] order.
	if slices.Contains(c.Switch.Order, adapter.OpenCode) {
		t.Error("the picker offers opencode before the person added it")
	}
	r := c.Routing
	for _, route := range []Route{r.Image, r.Video, r.Research, r.News, r.Review, r.Edit} {
		if slices.Contains(route.Vendors, adapter.OpenCode) {
			t.Errorf("opencode is in a default route: %+v", route)
		}
	}
	if slices.Contains(adapter.Vendors(), adapter.OpenCode) {
		t.Error("Vendors lists opencode, which would give it hooks, tools, and borrows")
	}
}

func TestOpenCodeParse(t *testing.T) {
	cases := []struct {
		name, text, wantErr string
	}{
		{"auto and providers", "[opencode]\nauto = true\nproviders = [\"xai\", \"mistral\"]\n", ""},
		{"claude-agent never", "[opencode]\nproviders = [\"claude-agent\"]\n", "never allows"},
		{"opencode's own service only through free_models", "[opencode]\nproviders = [\"opencode\"]\n", "opencode-free-models on"},
		{"the repo name is not a provider", "[opencode]\nproviders = [\"opencode-free\"]\n", "opencode-free-models on"},
		{"free models on", "[opencode]\nfree_models = true\n", ""},
		{"free models not a bool", "[opencode]\nfree_models = \"yes\"\n", "free_models"},
		{"anthropic until aggy key", "[opencode]\nproviders = [\"anthropic\"]\n", "never allows"},
		{"a bad name", "[opencode]\nproviders = [\"../x\"]\n", "not a provider id"},
		{"a route to it", "[routing]\nreview = [\"opencode\"]\n", "on screen only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.text)
			if c.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
		})
	}
}

func TestOpenCodeProviders(t *testing.T) {
	user := Default()
	user.OpenCode.Providers = []string{"openai", "xai", "mistral"}
	cases := []struct {
		name  string
		repo  []adapter.Vendor
		want  []string
		allow bool
	}{
		{"no list: all", nil, []string{"openai", "xai", "mistral"}, true},
		{"opencode alone: only other names count", []adapter.Vendor{"opencode"}, []string{}, true},
		{"grok counts for xai", []adapter.Vendor{"opencode", "grok"}, []string{"xai"}, true},
		{"codex counts for openai", []adapter.Vendor{"opencode", "codex"}, []string{"openai"}, true},
		{"a provider by its own name", []adapter.Vendor{"opencode", "mistral"}, []string{"mistral"}, true},
		{"a repo that leaves opencode out", []adapter.Vendor{"grok", "codex"}, []string{"openai", "xai"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := Merge(user, RepoConfig{AllowedVendors: c.repo})
			if got := e.OpenCodeProviders(); !slices.Equal(got, c.want) {
				t.Errorf("providers = %q, want %q", got, c.want)
			}
			if e.Allows(adapter.OpenCode) != c.allow {
				t.Errorf("Allows(opencode) = %v, want %v", e.Allows(adapter.OpenCode), c.allow)
			}
		})
	}
}

// TestOpenCodeFreeModels: the user's free_models adds OpenCode's own
// provider; a repo's allowed_vendors refuses it unless it names
// opencode-free; a repo's own free_models turns nothing on.
func TestOpenCodeFreeModels(t *testing.T) {
	cases := []struct {
		name     string
		user     string // user config text
		repo     string // repo config text
		want     []string
		repoSets bool
	}{
		{"off", "", "", []string{"openai", "xai"}, false},
		{"on: added last", "[opencode]\nfree_models = true\n", "", []string{"openai", "xai", "opencode"}, false},
		{"on, repo allows opencode only: refused", "[opencode]\nfree_models = true\n",
			`allowed_vendors = ["opencode", "codex"]`, []string{"openai"}, false},
		{"on, repo names opencode-free", "[opencode]\nfree_models = true\n",
			`allowed_vendors = ["opencode", "opencode-free"]`, []string{"opencode"}, false},
		{"on, repo leaves opencode out", "[opencode]\nfree_models = true\n",
			`allowed_vendors = ["codex"]`, []string{"openai"}, false},
		{"off, repo turns it on: ignored", "", "[opencode]\nfree_models = true\n", []string{"openai", "xai"}, true},
		{"off, repo turns it on and lists it: still ignored", "",
			"allowed_vendors = [\"opencode\", \"opencode-free\"]\n[opencode]\nfree_models = true\n", []string{}, true},
		{"on, repo turns it off: ignored too", "[opencode]\nfree_models = true\n", "[opencode]\nfree_models = false\n",
			[]string{"openai", "xai", "opencode"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			user, err := Parse(c.user)
			if err != nil {
				t.Fatal(err)
			}
			repo, err := ParseRepo(c.repo)
			if err != nil {
				t.Fatal(err)
			}
			e := Merge(user, repo)
			if got := e.OpenCodeProviders(); !slices.Equal(got, c.want) {
				t.Errorf("providers = %q, want %q", got, c.want)
			}
			if repo.SetsFreeModels() != c.repoSets {
				t.Errorf("SetsFreeModels = %v, want %v", repo.SetsFreeModels(), c.repoSets)
			}
			if !c.repoSets && e.OpenCode.FreeModels != user.OpenCode.FreeModels {
				t.Error("Merge changed free_models")
			}
		})
	}
	// Any other key in a repo's [opencode] is still unknown.
	if _, err := ParseRepo("[opencode]\nproviders = [\"opencode\"]\n"); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("a repo's [opencode] providers: err = %v, want an unknown key", err)
	}
}

func TestAllowedVendorsNames(t *testing.T) {
	cases := []struct {
		text string
		ok   bool
	}{
		{`allowed_vendors = ["opencode", "mistral"]`, true},
		{`allowed_vendors = ["claude", "mistral"]`, false}, // no opencode: a typo stays an error
		{`allowed_vendors = ["opencode", "../x"]`, false},
		{`allowed_vendors = ["opencode", "opencode"]`, false},
	}
	for _, c := range cases {
		if _, err := ParseRepo(c.text); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.text, err, c.ok)
		}
	}
}
