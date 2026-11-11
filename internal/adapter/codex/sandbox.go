package codex

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
)

// Codex's own sandbox on screen (T6980, docs/design.md section 10, "The
// vendors' own sandboxes"). Codex runs its shell commands in a sandbox by
// default, so aggy only sees that a flag the person gives does not loosen it
// unasked, and says which level the session starts with.
//
// The levels are SandboxModeCliArg at Codex commit 6aaac69 (rust-v0.164.0,
// the tested version), codex-rs/utils/cli/src/sandbox_mode_cli_arg.rs
// lines 10-16: ReadOnly, WorkspaceWrite, and DangerFullAccess, which clap
// spells read-only, workspace-write, and danger-full-access (rename_all =
// "kebab-case"). The flag is --sandbox, short -s
// (codex-rs/utils/cli/src/shared_options.rs, lines 38-41 at 6aaac69), and
// the config key the same values go in is sandbox_mode. At 6aaac69 there is
// no --full-auto in shared_options.rs; --dangerously-bypass-approvals-and-
// sandbox (alias --yolo) runs "without sandboxing", and --approve-for-me
// (alias --not-so-yolo) sets sandbox_mode="workspace-write" with on-request
// approvals (same file, lines 43-93). Both stay gated as before (PassFlags).
//
// Codex's default, from its docs (learn.chatgpt.com/docs/agent-approvals-
// security, read 2026-11-11; not live-checked at 0.164.0): workspace write
// with on-request approvals in a version-controlled folder, read-only in
// one that is not, and possibly read-only until the folder is trusted;
// network off. A config.toml or a --profile of the person's may set another
// level, which aggy does not read.

// The sandbox levels, from the most to the least confined.
const (
	SandboxReadOnly       = "read-only"
	SandboxWorkspaceWrite = "workspace-write"
	SandboxFullAccess     = "danger-full-access"
)

// ErrSandboxValue matches a sandbox level Codex does not list, refused
// outright: aggy cannot tell whether it loosens.
var ErrSandboxValue = errors.New("it is not one of Codex's sandbox levels (read-only, workspace-write, danger-full-access), so aggy cannot tell what it allows")

// sandboxValue is v as Codex reads a level: a -c value is TOML, so a quoted
// string counts as its contents.
func sandboxValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = v[1 : len(v)-1]
	}
	return v
}

// checkLevel passes read-only and workspace-write, the default level and
// the one tighter than it, gates danger-full-access as a loosening flag
// (asked in place, T6979), and refuses any other value. flag is how the
// refusal names it.
func checkLevel(flag, val string) error {
	switch sandboxValue(val) {
	case SandboxReadOnly, SandboxWorkspaceWrite:
		return nil
	case SandboxFullAccess:
		return fmt.Errorf("%s %s: %w", flag, SandboxFullAccess, adapter.ErrLoosens)
	}
	return fmt.Errorf("%s %q: %w", flag, val, ErrSandboxValue)
}

// checkSandbox is --sandbox and -s.
func checkSandbox(val string) error { return checkLevel("--sandbox", val) }

// SandboxLevel is the level a Codex launch with the CLI words extra starts
// with, as far as aggy can tell, and from: the last --sandbox, -s, or -c
// sandbox_mode (Codex takes the last -c of a key; two --sandbox it refuses
// itself), or --yolo and its spellings, or --approve-for-me. level is "" when
// the words set none, so Codex's own default holds, and from then names a
// --profile when one is given, which may set it. Nothing after "--" counts.
func SandboxLevel(extra []string) (level, from string) {
	profile := ""
	for i := 0; i < len(extra); i++ {
		w := extra[i]
		if w == "--" {
			break
		}
		name, val, hasVal := strings.Cut(w, "=")
		if !strings.HasPrefix(w, "--") && len(w) > 2 {
			name, val, hasVal = w[:2], strings.TrimPrefix(w[2:], "="), true
		}
		next := func() (string, bool) {
			if hasVal {
				return val, true
			}
			if i+1 < len(extra) {
				i++
				return extra[i], true
			}
			return "", false
		}
		switch {
		case name == "--sandbox" || name == "-s":
			if v, ok := next(); ok {
				level, from = sandboxValue(v), "--sandbox"
			}
		case name == "--config" || name == "-c":
			v, ok := next()
			if !ok {
				continue
			}
			key, value, has := strings.Cut(v, "=")
			if has && strings.Trim(strings.TrimSpace(key), `"' `) == "sandbox_mode" {
				level, from = sandboxValue(value), "-c sandbox_mode"
			}
		case name == "--dangerously-bypass-approvals-and-sandbox" || name == "--yolo":
			level, from = SandboxFullAccess, name
		case name == "--approve-for-me" || name == "--not-so-yolo":
			level, from = SandboxWorkspaceWrite, name
		case name == "--profile" || name == "-p":
			if v, ok := next(); ok {
				profile = v
			}
		case slices.Contains([]string{"--model", "-m", "--ask-for-approval", "-a", "--disable", "--enable", "--add-dir", "--image", "-i", "--local-provider"}, name) && !hasVal:
			i++ // its value, which is no flag of these
		}
	}
	if level == "" && profile != "" {
		from = "--profile"
	}
	return level, from
}

// SandboxLine is the launch line for a Codex session's sandbox, from
// SandboxLevel.
func SandboxLine(level, from string) string {
	switch {
	case level == SandboxFullAccess:
		return "Codex's sandbox: off (" + SandboxFullAccess + ", from " + from + "): its shell commands run with your full access"
	case level != "":
		return "Codex's sandbox: " + level + " (from " + from + ")"
	case from == "--profile":
		return "Codex's sandbox: as your --profile or Codex's own default sets it; aggy does not read the profile"
	}
	return "Codex's sandbox: Codex's own default (its docs: workspace-write in a version-controlled folder, read-only elsewhere; your config.toml may set another)"
}
