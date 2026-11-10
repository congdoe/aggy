package claude

import (
	"encoding/json"
	"fmt"
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
// (settings.md), and any key naming "dangerous" or "bypass" with a value that
// is not false, as a later setting such as skipDangerousModePermissionPrompt
// might. Other keys pass: an allow rule cannot carve an exception out of a deny
// rule (permissions.md), so aggy's deny rules hold over the person's allows.
func CheckSettings(layer map[string]any) error {
	gated := func(what string) error { return fmt.Errorf("--settings %s: %w", what, adapter.ErrLoosens) }
	if p, ok := layer["permissions"].(map[string]any); ok {
		if m, ok := p["defaultMode"]; ok {
			s, isString := m.(string)
			if !isString || checkPermissionMode(s) != nil {
				return gated("permissions.defaultMode")
			}
		}
	}
	var walk func(m map[string]any, path string) error
	walk = func(m map[string]any, path string) error {
		for k, v := range m {
			low := strings.ToLower(k)
			tightens := strings.HasPrefix(low, "disable") && strings.Contains(low, "bypass") // disableBypassPermissionsMode
			if (low == "disableallhooks" || strings.Contains(low, "dangerous") || strings.Contains(low, "bypass") && !tightens) && v != false {
				return gated(path + k)
			}
			if s, ok := v.(string); ok && strings.EqualFold(s, "bypassPermissions") {
				return gated(path + k)
			}
			if sub, ok := v.(map[string]any); ok {
				if err := walk(sub, path+k+"."); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(layer, "")
}
