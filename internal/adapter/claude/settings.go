package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
)

// MaxSettings caps a --settings file, as Claude does: "The file must be a
// regular file no larger than 2 MiB" (cli-reference, read 2026-11-09).
const MaxSettings = 2 << 20

// maxInlineSettings caps the merged layer aggy passes inline: argv is
// limited (ARG_MAX), and a layer that big belongs in a settings file.
const maxInlineSettings = 256 << 10

// ParseSettings reads one --settings layer the person gave: the inline JSON,
// or the bytes of the file the value names. It must be a JSON object.
func ParseSettings(b []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, fmt.Errorf("--settings: not a JSON object, which Claude reads it as")
	}
	return m, nil
}

// InlineSettings reports whether a --settings value is inline JSON rather
// than a path ("Path to a settings JSON file or an inline JSON string",
// cli-reference).
func InlineSettings(val string) bool { return strings.HasPrefix(strings.TrimSpace(val), "{") }

// MergeSettings lays b over a, as Claude's settings layers combine: objects
// merge key by key, lists join (permissions and hooks lists merge across
// layers, settings.md), and any other value in b replaces a's. Neither is
// changed.
func MergeSettings(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		switch bv := v.(type) {
		case map[string]any:
			if av, ok := out[k].(map[string]any); ok {
				out[k] = MergeSettings(av, bv)
				continue
			}
		case []any:
			if av, ok := out[k].([]any); ok {
				out[k] = append(slices.Clone(av), bv...)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// SettingsJSON is the one settings layer an on-screen or print launch passes
// (ok false when there is none): its permission lists merge with every
// settings file's, and aggy's policy rules (deny, or ask on screen; every one
// a deny in print mode, planHeadless) win over any allow
// (code.claude.com/docs settings.md, permissions.md). A person's layer cannot
// empty or drop them: lists join, and a non-list value of theirs under
// permissions gives way to aggy's (MergeSettings; TestMergeKeepsPolicy). The tools' hooks ride in the same layer, for this session
// only, as the borrow guard's do (GuardSettings). The person's own --settings
// layers go in it too, under aggy's (T6962, mergeUser): lists join, so aggy's
// policy rules and hooks stay, and where both set one value
// aggy's wins, but for a stricter defaultMode of plan.
//
// aggy's own layer goes inline when the person gave no --settings file: it
// holds rule patterns and hook commands, no secret, and an inline layer needs
// no file the CLI may read after aggy removes it.
func SettingsJSON(req adapter.InteractiveRequest) ([]byte, bool, error) {
	perms := permissionsLayer(req)
	if len(perms) == 0 && len(req.Hooks) == 0 && len(req.UserSettings) == 0 {
		return nil, false, nil
	}
	layer := map[string]any{}
	if len(perms) > 0 {
		layer["permissions"] = perms
	}
	if len(req.Hooks) > 0 {
		layer["hooks"] = req.Hooks
	}
	if len(req.UserSettings) > 0 {
		own, err := asSettings(layer)
		if err != nil {
			return nil, false, err
		}
		layer, _ = mergeUser(req.UserSettings, own)
	}
	js, err := json.Marshal(layer)
	return js, err == nil, err
}

// SettingsOverrides names the keys of the person's --settings that aggy's own
// layer replaces for this launch, for a note (PR #124 review).
func SettingsOverrides(req adapter.InteractiveRequest) []string {
	if len(req.UserSettings) == 0 {
		return nil
	}
	layer := map[string]any{}
	if perms := permissionsLayer(req); len(perms) > 0 {
		layer["permissions"] = perms
	}
	if len(req.Hooks) > 0 {
		layer["hooks"] = req.Hooks
	}
	own, err := asSettings(layer)
	if err != nil {
		return nil
	}
	_, keys := mergeUser(req.UserSettings, own)
	return keys
}

// asSettings is aggy's own layer as plain JSON values, for MergeSettings.
func asSettings(layer map[string]any) (map[string]any, error) {
	b, err := json.Marshal(layer)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

// joinMCPConfig puts file into the person's first --mcp-config in extra, as
// its first value, and reports whether there was one. The "=" form becomes two
// words, so the file can go before its value (unverified: whether commander's
// "=" form also takes the words after it; written as two words it does).
// Nothing after "--" is looked at.
func joinMCPConfig(extra []string, file string) ([]string, bool) {
	for i, w := range extra {
		if w == "--" {
			break
		}
		name, val, hasVal := strings.Cut(w, "=")
		if name != "--mcp-config" {
			continue
		}
		out := append(slices.Clone(extra[:i]), "--mcp-config", file)
		if hasVal {
			out = append(out, val)
		}
		return append(out, extra[i+1:]...), true
	}
	return extra, false
}

// CheckSettings refuses a settings layer the person gave that would get past
// aggy's safety policy or hooks (adapter.ErrLoosens, which --no-policy lets
// through): a permissions.defaultMode the --permission-mode table gates,
// disableAllHooks, which turns off hooks aggy's turn-end and tools rely on
// (settings.md), allowManagedHooksOnly and allowManagedPermissionRulesOnly,
// which would drop aggy's hooks and deny rules (neither is managed),
// enableAllProjectMcpServers, which skips the project MCP trust prompt, and
// any key naming "dangerous" or "bypass" with a value that is not false, as a
// later setting such as skipDangerousModePermissionPrompt might. Arrays are
// walked as well as objects.
//
// Refused outright, --no-policy or not (ErrSettingsCredential): an env holding
// an API key, an auth token, or a *_BASE_URL variable, and apiKeyHelper. aggy
// never sets those for a CLI (CLAUDE.md), and passing them in argv would; the
// person can put them in their own Claude settings file, which aggy does not
// touch.
//
// Other keys pass: an allow rule cannot carve an exception out of a deny or
// ask rule (permissions.md), so aggy's policy rules (deny, or ask on screen)
// hold over the person's allows. TODO(T6895): that a
// person's defaultMode of auto or dontAsk still stops at aggy's ask rules is
// unchecked. Keys
// are read in sorted order, so the refusal names the same one each time.
func CheckSettings(layer map[string]any) error {
	gated := func(what string) error { return fmt.Errorf("--settings %s: %w", what, adapter.ErrLoosens) }
	for _, k := range credentialHelpers {
		if _, ok := layer[k]; ok {
			return fmt.Errorf("--settings %s: %w", k, ErrSettingsCredential)
		}
	}
	if env, ok := layer["env"].(map[string]any); ok {
		for _, k := range sortedKeys(env) {
			if credentialVar(k, env[k]) {
				return fmt.Errorf("--settings env.%s: %w", k, ErrSettingsCredential)
			}
		}
	}
	if p, ok := layer["permissions"].(map[string]any); ok {
		if m, ok := p["defaultMode"]; ok {
			s, isString := m.(string)
			if !isString || checkPermissionMode(s) != nil {
				return gated("permissions.defaultMode")
			}
		}
	}
	var walk func(v any, path string) error
	walk = func(v any, path string) error {
		switch v := v.(type) {
		case map[string]any:
			for _, k := range sortedKeys(v) {
				sub := v[k]
				low := strings.ToLower(k)
				tightens := strings.HasPrefix(low, "disable") && strings.Contains(low, "bypass") // disableBypassPermissionsMode
				if (slices.Contains(loosening, low) || strings.Contains(low, "dangerous") || strings.Contains(low, "bypass") && !tightens) && sub != false {
					return gated(path + k)
				}
				if err := walk(sub, path+k+"."); err != nil {
					return err
				}
			}
		case []any:
			for i, e := range v {
				if err := walk(e, fmt.Sprintf("%s%d.", path, i)); err != nil {
					return err
				}
			}
		case string:
			if strings.EqualFold(v, "bypassPermissions") {
				return gated(strings.TrimSuffix(path, "."))
			}
		}
		return nil
	}
	return walk(layer, "")
}

// loosening are the settings keys, in lower case, that CheckSettings gates by
// name. The settings reference's index (code.claude.com/docs/en/
// settings-reference, read 2026-11-10): disableAllHooks "Turn off hooks";
// allowManagedHooksOnly "Run only the hooks your organization deploys";
// allowManagedPermissionRulesOnly "Make managed settings the only settings
// source of permission rules"; enableAllProjectMcpServers "Approve every server
// in project .mcp.json files without a prompt"; enabledMcpjsonServers "Approve
// specific servers from a project's .mcp.json" (PR #124 re-review).
var loosening = []string{"disableallhooks", "allowmanagedhooksonly", "allowmanagedpermissionrulesonly", "enableallprojectmcpservers",
	"enabledmcpjsonservers"}

// credentialHelpers are the settings keys that run a command of the person's
// to produce a credential or headers, refused outright like the credentials
// themselves. Same index: apiKeyHelper "Generate the API credential with your
// own command"; otelHeadersHelper "Generate rotating OpenTelemetry headers with
// your own command"; awsAuthRefresh "Refresh expired Bedrock credentials in
// .aws with your own command"; awsCredentialExport "Supply Bedrock credentials
// as JSON from your own command"; gcpAuthRefresh "Refresh Google Cloud
// credentials with your own command". policyHelper "Run an executable that
// computes managed settings at startup" is refused too: it would set policy
// over aggy's. The fetch read the index, not every full entry (the page was
// truncated), so a later key of this kind is not known.
var credentialHelpers = []string{"apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "gcpAuthRefresh", "otelHeadersHelper", "policyHelper"}

// ErrSettingsCredential matches a --settings layer that would set a credential
// or an endpoint for the CLI, which aggy never passes, with --no-policy or not.
var ErrSettingsCredential = errors.New("it sets a credential or an API endpoint for the CLI, which aggy never passes on its command line; put it in your own Claude settings file")

// credentialVar reports whether an env key names an API key, a token, a
// secret, a password, an authorization or custom header, or a base URL, or is a
// proxy variable whose value carries user@ credentials.
func credentialVar(k string, v any) bool {
	u := strings.ToUpper(k)
	switch {
	case strings.HasSuffix(u, "_BASE_URL"), strings.Contains(u, "API_KEY"), strings.Contains(u, "AUTH_TOKEN"),
		strings.HasSuffix(u, "_TOKEN"), strings.Contains(u, "SECRET"), strings.Contains(u, "PASSWORD"),
		strings.HasSuffix(u, "_CUSTOM_HEADERS"), strings.Contains(u, "_AUTHORIZATION"), strings.HasPrefix(u, "AUTHORIZATION"):
		return true
	case u == "HTTP_PROXY" || u == "HTTPS_PROXY" || u == "ALL_PROXY":
		s, _ := v.(string)
		return strings.Contains(s, "@")
	}
	return false
}

// sortedKeys are m's keys in order.
func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// mergeUser lays aggy's own layer over the person's (MergeSettings), except
// that a person's permissions.defaultMode of plan, stricter than any mode aggy
// sets, stands (PR #124 review). It returns the dotted keys where aggy's value
// replaced a different one of theirs, for a note at launch.
func mergeUser(user, own map[string]any) (map[string]any, []string) {
	if p, ok := user["permissions"].(map[string]any); ok && p["defaultMode"] == "plan" {
		if op, ok := own["permissions"].(map[string]any); ok {
			op = maps.Clone(op)
			delete(op, "defaultMode")
			own = maps.Clone(own)
			own["permissions"] = op
		}
	}
	return MergeSettings(user, own), overridden(user, own, "")
}

// overridden lists the dotted keys whose scalar value in b differs from a's.
func overridden(a, b map[string]any, path string) []string {
	var out []string
	for _, k := range sortedKeys(b) {
		av, ok := a[k]
		if !ok {
			continue
		}
		switch bv := b[k].(type) {
		case map[string]any:
			if am, ok := av.(map[string]any); ok {
				out = append(out, overridden(am, bv, path+k+".")...)
			}
		case []any:
		default:
			if _, isList := av.([]any); !isList && fmt.Sprint(av) != fmt.Sprint(bv) {
				out = append(out, path+k)
			}
		}
	}
	return out
}
