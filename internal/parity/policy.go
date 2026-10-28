package parity

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
)

// The safety policy: never read .env or key files, never push to main,
// no destructive commands. It is a guardrail against an agent's
// mistakes, not a sandbox: every CLI matches these rules against the
// command text or tool path it sees, so `git -C . push origin main`,
// `/bin/rm -rf x`, or a script that opens .env itself gets past them.
// aggy only ever adds denials: nothing here allows a tool, and a repo
// cannot change the policy.

// DenyRules are the policy's deny rules in the syntax Claude Code and
// Grok Build share. Claude takes them as permissions.deny in an inline
// --settings layer (code.claude.com/docs settings.md: lists merge across
// layers; permissions.md: Read rules use gitignore patterns, so
// "**/.env" matches at any depth under the working directory, and a "*"
// in a Bash rule matches any text, spaces included). Grok takes each as a
// --deny flag (grok-build 2bdd1d6a, src/app/cli.rs and user-guide
// 22-permissions-and-safety.md: deny wins over every allow, from any
// source; an unrooted "**/" glob matches anywhere; a Bash pattern is a
// prefix or a whole-command glob). Grok splits a --deny value on commas,
// so no rule holds one.
var DenyRules = []string{
	// Never read .env or key files.
	"Read(**/.env)",
	"Read(**/.env.*)",
	"Read(**/*.pem)",
	"Read(**/*.key)",
	"Read(**/id_rsa)",
	"Read(**/id_dsa)",
	"Read(**/id_ecdsa)",
	"Read(**/id_ed25519)",
	// Never push to main.
	"Bash(git push * main)",
	"Bash(git push * main *)",
	"Bash(git push * *:main)",
	"Bash(git push * *:main *)",
	// No destructive commands.
	"Bash(rm -rf *)",
	"Bash(rm -fr *)",
	"Bash(git reset --hard*)",
	"Bash(git clean *)",
	"Bash(git push --force*)",
	"Bash(git push -f*)",
	"Bash(git push * --force*)",
	"Bash(git push * -f*)",
}

// CodexRules is the policy as a Codex exec-policy rules file. Codex
// 0.164.0 loads every regular *.rules file in <config folder>/rules of
// each config layer, the user layer being $CODEX_HOME
// (core/src/exec_policy.rs, load_exec_policy and collect_policy_files),
// and takes the strictest decision of every rule that matches a command,
// forbidden first (execpolicy/README.md), so a rule here can only make a
// command stricter. Syntax: prefix_rule(pattern, decision, justification)
// from execpolicy/README.md and execpolicy/src/parser.rs; a list in a
// pattern is a set of alternatives. Prefix rules cannot match a word in
// the middle, so "push to main" covers only the remotes origin and
// upstream, and Codex has no rule for reading a file: .env is not
// covered here.
//
// A rules file Codex cannot parse makes it drop every rules file, the
// person's own included (load_exec_policy_with_warning), so this text is
// fixed and held by a golden test.
const CodexRules = `# Written by aggy: its safety policy (aggy policy wire). aggy unwire
# removes this file. A guardrail against mistakes, not a sandbox.

prefix_rule(
    pattern = ["git", "push", ["origin", "upstream"], ["main", "HEAD:main", "main:main"]],
    decision = "forbidden",
    justification = "aggy safety policy: never push to main. Push a branch and open a pull request.",
)

prefix_rule(
    pattern = ["git", "push", ["--force", "-f", "--force-with-lease"]],
    decision = "forbidden",
    justification = "aggy safety policy: no force push.",
)

prefix_rule(
    pattern = ["rm", ["-rf", "-fr"]],
    decision = "forbidden",
    justification = "aggy safety policy: no recursive forced delete. Remove the specific files instead.",
)

prefix_rule(
    pattern = ["git", "reset", "--hard"],
    decision = "forbidden",
    justification = "aggy safety policy: no hard reset. Use git stash or git restore on specific files.",
)

prefix_rule(
    pattern = ["git", "clean"],
    decision = "forbidden",
    justification = "aggy safety policy: git clean deletes untracked files. Remove the specific files instead.",
)
`

// CodexRulesName is the file aggy owns in Codex's rules folder.
const CodexRulesName = "aggy.rules"

// CodexRulesPath returns $CODEX_HOME/rules/aggy.rules, or
// ~/.codex/rules/aggy.rules.
func CodexRulesPath() (string, error) {
	cfg, err := CodexConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfg), "rules", CodexRulesName), nil
}

// Rules file states.
const (
	RulesWired   = "wired"   // the file holds CodexRules
	RulesMissing = "missing" // no file
	RulesOther   = "other"   // a file aggy did not write, or an older policy
)

// CodexRulesState reports what is at path. It reads nothing but that
// file and its folder's metadata.
func CodexRulesState(path string) (string, error) {
	if fi, err := safefs.Lstat(filepath.Dir(path)); err == nil && (fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir()) {
		return "", fmt.Errorf("parity: %s is not a real directory", filepath.Dir(path))
	}
	b, err := safefs.ReadFileLimit(path, 64<<10)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return RulesMissing, nil
	case errors.Is(err, safefs.ErrTooLarge):
		return RulesOther, nil
	case err != nil:
		return "", err
	case bytes.Equal(b, []byte(CodexRules)):
		return RulesWired, nil
	}
	return RulesOther, nil
}

// CodexRulesPatch returns the patch that creates the rules file at path,
// to apply through a manifest so aggy unwire removes it. It never
// replaces a file: one already holding CodexRules gives a patch that
// changes nothing, and any other file is refused.
func CodexRulesPatch(path string) (patch.Patch, error) {
	st, err := CodexRulesState(path)
	if err != nil {
		return patch.Patch{}, err
	}
	h := patch.NoFile
	switch st {
	case RulesOther:
		return patch.Patch{}, fmt.Errorf("parity: %s exists and is not aggy's current policy; aggy never replaces it (undo the old one with aggy unwire, or move the file away)", path)
	case RulesWired:
		h = patch.Hash([]byte(CodexRules))
	}
	return patch.Patch{File: path, ExpectedHash: h, Mode: 0o600, Transform: func(src []byte) ([]byte, error) {
		if src != nil && !bytes.Equal(src, []byte(CodexRules)) {
			return nil, fmt.Errorf("parity: %s is not aggy's", path)
		}
		return []byte(CodexRules), nil
	}}, nil
}
