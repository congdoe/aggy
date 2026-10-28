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
// artifacts. A missing dir is no files. It reads through safefs, never
// follows a symlink, and never copies anything.
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
		if !e.Type().IsRegular() {
			continue
		}
		if !slices.Contains(exts, strings.ToLower(filepath.Ext(e.Name()))) {
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
