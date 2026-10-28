package adapter

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/safefs"
)

// Helpers shared by the vendor adapters.

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether s is a UUID, the form --session-id takes on
// Claude and Grok.
func IsUUID(s string) bool { return uuidRE.MatchString(s) }

// CheckAbs returns an error unless p is a clean absolute path. what names
// the field for the message.
func CheckAbs(what, p string) error {
	if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("adapter: %s must be a clean absolute path, got %q", what, p)
	}
	return nil
}

// BriefPrompt is the first message of an interactive session that has a
// brief: a short pointer to the file, never the brief itself, so argv
// stays small whatever the brief's size.
func BriefPrompt(path string) string {
	return "Read the handoff brief at " + path + " before anything else, then continue the work it describes."
}

// ClassifyName maps a vendor's error class name onto aggy's ErrorClass.
// "rate_limit" is recorded for the Claude and Grok StopFailure hook, and
// Grok says its classes mirror Claude Code's. The other vendor names
// (authentication_failed, billing_error, server_error, error_max_turns,
// ...) are unconfirmed; aggy's own class names are accepted too. An empty
// name is ErrNone and anything else is ErrUnknown.
func ClassifyName(name string) ErrorClass {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		return ErrNone
	case "rate_limit", "rate_limit_error", "usage_limit_reached":
		return ErrRateLimit
	case "auth", "authentication_failed", "authentication_error":
		return ErrAuth
	case "billing", "billing_error":
		return ErrBilling
	case "server", "server_error", "overloaded", "overloaded_error", "api_error":
		return ErrServer
	case "invalid_request", "invalid_request_error":
		return ErrInvalidRequest
	case "max_turns", "error_max_turns":
		return ErrMaxTurns
	case "timeout":
		return ErrTimeout
	}
	return ErrUnknown
}

// ListFiles returns the regular files directly in dir whose extension
// (lower-cased, with the dot) is in exts, sorted by name, as kind
// artifacts. With no exts it returns every entry but directories,
// symlinks and other odd files included, for the borrow's artifact gate
// to refuse with a reason. A missing dir is no files. It reads through
// safefs, never follows a symlink, and never copies anything.
func ListFiles(dir, kind string, exts ...string) ([]Artifact, error) {
	entries, err := safefs.ReadDir(dir)
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Artifact
	for _, e := range entries {
		if len(exts) == 0 {
			if e.IsDir() {
				continue
			}
		} else if !e.Type().IsRegular() || !slices.Contains(exts, strings.ToLower(filepath.Ext(e.Name()))) {
			continue
		}
		out = append(out, Artifact{Path: filepath.Join(dir, e.Name()), Kind: kind})
	}
	slices.SortFunc(out, func(a, b Artifact) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// SubDirs returns the names of the directories directly in dir. A missing
// dir has none.
func SubDirs(dir string) ([]string, error) {
	entries, err := safefs.ReadDir(dir)
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

// ValidSessionID returns an error unless id is a session id that may be
// put in argv for vendor v. It refuses anything empty, anything starting
// with "-" (a CLI would parse it as a flag), and any character outside
// [A-Za-z0-9-]. Claude and Grok ids are UUIDs (--session-id takes one).
// The Codex thread-id format is not recorded, so Codex ids must be UUIDs
// too until it is. Every id an adapter puts in argv, and every id
// ParseHeadless returns, passes through it.
func ValidSessionID(v Vendor, id string) error {
	switch {
	case id == "":
		return fmt.Errorf("adapter: empty %s session id", v)
	case strings.HasPrefix(id, "-"):
		return fmt.Errorf("adapter: %s session id %q starts with -", v, id)
	}
	for _, r := range id {
		if !strings.ContainsRune(idChars, r) {
			return fmt.Errorf("adapter: %s session id %q has a character outside [A-Za-z0-9-]", v, id)
		}
	}
	if !IsUUID(id) {
		return fmt.Errorf("adapter: %s session id %q is not a UUID", v, id)
	}
	return nil
}

// idChars are the characters a session id may hold.
const idChars = "-0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// ParsedSessionID checks an id a vendor CLI reported against the one aggy
// pre-assigned or resumed (empty when none): a pre-assigned id is kept, and a
// different reported id is an error, as is an invalid one. It returns the
// id to record.
func ParsedSessionID(v Vendor, assigned, reported string) (string, error) {
	if reported == "" {
		return assigned, nil
	}
	if err := ValidSessionID(v, reported); err != nil {
		return assigned, fmt.Errorf("%w (reported by the CLI)", err)
	}
	if assigned != "" && !strings.EqualFold(assigned, reported) {
		return assigned, fmt.Errorf("adapter: %s reported session %s, but aggy expected %s", v, reported, assigned)
	}
	if assigned != "" {
		return assigned, nil
	}
	return reported, nil
}

// ErrWebSearchKind is the reason web search is refused outside research
// and news.
const ErrWebSearchKind = "web search is only for research and news: a child that can read files must not also have web egress"

// CheckWebSearch returns ErrUnsupported when req asks for web search in a
// kind other than research or news.
func CheckWebSearch(v Vendor, req HeadlessRequest) error {
	if req.WebSearch && req.Kind != Research && req.Kind != News {
		return fmt.Errorf("%s: %s: %w: %s", v, req.Kind, ErrUnsupported, ErrWebSearchKind)
	}
	return nil
}

// CheckIsolated returns an error when a research or news request does not
// vouch for an isolated working dir (see HeadlessRequest.IsolatedCwd).
func CheckIsolated(v Vendor, req HeadlessRequest) error {
	if (req.Kind == Research || req.Kind == News) && !req.IsolatedCwd {
		return fmt.Errorf("%s: %s needs a fresh, empty temp working dir (IsolatedCwd)", v, req.Kind)
	}
	return nil
}

var modelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]*$`)

// ValidModel returns an error unless model is safe to pass as a flag
// value: it may not start with "-" and holds only letters, digits, and
// . _ : / [ ] -. An empty model means no model flag and is valid.
func ValidModel(model string) error {
	if model != "" && !modelRE.MatchString(model) {
		return fmt.Errorf("adapter: model %q is not a model name", model)
	}
	return nil
}

// ErrLockdownRejected matches a headless run whose CLI rejected or
// ignored one of the lockdown flags. The borrow fails closed: aggy never
// retries without the flag.
var ErrLockdownRejected = errors.New("the CLI rejected a lockdown flag")

// Rejection formats: whole lines, matched per vendor.
var (
	// commander's unknown option and invalid choice (Claude Code is a
	// commander CLI).
	commanderRejected = []*regexp.Regexp{
		regexp.MustCompile(`^error: unknown option '[^']+'`),
		regexp.MustCompile(`^error: option '[^']+' argument '[^']*' is invalid\.`),
	}
	// clap's unknown argument and invalid value (Codex and Grok are clap
	// CLIs; Grok: grok-build 2bdd1d6, xai-grok-pager/src/app/cli.rs:2).
	clapRejected = []*regexp.Regexp{
		regexp.MustCompile(`^error: unexpected argument '[^']+' found`),
		regexp.MustCompile(`^error: invalid value '[^']*' for '[^']+'`),
	}
	// Codex's --strict-config message for a feature it does not know
	// (config/src/loader/mod.rs:264,671 at 6aaac69), at the start of a
	// stderr line. codex exec prints it as "Error loading config.toml:
	// <err>" (exec/src/lib.rs:847); the bare and "Error: " forms are kept.
	codexUnknownField = regexp.MustCompile("^(?:Error loading config\\.toml: |[Ee]rror: )?unknown configuration field `[^`]+` in -c/--config override")
)

// rejected returns the formats vendor v prints for a flag it refuses.
func rejected(v Vendor) []*regexp.Regexp {
	if v == Claude {
		return commanderRejected
	}
	return clapRejected
}

// CheckLockdown returns an error matching ErrLockdownRejected when a run
// exited non-zero and a line of its stderr or stdout is v's own message
// for a refused option: one of the lockdown flags aggy depends on may be
// gone from this version. Only the vendor's exact formats count, so an
// unrelated failure that mentions "invalid value" keeps its own class.
func CheckLockdown(v Vendor, out Output) error {
	if out.ExitCode == 0 {
		return nil
	}
	for i, stream := range [][]byte{out.Stderr, out.Stdout} {
		pats := rejected(v)
		if v == Codex && i == 0 {
			// The --strict-config message goes to stderr only.
			pats = append(slices.Clone(pats), codexUnknownField)
		}
		for _, line := range strings.Split(string(stream), "\n") {
			line = strings.TrimSpace(line)
			for _, re := range pats {
				if re.MatchString(line) {
					return fmt.Errorf("%s: %w: %s", v, ErrLockdownRejected, line)
				}
			}
		}
	}
	return nil
}
