package execrun

import (
	"fmt"
	"strings"
)

// toolPass are the variables a managed tool's MCP server gets from the
// environment aggy tool exec was started with: passEnv without the vendor
// CLIs' homes, which a tool has no use for. No npm_config_*, uv, or pip
// variable is set or passed: a package runner reads the user's own config
// (~/.npmrc, ~/.config/uv) through HOME, and finds no project config in
// the private folder the server runs in.
var toolPass = []string{"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE"}

// ToolEnv builds a tool server's environment from scratch (W71-2): the
// toolPass variables from base (PATH without empty, ".", or relative
// entries), then each variable in names that base sets. names are the
// variables the tool's definition declares, which internal/tooldef has
// limited to the tool's own (<NAME>_...) and checked against secrets and
// loader injection; a name it would refuse here is an error all the same.
// The values are read now, from base, and never written to a file.
func ToolEnv(base, names []string) ([]string, error) {
	for _, n := range names {
		if n == "" || strings.ContainsAny(n, "=\x00") || contains(toolPass, n) || strings.HasPrefix(n, "AGGY_") {
			return nil, fmt.Errorf("execrun: refusing to pass %q to a tool", n)
		}
	}
	var env []string
	for _, kv := range base {
		k, v, ok := strings.Cut(kv, "=")
		switch {
		case !ok:
		case contains(toolPass, k):
			if k == "PATH" {
				if v = cleanPath(v); v == "" {
					continue
				}
			}
			env = append(env, k+"="+v)
		case contains(names, k):
			env = append(env, kv)
		}
	}
	return env, nil
}
