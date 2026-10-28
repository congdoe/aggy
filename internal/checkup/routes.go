package checkup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// Finding codes for routes that can take a CLI off its subscription.
const (
	CodeBaseURLEnv       = "base-url-env"      // a *_BASE_URL variable is set
	CodeRouteConfig      = "route-config"      // a vendor config file sets a base URL or API key
	CodeConfigUnreadable = "config-unreadable" // a vendor config file could not be checked
)

// maxConfig caps the size of a vendor config file the route check reads.
const maxConfig = 1 << 20

// routePrefixes map the variable prefixes of each vendor's endpoints to
// the vendor.
var routePrefixes = []struct {
	prefix string
	vendor adapter.Vendor
}{
	{"ANTHROPIC_", adapter.Claude},
	{"OPENAI_", adapter.Codex},
	{"CODEX_", adapter.Codex},
	{"GROK_", adapter.Grok},
	{"XAI_", adapter.Grok},
}

// BaseURLVar reports whether name is a base-URL variable that can route a
// vendor CLI elsewhere: ANTHROPIC_BASE_URL, OPENAI_BASE_URL,
// GROK_MODELS_BASE_URL, and any other *_BASE_URL starting ANTHROPIC_,
// OPENAI_, CODEX_, GROK_, or XAI_. It returns the vendor concerned.
func BaseURLVar(name string) (adapter.Vendor, bool) {
	if !strings.HasSuffix(name, "_BASE_URL") {
		return "", false
	}
	for _, p := range routePrefixes {
		if strings.HasPrefix(name, p.prefix) {
			return p.vendor, true
		}
	}
	return "", false
}

// apiKeyVar returns the vendor of an API-key variable.
func apiKeyVar(name string) (adapter.Vendor, bool) {
	for _, k := range apiKeys {
		if k.name == name {
			return k.vendor, true
		}
	}
	return "", false
}

// RouteEnv flags each base-URL variable (see BaseURLVar) set and
// non-empty in environ. It names the variable only, never its value.
func RouteEnv(environ []string) []Finding {
	get := envLookup(environ)
	var names []string
	seen := map[string]bool{}
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := BaseURLVar(k); ok && !seen[k] && get(k) != "" {
			seen[k] = true
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var out []Finding
	for _, k := range names {
		v, _ := BaseURLVar(k)
		out = append(out, Finding{
			Code: CodeBaseURLEnv, Level: LevelWarn, Vendor: v,
			Message: k + " is set: the CLI may send its requests to another endpoint, which can bill an API key or sit in the middle of plan traffic",
		})
	}
	return out
}

// vendorHome returns the vendor's config directory from environ: the
// relocation variable when set, else ~/.<name>. ok is false when it
// cannot be found or is not absolute.
func vendorHome(get func(string) string, relocate, name string) (string, bool) {
	if d := get(relocate); d != "" {
		return filepath.Clean(d), filepath.IsAbs(d)
	}
	h := get("HOME")
	if h == "" || !filepath.IsAbs(h) {
		return "", false
	}
	return filepath.Join(h, name), true
}

// RouteFiles reads the vendor config files that can route a CLI off its
// subscription, through safefs (which refuses credential files), and
// flags what it finds by file and key, never by value:
//
//   - Claude: settings.json in $CLAUDE_CONFIG_DIR or ~/.claude, whose
//     "env" block sets a base-URL variable (BaseURLVar) or an API-key
//     variable (ANTHROPIC_API_KEY, OPENAI_API_KEY, XAI_API_KEY).
//   - Grok: config.toml in $GROK_HOME or ~/.grok, whose
//     toolset.image_gen or toolset.video_gen sets base_url or env_key.
//
// Codex is not checked: the key that sets its OpenAI provider's base URL
// is not recorded in docs/adapters.md. A missing file is not a finding;
// one that cannot be read or parsed is, at info level. Paths are shown
// with the home directory as ~.
func RouteFiles(environ []string) []Finding {
	get := envLookup(environ)
	home := get("HOME")
	var out []Finding
	if dir, ok := vendorHome(get, "CLAUDE_CONFIG_DIR", ".claude"); ok {
		out = append(out, claudeSettings(filepath.Join(dir, "settings.json"), home)...)
	}
	if dir, ok := vendorHome(get, "GROK_HOME", ".grok"); ok {
		out = append(out, grokConfig(filepath.Join(dir, "config.toml"), home)...)
	}
	return out
}

// readConfig reads a vendor config file. A missing file is (nil, nil
// finding); any other failure is an info finding naming the file only.
func readConfig(path, home string, v adapter.Vendor) ([]byte, *Finding) {
	b, err := safefs.ReadFileLimit(path, maxConfig)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &Finding{Code: CodeConfigUnreadable, Level: LevelInfo, Vendor: v,
			Message: fmt.Sprintf("%s: cannot be checked for base URLs and API keys (%s)", ShowPath(path, home), reason(err))}
	}
	return b, nil
}

// reason says why a file could not be read, without its content.
func reason(err error) string {
	switch {
	case errors.Is(err, safefs.ErrAuthPath):
		return "a credential file"
	case errors.Is(err, safefs.ErrNotRegular):
		return "not a regular file"
	case errors.Is(err, safefs.ErrTooLarge):
		return "too large"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	return "unreadable"
}

func routeConfig(path, home string, v adapter.Vendor, key, what string) Finding {
	return Finding{Code: CodeRouteConfig, Level: LevelWarn, Vendor: v,
		Message: fmt.Sprintf("%s: %s is set: %s", ShowPath(path, home), key, what)}
}

// set reports whether a config value is present and not empty.
func set(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	}
	return true
}

func claudeSettings(path, home string) []Finding {
	b, f := readConfig(path, home, adapter.Claude)
	if f != nil {
		return []Finding{*f}
	}
	if b == nil {
		return nil
	}
	var s struct {
		Env any `json:"env"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &s); err != nil {
		return []Finding{{Code: CodeConfigUnreadable, Level: LevelInfo, Vendor: adapter.Claude,
			Message: ShowPath(path, home) + ": cannot be checked for base URLs and API keys (not valid JSON)"}}
	}
	env, _ := s.Env.(map[string]any) // an env that is not an object sets nothing
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Finding
	for _, k := range keys {
		if !set(env[k]) {
			continue
		}
		if _, ok := BaseURLVar(k); ok {
			out = append(out, routeConfig(path, home, adapter.Claude, "env."+k,
				"Claude Code may send its requests to another endpoint, which can bill an API key or sit in the middle of plan traffic"))
		} else if _, ok := apiKeyVar(k); ok {
			out = append(out, routeConfig(path, home, adapter.Claude, "env."+k,
				"Claude Code may bill the API instead of the subscription"))
		}
	}
	return out
}

func grokConfig(path, home string) []Finding {
	b, f := readConfig(path, home, adapter.Grok)
	if f != nil {
		return []Finding{*f}
	}
	if b == nil {
		return nil
	}
	var c struct {
		Toolset map[string]any `toml:"toolset"`
	}
	if _, err := toml.Decode(string(b), &c); err != nil {
		return []Finding{{Code: CodeConfigUnreadable, Level: LevelInfo, Vendor: adapter.Grok,
			Message: ShowPath(path, home) + ": cannot be checked for media API settings (not valid TOML)"}}
	}
	var out []Finding
	for _, tool := range []string{"image_gen", "video_gen"} {
		tbl, _ := c.Toolset[tool].(map[string]any)
		for _, key := range []string{"base_url", "env_key"} {
			if set(tbl[key]) {
				out = append(out, routeConfig(path, home, adapter.Grok, "toolset."+tool+"."+key,
					"Grok may send media requests to an API key instead of the subscription"))
			}
		}
	}
	return out
}
