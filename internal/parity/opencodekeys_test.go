package parity

import (
	"slices"
	"testing"
)

// TestOpenCodeKeyFiles (T6931): a key reaches OpenCode only as a file the
// config names; anthropic is allowed only with one; a bad path or a provider
// the launch does not allow refuses the launch.
func TestOpenCodeKeyFiles(t *testing.T) {
	cases := []struct {
		name      string
		providers []string
		keys      map[string]string
		ok        bool
		rules     []openCodeRule
	}{
		{"no key, anthropic denied", []string{"xai"}, nil, true, []openCodeRule{
			{"provider.use", "*", "deny"}, {"provider.use", "xai", "allow"},
			{"provider.use", "claude-agent", "deny"}, {"provider.use", "anthropic", "deny"}, {"provider.use", "opencode", "deny"}}},
		{"anthropic keyed", []string{"xai", "anthropic"}, map[string]string{"anthropic": "/run/x.memory/opencode/keys/anthropic"}, true, []openCodeRule{
			{"provider.use", "*", "deny"}, {"provider.use", "xai", "allow"}, {"provider.use", "anthropic", "allow"},
			{"provider.use", "claude-agent", "deny"}, {"provider.use", "opencode", "deny"}}},
		{"openrouter keyed", []string{"openrouter"}, map[string]string{"openrouter": "/k/openrouter"}, true, nil},
		{"anthropic with no key", []string{"anthropic"}, nil, false, nil},
		{"key for a provider not allowed", []string{"xai"}, map[string]string{"anthropic": "/k/anthropic"}, false, nil},
		{"relative path", []string{"anthropic"}, map[string]string{"anthropic": "k/anthropic"}, false, nil},
		{"brace in path", []string{"anthropic"}, map[string]string{"anthropic": "/k/{env:HOME}/a"}, false, nil},
		{"closing brace in path", []string{"anthropic"}, map[string]string{"anthropic": "/k/a}b"}, false, nil},
		{"unclean path", []string{"anthropic"}, map[string]string{"anthropic": "/k/../a"}, false, nil},
		{"claude-agent keyed", []string{"claude-agent"}, map[string]string{"claude-agent": "/k/c"}, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Providers, cfg.KeyFiles = c.providers, c.keys
			_, err := cfg.Render()
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok %v", err, c.ok)
			}
			if !c.ok {
				return
			}
			d := renderDoc(t, cfg)
			if c.rules != nil {
				var got []openCodeRule
				for _, r := range d.Policies {
					if r.Action == "provider.use" {
						got = append(got, r)
					}
				}
				if !slices.Equal(got, c.rules) {
					t.Errorf("rules = %+v, want %+v", got, c.rules)
				}
			}
			if len(d.Provider) != len(c.keys) {
				t.Errorf("provider block = %+v", d.Provider)
			}
			for p, path := range c.keys {
				if got := d.Provider[p].Options.APIKey; got != "{file:"+path+"}" {
					t.Errorf("%s apiKey = %q", p, got)
				}
			}
		})
	}
}
