package checkup

import (
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
)

// Level is how serious a finding is. aggy doctor exits non-zero when any
// finding is LevelError.
type Level int

// Levels.
const (
	// LevelInfo is a fact worth showing that needs no action.
	LevelInfo Level = iota
	// LevelWarn may cost money or behave unexpectedly, but aggy works.
	LevelWarn
	// LevelError means aggy cannot work as set up until it is fixed.
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelWarn:
		return "warning"
	case LevelError:
		return "error"
	}
	return "info"
}

// Finding is one thing a check found. It names variables, files, and
// keys, never their values.
type Finding struct {
	Code    string         // stable, for reports: e.g. "api-key-env"
	Level   Level          //
	Vendor  adapter.Vendor // the vendor it concerns, or ""
	Message string         // for the person; holds no secret or env value
}

// Finding codes. Each is stable, for reports and issues.
const (
	CodeAPIKeyEnv = "api-key-env" // warning: an API-key variable is set

	CodeCLIFound        = "cli-found"            // info: installed, with its version
	CodeCLIMissing      = "cli-missing"          // info: not on PATH
	CodeNoCLI           = "no-cli"               // error: no vendor CLI is installed
	CodeProbeFailed     = "cli-probe-failed"     // error: installed, but --version failed
	CodeVersionUntested = "version-untested"     // warning: outside the tested range, or unreadable
	CodeSignedIn        = "signed-in"            // info
	CodeSignedOut       = "signed-out"           // warning: the CLI reports it is not signed in
	CodeSignInUnknown   = "sign-in-unknown"      // info: no verified way to ask
	CodeSignInFailed    = "sign-in-check-failed" // warning: the sign-in check did not run
)

// apiKeys maps each API-key variable to the vendor CLI it can switch to
// API billing.
var apiKeys = []struct {
	name   string
	vendor adapter.Vendor
}{
	{"ANTHROPIC_API_KEY", adapter.Claude},
	{"OPENAI_API_KEY", adapter.Codex},
	{"XAI_API_KEY", adapter.Grok},
}

// envLookup returns a lookup over environ (KEY=value entries). The last
// entry for a key wins, as in a process environment.
func envLookup(environ []string) func(string) string {
	m := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return func(k string) string { return m[k] }
}

// APIKeyEnv warns for each of ANTHROPIC_API_KEY, OPENAI_API_KEY, and
// XAI_API_KEY that is set and non-empty in environ: the vendor CLI may
// bill the API instead of the subscription. It reports the variable's
// name only; the value is never printed, logged, stored, or hashed.
func APIKeyEnv(environ []string) []Finding {
	get := envLookup(environ)
	var out []Finding
	for _, k := range apiKeys {
		if get(k.name) == "" {
			continue
		}
		out = append(out, Finding{
			Code: CodeAPIKeyEnv, Level: LevelWarn, Vendor: k.vendor,
			Message: k.name + " is set: the CLI may bill the API instead of the subscription",
		})
	}
	return out
}

// Environment returns every environment and config-file finding: API-key
// variables, base-URL variables, and routes set in vendor config files.
func Environment(environ []string) []Finding {
	out := APIKeyEnv(environ)
	out = append(out, RouteEnv(environ)...)
	return append(out, RouteFiles(environ)...)
}

// ForVendor returns the findings about v.
func ForVendor(fs []Finding, v adapter.Vendor) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Vendor == v {
			out = append(out, f)
		}
	}
	return out
}
