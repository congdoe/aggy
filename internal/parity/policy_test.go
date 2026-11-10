package parity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/patch"
)

// TestDenyRulesShape: every rule is a Read or Bash rule in its vendor's
// syntax, and none holds a comma, which Grok's --deny splits on.
func TestDenyRulesShape(t *testing.T) {
	for name, tc := range map[string]struct {
		rules []string
		re    *regexp.Regexp
	}{
		"claude": {ClaudeDeny, regexp.MustCompile(`^(Read\(//\*\*/[^,()]+\)|Bash\([^,()]+\))$`)},
		"grok":   {GrokDeny, regexp.MustCompile(`^(Read\(\*\*/[^,()/]+\)|Bash\([^,()]+\))$`)},
	} {
		seen := map[string]bool{}
		for _, r := range tc.rules {
			if !tc.re.MatchString(r) || seen[r] {
				t.Errorf("%s rule %q", name, r)
			}
			seen[r] = true
		}
	}
}

// bashGlob matches cmd against a Bash(...) rule as a whole-command glob,
// where * matches any text: the semantics both Claude and Grok document.
func bashGlob(rule, cmd string) bool {
	p, ok := strings.CutPrefix(rule, "Bash(")
	if !ok {
		return false
	}
	p = strings.TrimSuffix(p, ")")
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(p), `\*`, ".*") + "$"
	return regexp.MustCompile(re).MatchString(cmd)
}

// codexPrefix matches cmd's words against the patterns in CodexRules: a
// prefix of the words, each word equal to the token or one of its
// alternatives.
func codexPrefix(t *testing.T, cmd string) bool {
	t.Helper()
	words := strings.Fields(cmd)
	for _, m := range regexp.MustCompile(`pattern = (\[.*\]),`).FindAllStringSubmatch(CodexRules, -1) {
		var pat []any
		if err := json.Unmarshal([]byte(m[1]), &pat); err != nil {
			t.Fatalf("pattern %s: %v", m[1], err)
		}
		if len(words) < len(pat) {
			continue
		}
		ok := true
		for i, tok := range pat {
			switch tok := tok.(type) {
			case string:
				ok = ok && words[i] == tok
			case []any:
				hit := false
				for _, alt := range tok {
					hit = hit || words[i] == alt
				}
				ok = ok && hit
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// TestPolicyCoverage holds what the rules stop and what they leave alone,
// as docs/design.md lists it.
func TestPolicyCoverage(t *testing.T) {
	glob := func(cmd string) bool {
		for _, r := range bashDeny {
			if bashGlob(r, cmd) {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct {
		cmd          string
		bash, prefix bool // stopped by Claude and Grok; by Codex
	}{
		{"git push origin main", true, true},
		{"git push -u origin main", true, true},
		{"git push origin +main", true, true},
		{"git push origin HEAD:main", true, true},
		{"git push origin HEAD:refs/heads/main", true, true},
		{"git push fork main", true, false},
		{"git push origin main --force", true, true},
		{"git push --force origin feat", true, true},
		{"git push -f", true, true},
		{"git push --force-with-lease", true, true},
		{"git push origin --force", true, true},
		{"git push origin feat --force", true, false},
		{"git push origin +feat", true, false},
		{"rm -rf build", true, true},
		{"rm -Rf build", true, true},
		{"rm -r -f build", true, true},
		{"rm --recursive --force build", true, true},
		{"rm -f --recursive build", true, true},
		{"rm -rfv build", true, false},
		{"git reset --hard", true, true},
		{"git reset HEAD~1 --hard", true, false},
		{"git clean -fdx", true, true},
		// Left alone: not destructive, or not expressible.
		{"git push", false, false},
		{"git push origin feature", false, false},
		{"git push origin maintenance", false, false},
		{"git push origin feat-fix", false, false},
		{"rm -r build", false, false},
		{"rm my-repo.txt", false, false},
		{"git reset --soft HEAD~1", false, false},
		{"git status", false, false},
	} {
		if got := glob(tc.cmd); got != tc.bash {
			t.Errorf("Claude/Grok stop %q = %v, want %v", tc.cmd, got, tc.bash)
		}
		if got := codexPrefix(t, tc.cmd); got != tc.prefix {
			t.Errorf("Codex stops %q = %v, want %v", tc.cmd, got, tc.prefix)
		}
	}
}

// TestCodexRulesShape holds the rules file to the recorded syntax: every
// rule is a prompt prefix_rule with a justification (T6967: Codex asks on
// screen, and refuses under approval never, as codex exec runs), because a
// file Codex cannot parse makes it drop the person's own rules too. The
// older file aggy still recognizes is every rule forbidden, with the same
// patterns.
func TestCodexRulesShape(t *testing.T) {
	for _, tc := range []struct {
		text, decision string
	}{{CodexRules, "prompt"}, {codexRulesV1, "forbidden"}} {
		rules := strings.Count(tc.text, "prefix_rule(")
		if rules != 9 || strings.Count(tc.text, `decision = "`+tc.decision+`",`) != rules || strings.Count(tc.text, "justification = \"aggy safety policy: ") != rules {
			t.Errorf("rules file shape changed:\n%s", tc.text)
		}
		if strings.Count(tc.text, "(") != strings.Count(tc.text, ")") || strings.Count(tc.text, "[") != strings.Count(tc.text, "]") {
			t.Error("unbalanced brackets")
		}
		for _, l := range strings.Split(tc.text, "\n") {
			if strings.Contains(l, `decision = "allow"`) {
				t.Errorf("a rule that allows: %s", l)
			}
		}
	}
	patterns := func(s string) []string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if strings.Contains(l, "pattern = ") {
				out = append(out, l)
			}
		}
		return out
	}
	if !slices.Equal(patterns(CodexRules), patterns(codexRulesV1)) {
		t.Error("the asking file covers other commands than the refusing one")
	}
}

// TestCodexRulesOld (T6967): aggy's older, refusing file still counts as
// wired for a launch, and aggy policy wire replaces it, and only it.
func TestCodexRulesOld(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	codex := t.TempDir()
	t.Setenv("CODEX_HOME", codex)
	path, _ := CodexRulesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(codexRulesV1), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := CodexRulesState(path); st != RulesOld || err != nil {
		t.Fatalf("state %q, %v", st, err)
	}
	p, err := CodexRulesPatch(path)
	if err != nil || p.ExpectedHash != patch.Hash([]byte(codexRulesV1)) {
		t.Fatalf("%+v, %v", p, err)
	}
	if _, err := patch.Apply(p, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != CodexRules {
		t.Errorf("not updated: %s", b)
	}
}

func TestCodexRulesPatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	codex := t.TempDir()
	t.Setenv("CODEX_HOME", codex)
	path, err := CodexRulesPath()
	if err != nil || path != filepath.Join(codex, "rules", "aggy.rules") {
		t.Fatalf("path %s, %v", path, err)
	}
	if st, err := CodexRulesState(path); st != RulesMissing || err != nil {
		t.Errorf("state %q, %v", st, err)
	}
	p, err := CodexRulesPatch(path)
	if err != nil || p.ExpectedHash != patch.NoFile {
		t.Fatalf("%+v, %v", p, err)
	}
	if _, err := patch.Apply(p, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if st, _ := CodexRulesState(path); st != RulesWired {
		t.Errorf("state after apply %q", st)
	}
	if p, err := CodexRulesPatch(path); err != nil {
		t.Error(err)
	} else if pl, err := patch.Plan(p); err != nil || pl.Action != patch.NoOp {
		t.Errorf("rewire: %+v, %v", pl, err)
	}
	// A file of the person's is never replaced.
	if err := os.WriteFile(path, []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := CodexRulesState(path); st != RulesOther {
		t.Errorf("state %q", st)
	}
	if _, err := CodexRulesPatch(path); err == nil {
		t.Error("a file aggy did not write would be replaced")
	}
	// A symlinked file or rules folder is refused.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(codex, "elsewhere"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := CodexRulesPatch(path); err == nil {
		t.Error("symlinked rules file accepted")
	}
	if err := os.RemoveAll(filepath.Join(codex, "rules")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(codex, "rules")); err != nil {
		t.Fatal(err)
	}
	if _, err := CodexRulesPatch(path); err == nil {
		t.Error("symlinked rules folder accepted")
	}
}

// TestRewriteDeny (H78-1): the rules for a hook that rewrites with "rtk "
// cover the prefixed forms of what the policy denies, and stay in
// Claude's rule shape.
func TestRewriteDeny(t *testing.T) {
	rules := RewriteDeny("rtk ")
	if len(rules) != len(keyFiles)+len(bashDeny) || rules[0] != "Bash(rtk *.env*)" || !slices.Contains(rules, "Bash(rtk git push --force*)") {
		t.Fatalf("rules = %q", rules)
	}
	re := regexp.MustCompile(`^Bash\(rtk [^,()]+\)$`)
	for _, r := range rules {
		if !re.MatchString(r) {
			t.Errorf("rule %q", r)
		}
	}
	denied := func(cmd string) bool {
		return slices.ContainsFunc(rules, func(r string) bool { return bashGlob(r, cmd) })
	}
	for _, cmd := range []string{"rtk git push --force origin x", "rtk git push origin main", "rtk rm -rf /", "rtk git reset --hard",
		"rtk read .env", "rtk grep KEY .env.local", "rtk cat ~/.ssh/id_rsa", "rtk head server.pem"} {
		if !denied(cmd) {
			t.Errorf("%q is not denied", cmd)
		}
	}
	for _, cmd := range []string{"rtk git status", "rtk ls src", "git push --force"} {
		if denied(cmd) {
			t.Errorf("%q is denied", cmd)
		}
	}
}
