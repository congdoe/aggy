package parity

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/patch"
)

// TestDenyRulesShape: every rule is a Read or Bash rule Claude and Grok
// both take, and none holds a comma, which Grok's --deny splits on.
func TestDenyRulesShape(t *testing.T) {
	re := regexp.MustCompile(`^(Read\(\*\*/[^,()]+\)|Bash\([^,()]+\))$`)
	seen := map[string]bool{}
	for _, r := range DenyRules {
		if !re.MatchString(r) || seen[r] {
			t.Errorf("rule %q", r)
		}
		seen[r] = true
	}
}

// TestCodexRulesShape holds the rules file to the recorded syntax: every
// rule is a forbidden prefix_rule with a justification, because a file
// Codex cannot parse makes it drop the person's own rules too.
func TestCodexRulesShape(t *testing.T) {
	rules := strings.Count(CodexRules, "prefix_rule(")
	if rules != 5 || strings.Count(CodexRules, `decision = "forbidden",`) != rules || strings.Count(CodexRules, "justification = \"aggy safety policy: ") != rules {
		t.Errorf("rules file shape changed:\n%s", CodexRules)
	}
	if strings.Count(CodexRules, "(") != strings.Count(CodexRules, ")") || strings.Count(CodexRules, "[") != strings.Count(CodexRules, "]") {
		t.Error("unbalanced brackets")
	}
	for _, l := range strings.Split(CodexRules, "\n") {
		if strings.Contains(l, `decision = "allow"`) || strings.Contains(l, `"prompt"`) {
			t.Errorf("a rule that is not a denial: %s", l)
		}
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
