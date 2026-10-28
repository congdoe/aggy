package checkup

import (
	"bytes"
	"encoding/json"
	"errors"
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
		msg := k + " is set: the CLI may send its requests to another endpoint, which can bill an API key or sit in the middle of plan traffic"
		if strings.HasPrefix(k, "OPENAI_") {
			// Codex 0.164 does not read OPENAI_BASE_URL (its config key
			// openai_base_url does that), but other OpenAI tools may.
			v = ""
			msg = k + " is set: Codex does not read it, but other OpenAI tools may send their requests to another endpoint, which can bill an API key"
		}
		out = append(out, Finding{Code: CodeBaseURLEnv, Level: LevelWarn, Vendor: v, Message: msg})
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
//   - Claude: settings.json and settings.local.json in
//     $CLAUDE_CONFIG_DIR or ~/.claude: an apiKeyHelper, or an "env"
//     block setting a base-URL variable (BaseURLVar), an API-key variable
//     (see APIKeyEnv), or CLAUDE_CODE_USE_BEDROCK or _VERTEX.
//   - Codex: config.toml in $CODEX_HOME or ~/.codex: a top-level
//     openai_base_url, or a base_url in any [model_providers.<id>]
//     (source-verified at codex rust-v0.164.0). auth.json, next to it,
//     is a credential file and is never read.
//   - Grok: config.toml in $GROK_HOME or ~/.grok: endpoints.xai_api_base_url
//     (which media tools use) or endpoints.models_base_url, and a
//     base_url, env_key, or api_key in any [model.<id>] (a bring-your-own
//     API model) (source-verified at grok-build 2bdd1d6a).
//
// A missing file is not a finding;
// one that cannot be read or parsed is a warning. Each finding's Path is
// the file.
func RouteFiles(environ []string) []Finding {
	get := envLookup(environ)
	var out []Finding
	if dir, ok := vendorHome(get, "CLAUDE_CONFIG_DIR", ".claude"); ok {
		for _, name := range []string{"settings.json", "settings.local.json"} {
			out = append(out, claudeSettings(filepath.Join(dir, name))...)
		}
	}
	if dir, ok := vendorHome(get, "CODEX_HOME", ".codex"); ok {
		out = append(out, codexConfig(filepath.Join(dir, "config.toml"))...)
	}
	if dir, ok := vendorHome(get, "GROK_HOME", ".grok"); ok {
		out = append(out, grokConfig(filepath.Join(dir, "config.toml"))...)
	}
	return out
}

// readConfig reads a vendor config file. A missing file is (nil, nil
// finding); any other failure is a warning naming the file only.
//
// Dotfile managers often link these files, so a symlink is resolved and
// its target read, after the target passes the safefs credential check,
// with safefs.ReadFileLimit, which opens it without following a link.
func readConfig(path string, v adapter.Vendor) ([]byte, *Finding) {
	unreadable := func(why string) *Finding {
		return &Finding{Code: CodeConfigUnreadable, Level: LevelWarn, Vendor: v, Path: path,
			Message: "cannot be checked for base URLs and API keys (" + why + ")"}
	}
	fi, err := safefs.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, unreadable(reason(err))
	}
	target := path
	if fi.Mode()&fs.ModeSymlink != 0 {
		r, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, unreadable("a symlink whose target cannot be found")
		}
		if err := safefs.Check(r); err != nil {
			return nil, unreadable("a symlink to " + reason(err))
		}
		target = r
	}
	b, err := safefs.ReadFileLimit(target, maxConfig)
	if err != nil {
		return nil, unreadable(reason(err))
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

func routeConfig(path string, v adapter.Vendor, key, what string) Finding {
	return Finding{Code: CodeRouteConfig, Level: LevelWarn, Vendor: v, Path: path,
		Message: key + " is set: " + what}
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

func claudeSettings(path string) []Finding {
	b, f := readConfig(path, adapter.Claude)
	if f != nil {
		return []Finding{*f}
	}
	if b == nil {
		return nil
	}
	var s struct {
		Env          any `json:"env"`
		APIKeyHelper any `json:"apiKeyHelper"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &s); err != nil {
		return []Finding{{Code: CodeConfigUnreadable, Level: LevelWarn, Vendor: adapter.Claude, Path: path,
			Message: "cannot be checked for base URLs and API keys (not valid JSON)"}}
	}
	env, _ := s.Env.(map[string]any) // an env that is not an object sets nothing
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Finding
	if set(s.APIKeyHelper) {
		out = append(out, routeConfig(path, adapter.Claude, "apiKeyHelper",
			"Claude Code may get an API key from it and bill the API instead of the subscription"))
	}
	for _, k := range keys {
		if !set(env[k]) {
			continue
		}
		if p, ok := cloudVar(k); ok {
			f := cloudFinding("env."+k, p)
			f.Code, f.Path = CodeRouteConfig, path
			out = append(out, f)
			continue
		}
		if _, ok := BaseURLVar(k); ok {
			out = append(out, routeConfig(path, adapter.Claude, "env."+k,
				"Claude Code may send its requests to another endpoint, which can bill an API key or sit in the middle of plan traffic"))
		} else if _, ok := apiKeyVar(k); ok {
			out = append(out, routeConfig(path, adapter.Claude, "env."+k,
				"Claude Code may bill the API instead of the subscription"))
		}
	}
	return out
}

// tomlConfig reads and decodes a vendor TOML config into v. done is true
// when the caller has nothing more to check: no file, or the findings
// returned.
func tomlConfig(path string, vendor adapter.Vendor, v any) (out []Finding, done bool) {
	b, f := readConfig(path, vendor)
	if f != nil {
		return []Finding{*f}, true
	}
	if b == nil {
		return nil, true
	}
	if _, err := toml.Decode(string(b), v); err != nil {
		return []Finding{{Code: CodeConfigUnreadable, Level: LevelWarn, Vendor: vendor, Path: path,
			Message: "cannot be checked for base URLs and API keys (not valid TOML)"}}, true
	}
	return nil, false
}

// tableKeys returns the keys of a TOML table, sorted.
func tableKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func codexConfig(path string) []Finding {
	var c struct {
		OpenAIBaseURL  any            `toml:"openai_base_url"`
		ModelProviders map[string]any `toml:"model_providers"`
	}
	if out, done := tomlConfig(path, adapter.Codex, &c); done {
		return out
	}
	const what = "Codex may send its requests to another endpoint, which can bill an API key or sit in the middle of plan traffic"
	var out []Finding
	if set(c.OpenAIBaseURL) {
		out = append(out, routeConfig(path, adapter.Codex, "openai_base_url", what))
	}
	for _, id := range tableKeys(c.ModelProviders) {
		if p, _ := c.ModelProviders[id].(map[string]any); set(p["base_url"]) {
			out = append(out, routeConfig(path, adapter.Codex, "model_providers."+id+".base_url", what))
		}
	}
	return out
}

func grokConfig(path string) []Finding {
	var c struct {
		Endpoints map[string]any `toml:"endpoints"`
		Model     map[string]any `toml:"model"`
	}
	if out, done := tomlConfig(path, adapter.Grok, &c); done {
		return out
	}
	var out []Finding
	if set(c.Endpoints["xai_api_base_url"]) {
		out = append(out, routeConfig(path, adapter.Grok, "endpoints.xai_api_base_url",
			"Grok, and its image and video tools, may send requests to another endpoint, which can bill an API key"))
	}
	if set(c.Endpoints["models_base_url"]) {
		out = append(out, routeConfig(path, adapter.Grok, "endpoints.models_base_url",
			"Grok may send its requests to another endpoint, which can bill an API key"))
	}
	for _, id := range tableKeys(c.Model) {
		m, _ := c.Model[id].(map[string]any)
		for _, key := range []string{"base_url", "env_key", "api_key"} {
			if set(m[key]) {
				out = append(out, routeConfig(path, adapter.Grok, "model."+id+"."+key,
					"this model may bill an API key instead of the subscription"))
			}
		}
	}
	return out
}
