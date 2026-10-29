package parity

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
)

// The safety policy: never read .env or key files, never push to main,
// no destructive commands. It is a guardrail against an agent's
// mistakes, not a sandbox: every CLI matches these rules against the
// command text or tool path it sees, so `git -C . push origin main`,
// `/bin/rm -rf x`, or a script that opens .env itself gets past them.
// aggy only ever adds denials: nothing here allows a tool, and a repo
// cannot edit these rules. A trusted repo can still switch the Codex
// part off with a rules file Codex cannot parse (OtherCodexRules).

// keyFiles are the file names the policy never reads: .env files and
// private keys by their usual names.
var keyFiles = []string{".env", ".env.*", "*.pem", "*.key", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"}

// bashDeny are the policy's Bash rules, in the syntax Claude Code and
// Grok Build share: a "*" matches any text, spaces included, and a deny
// applies to every segment of a chained command (Claude: permissions.md,
// "Wildcard patterns" and "Compound commands"; Grok: user-guide
// 22-permissions-and-safety.md, "Bash Rules", where a pattern matches as
// a prefix or as a whole-command glob). Grok splits a --deny value on
// commas, so no rule holds one.
var bashDeny = []string{
	// Never push to main: the target is the last word, a refspec ending
	// in :main or :refs/heads/main, or main with options after it.
	"Bash(git push * main)",
	"Bash(git push * main *)",
	"Bash(git push * *:main)",
	"Bash(git push * *:main *)",
	"Bash(git push * refs/heads/main)",
	"Bash(git push * refs/heads/main *)",
	"Bash(git push * *:refs/heads/main)",
	"Bash(git push * *:refs/heads/main *)",
	// No force push: a force flag anywhere, or a +refspec.
	"Bash(git push --force*)",
	"Bash(git push -f*)",
	"Bash(git push * --force*)",
	"Bash(git push * -f*)",
	"Bash(git push * +*)",
	// No recursive forced delete, in the usual spellings.
	"Bash(rm -rf*)",
	"Bash(rm -fr*)",
	"Bash(rm -Rf*)",
	"Bash(rm -fR*)",
	"Bash(rm -r -f*)",
	"Bash(rm -f -r*)",
	"Bash(rm -R -f*)",
	"Bash(rm -f -R*)",
	"Bash(rm -r --force*)",
	"Bash(rm -R --force*)",
	"Bash(rm -f --recursive*)",
	"Bash(rm --recursive -f*)",
	"Bash(rm --force -r*)",
	"Bash(rm --force -R*)",
	"Bash(rm --recursive --force*)",
	"Bash(rm --force --recursive*)",
	// No hard reset, no git clean.
	"Bash(git reset --hard*)",
	"Bash(git reset * --hard*)",
	"Bash(git clean *)",
}

// ClaudeDeny are the policy's deny rules for Claude Code, which takes
// them as permissions.deny in an inline --settings layer (settings.md:
// lists merge across layers; permissions.md: a deny wins over any
// allow). A Read pattern starting with // is anchored at the file-system
// root, so "//**/.env" matches a .env anywhere, not only under the
// working directory (permissions.md, "Read and Edit"); a Read deny also
// covers Edit and Write on the path, and the file commands Claude
// recognizes in Bash, such as cat.
var ClaudeDeny = denyRules("Read(//**/")

// GrokDeny are the policy's deny rules for Grok Build, one --deny flag
// each (grok-build 2bdd1d6a, src/app/cli.rs; user-guide
// 22-permissions-and-safety.md). Grok has no // anchor; an unrooted
// "**/" glob matches anywhere, and a Read deny also covers the paths
// shell commands such as cat name.
var GrokDeny = denyRules("Read(**/")

// RewriteDeny are the policy's deny rules again for a hook that rewrites
// a shell command by putting prefix (such as "rtk ") in front (H78-1).
// Claude matches its rules against the hook's updatedInput, not the
// command the model asked for: a deny on the rewritten command blocked
// it, and a deny on the original let the rewrite run (live, 2026-10-29,
// claude 2.1.312; docs/verification.md). So these copies are required:
// every Bash rule with prefix in front, and for each
// key file a rule matching the prefixed command wherever the name
// appears in it. aggy hook-tool passes a rewrite only as exactly prefix +
// the original command (hook.FilterToolOutput, R80-1), so a Bash rule
// that matches the original matches the rewrite here. The key files are
// covered on the original by Read rules, which also stop the file
// commands Claude recognizes in Bash; that set is not documented, so it
// cannot be copied with the prefix, and the key-file rules match any
// prefixed command that names one, which also refuses harmless ones
// (rtk ls .env). TODO(H78-1): narrow them once that set is recorded.
func RewriteDeny(prefix string) []string {
	out := make([]string, 0, len(keyFiles)+len(bashDeny))
	for _, f := range keyFiles {
		out = append(out, "Bash("+prefix+"*"+strings.Trim(f, "*")+"*)")
	}
	for _, r := range bashDeny {
		out = append(out, "Bash("+prefix+strings.TrimPrefix(r, "Bash("))
	}
	return out
}

func denyRules(readPrefix string) []string {
	out := make([]string, 0, len(keyFiles)+len(bashDeny))
	for _, f := range keyFiles {
		out = append(out, readPrefix+f+")")
	}
	return append(out, bashDeny...)
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
    pattern = ["git", "push", ["origin", "upstream"], ["main", "+main", "HEAD:main", "main:main", "refs/heads/main", "HEAD:refs/heads/main"]],
    decision = "forbidden",
    justification = "aggy safety policy: never push to main. Push a branch and open a pull request.",
)

prefix_rule(
    pattern = ["git", "push", ["-u", "--set-upstream"], ["origin", "upstream"], ["main", "+main", "HEAD:main", "main:main", "refs/heads/main", "HEAD:refs/heads/main"]],
    decision = "forbidden",
    justification = "aggy safety policy: never push to main. Push a branch and open a pull request.",
)

prefix_rule(
    pattern = ["git", "push", ["--force", "-f", "--force-with-lease", "--force-if-includes"]],
    decision = "forbidden",
    justification = "aggy safety policy: no force push.",
)

prefix_rule(
    pattern = ["git", "push", ["origin", "upstream"], ["--force", "-f", "--force-with-lease", "--force-if-includes"]],
    decision = "forbidden",
    justification = "aggy safety policy: no force push.",
)

prefix_rule(
    pattern = ["rm", ["-rf", "-fr", "-Rf", "-fR"]],
    decision = "forbidden",
    justification = "aggy safety policy: no recursive forced delete. Remove the specific files instead.",
)

prefix_rule(
    pattern = ["rm", ["-r", "-R", "--recursive"], ["-f", "--force"]],
    decision = "forbidden",
    justification = "aggy safety policy: no recursive forced delete. Remove the specific files instead.",
)

prefix_rule(
    pattern = ["rm", ["-f", "--force"], ["-r", "-R", "--recursive"]],
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
