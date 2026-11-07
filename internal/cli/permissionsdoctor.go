package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/safefs"
)

// Finding codes of doctor's permissions findings.
const (
	codePermissionsFile = "permissions-file" // warning: a mistake in permissions.json
	codeConfigFile      = "config-file"      // warning: a mistake in config.toml
	codeSuggestFormat   = "suggest-format"   // warning: Claude's session files seem to have changed
)

// permissionFindings reports mistakes in the files people edit by hand, by line,
// and whether the session-file scan still recognizes what it reads. A message
// holds only a line number and aggy's words: a rule or a key goes in Detail,
// which doctor --report leaves out.
func permissionFindings(ctx context.Context) []checkup.Finding {
	var out []checkup.Finding
	add := func(code, path string, line int, msg, detail string) {
		if line > 0 {
			msg = fmt.Sprintf("line %d: %s", line, msg)
		}
		out = append(out, checkup.Finding{Code: code, Level: checkup.LevelWarn, Path: path, Message: msg, Detail: detail})
	}
	if path, err := permissionsFile(); err == nil {
		b, err := safefs.ReadConfig(path, permissions.MaxFile)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			add(codePermissionsFile, path, 0, "aggy cannot read this file, so its rules do not apply", err.Error())
		default:
			for _, e := range permissions.CheckFile(b) {
				add(codePermissionsFile, path, e.Line, e.Msg, e.Detail)
			}
		}
	}
	if path, err := config.Path(); err == nil {
		b, err := safefs.ReadConfig(path, config.MaxFileSize)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			add(codeConfigFile, path, 0, "aggy cannot read its config", err.Error())
		default:
			for _, e := range config.CheckFile(string(b)) {
				add(codeConfigFile, path, e.Line, "a mistake in aggy's config", e.Msg)
			}
		}
	}
	if moved, err := suggestFormatMoved(ctx); err == nil && moved {
		out = append(out, checkup.Finding{Code: codeSuggestFormat, Level: checkup.LevelWarn,
			Message: "Claude's session files show shell calls with no permission mode, so aggy permissions suggest finds nothing; their format may have changed"})
	}
	return out
}
