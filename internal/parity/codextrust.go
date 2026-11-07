package parity

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
)

// Codex keeps a folder's trust in config.toml, under [projects."<dir>"] as
// trust_level. aggy's own edit child overrides the same key
// (docs/verification.md, "-c projects=", source only), but what a person's
// trusted entry does in a live Codex has not been checked, and neither has
// whether Codex looks up a worktree by its main checkout, so this is taken to
// be the key, not known to be. TODO(T6895): confirm with a live check.

var inlineProjects = regexp.MustCompile(`(?m)^[ \t]*"?projects"?[ \t]*=`)

// ErrCodexTrustConflict means the config already says something else about the
// folder, which is the person's to change.
var ErrCodexTrustConflict = errors.New("parity: Codex's config already sets this folder's trust to something else")

// CodexTrustState says whether the config at path already trusts dir.
func CodexTrustState(path, dir string) (trusted bool, hash string, err error) {
	hash = patch.NoFile
	b, err := safefs.ReadConfig(path, 4<<20)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, hash, nil
	case err != nil:
		return false, "", err
	}
	hash = patch.Hash(b)
	got, err := AddCodexTrust(b, dir)
	if err != nil {
		return false, "", err
	}
	return string(got) == string(b), hash, nil
}

// CodexTrustPatch returns the change that trusts dir, to apply through a
// manifest so aggy unwire takes it back.
func CodexTrustPatch(path, dir, hash string) patch.Patch {
	return patch.Patch{File: path, ExpectedHash: hash, Mode: 0o600, Transform: func(src []byte) ([]byte, error) {
		return AddCodexTrust(src, dir)
	}}
}

// AddCodexTrust adds trust_level = "trusted" for dir by a minimal text edit,
// never a re-encoding, and checks the result decodes to the old document plus
// that one key. A config that already trusts dir comes back unchanged.
func AddCodexTrust(src []byte, dir string) ([]byte, error) {
	if strings.ContainsAny(dir, "\n\r\x00") {
		return nil, errors.New("a folder with a line break in its name cannot go in Codex's config")
	}
	old := map[string]any{}
	if err := toml.Unmarshal(src, &old); err != nil {
		return nil, fmt.Errorf("parity: Codex's config does not parse: %w", err)
	}
	projects, hasProjects := old["projects"].(map[string]any)
	entry, hasEntry := projects[dir].(map[string]any)
	if v, ok := entry["trust_level"]; ok {
		if v == "trusted" {
			return src, nil
		}
		return nil, ErrCodexTrustConflict
	}
	// A table cannot be extended once written inline. The decoder here is
	// lax about that, and Codex's is not, so the text is checked too.
	if _, ok := old["projects"]; (ok && !hasProjects) || inlineProjects.Match(src) {
		return nil, errors.New("parity: Codex's config writes projects in a way aggy does not edit")
	}
	nl := eol(src)
	key := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(dir) + `"`
	header := "[projects." + key + "]"
	line := `trust_level = "trusted"`
	var out []byte
	if hasEntry {
		// Under its own header, when the table is written that way.
		i := strings.Index(string(src), header)
		if i < 0 {
			return nil, errors.New("parity: Codex's config writes this folder in a way aggy does not edit")
		}
		at := i + len(header)
		if j := strings.IndexByte(string(src[at:]), '\n'); j >= 0 {
			at += j + 1
		} else {
			src = append(src[:at:at], nl...)
			at = len(src)
		}
		out = append(append(append([]byte{}, src[:at]...), line+nl...), src[at:]...)
	} else {
		out = append([]byte{}, src...)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, nl...)
		}
		if len(out) > 0 {
			out = append(out, nl...)
		}
		out = append(out, header+nl+line+nl...)
	}
	got := map[string]any{}
	if err := toml.Unmarshal(out, &got); err != nil {
		return nil, fmt.Errorf("parity: adding this folder would break Codex's config: %w", err)
	}
	want := old
	if !hasProjects {
		projects = map[string]any{}
		want["projects"] = projects
	}
	if !hasEntry {
		entry = map[string]any{}
		projects[dir] = entry
	}
	entry["trust_level"] = "trusted"
	if !reflect.DeepEqual(got, want) {
		return nil, errors.New("parity: adding this folder would change something else in Codex's config")
	}
	return out, nil
}
