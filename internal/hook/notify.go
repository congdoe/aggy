package hook

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Codex's legacy notify: `notify = [argv...]` at the top level of
// config.toml. Codex runs the argv with the agent-turn-complete JSON
// appended as one last argument, stdin, stdout, and stderr null, and
// does not wait (codex-rs/hooks/src/legacy_notify.rs#L13-L68 at
// 6aaac694). It is a single command, so aggy only adds it where the
// person has none.

// NotifyArgv is the notify argv that calls bin.
func NotifyArgv(bin string) []string {
	return []string{bin, "hook", "--vendor", "codex", "notify"}
}

// ErrNotifyTaken means config.toml already sets a notify that is not
// aggy's. Codex runs one, so aggy leaves it alone.
var ErrNotifyTaken = errors.New("config.toml already sets notify; Codex runs only one, so aggy leaves it alone")

// NotifyMissing reports whether Codex's config.toml src lacks aggy's
// notify for bin. A notify set to anything else, or a file that is not
// valid TOML, is an error (ErrNotifyTaken, ErrAmbiguous): aggy never
// replaces a notify.
func NotifyMissing(src []byte, bin string) (bool, error) {
	if err := CheckBin(bin); err != nil {
		return false, err
	}
	doc := map[string]any{}
	if _, err := toml.Decode(string(src), &doc); err != nil {
		return false, ambiguous("config.toml does not parse: %v", err)
	}
	cur, ok := doc["notify"]
	if !ok {
		return true, nil
	}
	if argv, ok := tomlStrings(cur); ok && slices.Equal(argv, NotifyArgv(bin)) {
		return false, nil
	}
	return false, ErrNotifyTaken
}

// tomlStrings returns a TOML array of strings.
func tomlStrings(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(list))
	for i, e := range list {
		if out[i], ok = e.(string); !ok {
			return nil, false
		}
	}
	return out, true
}

// AddNotify returns a patch transform that puts aggy's notify line first
// in config.toml (top-level keys must come before any table), in the
// file's line ending, and checks the result parses to the old document
// plus notify. With the notify already in place it returns src as is.
func AddNotify(bin string) func([]byte) ([]byte, error) {
	return func(src []byte) ([]byte, error) {
		missing, err := NotifyMissing(src, bin)
		if err != nil || !missing {
			return src, err
		}
		eol := "\n"
		if bytes.Contains(src, []byte("\r\n")) {
			eol = "\r\n"
		}
		quoted := make([]string, 0, 5)
		for _, a := range NotifyArgv(bin) {
			quoted = append(quoted, "'"+a+"'") // a TOML literal string; CheckBin refused quotes and control characters
		}
		out := append([]byte("notify = ["+strings.Join(quoted, ", ")+"]"+eol), src...)
		want := map[string]any{}
		_, _ = toml.Decode(string(src), &want)
		want["notify"] = anys(NotifyArgv(bin))
		got := map[string]any{}
		if _, err := toml.Decode(string(out), &got); err != nil || !reflect.DeepEqual(got, want) {
			return nil, ambiguous("cannot add notify to config.toml safely")
		}
		return out, nil
	}
}

func anys(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
