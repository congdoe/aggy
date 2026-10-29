package checkup

import (
	"path/filepath"
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
	Code   string         // stable, for reports: e.g. "api-key-env"
	Level  Level          //
	Vendor adapter.Vendor // the vendor it concerns, or ""
	// Path is the absolute path the finding is about, or "". It is kept
	// out of Message so each output can show it its own way (Text,
	// ReportText).
	Path string
	// Message is aggy's own text: no secret, env value, or path.
	Message string
	// Detail is free text from a CLI or the system, such as an error,
	// which may hold paths and names; a report leaves it out.
	Detail string
	// Tool is the managed tool it concerns, or "", for aggy tool
	// status's wired column (T6726). No output prints it: a user tool's
	// name is the person's own (H83-1).
	Tool string
}

// Text is the finding for the person: "<path>: <message>: <detail>",
// with the home directory shown as ~. It may hold CLI text: print it
// through termsafe.
func (f Finding) Text(home string) string {
	s := f.Message
	if f.Path != "" {
		s = ShowPath(f.Path, home) + ": " + s
	}
	if f.Detail != "" {
		s += ": " + f.Detail
	}
	return s
}

// ReportText is the finding for a public report: the path as ReportPath
// shows it, the message, and no detail.
func (f Finding) ReportText(home string) string {
	s := f.Message
	if f.Path != "" {
		s = ReportPath(f.Path, home) + ": " + s
	}
	if f.Detail != "" {
		s += " (details left out of the report)"
	}
	return s
}

// ReportPath shows p for a public report: under home as ~/..., anywhere
// else as <path>/<base name>, since a path outside home can hold a user
// or company name (/mnt/c/Users/John Doe/..., /Volumes/Acme Corp/...).
func ReportPath(p, home string) string {
	if s := ShowPath(p, home); s != p {
		return s
	}
	return "<path>/" + filepath.Base(p)
}

// Finding codes. Each is stable, for reports and issues.
const (
	CodeAPIKeyEnv = "api-key-env"        // warning: an API-key variable is set
	CodeCloudEnv  = "cloud-provider-env" // warning: Claude Code is switched to Bedrock or Vertex

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
// ANTHROPIC_API_KEY, OPENAI_API_KEY, and XAI_API_KEY are in
// docs/design.md section 2. CODEX_API_KEY is source-verified (codex
// rust-v0.164.0: codex exec prefers it over any stored login, and it
// bills the API). ANTHROPIC_AUTH_TOKEN is in Claude Code's docs. See the
// table in docs/adapters.md.
var apiKeys = []struct {
	name   string
	vendor adapter.Vendor
}{
	{"ANTHROPIC_API_KEY", adapter.Claude},
	{"ANTHROPIC_AUTH_TOKEN", adapter.Claude},
	{"OPENAI_API_KEY", adapter.Codex},
	{"CODEX_API_KEY", adapter.Codex},
	{"XAI_API_KEY", adapter.Grok},
}

// cloudVars switch Claude Code to a cloud provider's API, billed to that
// account instead of the subscription (Claude Code's docs).
var cloudVars = []struct{ name, provider string }{
	{"CLAUDE_CODE_USE_BEDROCK", "Amazon Bedrock"},
	{"CLAUDE_CODE_USE_VERTEX", "Google Vertex AI"},
}

// cloudVar returns the provider a cloud variable switches Claude Code to.
func cloudVar(name string) (string, bool) {
	for _, c := range cloudVars {
		if c.name == name {
			return c.provider, true
		}
	}
	return "", false
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

// APIKeyEnv warns for each API-key variable (ANTHROPIC_API_KEY,
// ANTHROPIC_AUTH_TOKEN, OPENAI_API_KEY, CODEX_API_KEY, XAI_API_KEY) set
// and non-empty in environ: the vendor CLI may bill the API instead of
// the subscription; and for CLAUDE_CODE_USE_BEDROCK and
// CLAUDE_CODE_USE_VERTEX. It reports the variable's name only; the value
// is never printed, logged, stored, or hashed.
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
	for _, c := range cloudVars {
		if get(c.name) != "" {
			out = append(out, cloudFinding(c.name, c.provider))
		}
	}
	return out
}

func cloudFinding(where, provider string) Finding {
	return Finding{Code: CodeCloudEnv, Level: LevelWarn, Vendor: adapter.Claude,
		Message: where + " is set: Claude Code may use " + provider + ", billed to that account instead of the subscription"}
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
