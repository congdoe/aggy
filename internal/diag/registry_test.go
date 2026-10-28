package diag

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoRoot is the module root, two levels up from this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// sources returns every non-test Go file of the module, by path relative
// to root, with its text.
func sources(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata" || d.Name() == ".claude") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var (
	constRE   = regexp.MustCompile(`(?m)^\s+((?:Ev|Code)[A-Z]\w*)\s+(Event|Code)\s*=\s*"([^"]+)"`)
	literalRE = regexp.MustCompile(`\b(?:Event|Code):\s*"|\bdiag\.(?:Event|Code)\(\s*"`)
	eventRE   = regexp.MustCompile(`^[a-z]+(\.[a-z]+)?\.[a-z_]+\.[a-z_]+$`)
	codeRE    = regexp.MustCompile(`^[a-z]+\.[a-z_]+$`)
)

// TestRegistry: every event and code constant is registered once, with a
// name that follows the naming rules, a level, a meaning, and a next
// step; every one is used outside the registry; and no code names an
// event or code with a string instead of a constant.
func TestRegistry(t *testing.T) {
	root := repoRoot(t)
	src := sources(t, root)
	reg := src["internal/diag/codes.go"]
	consts := constRE.FindAllStringSubmatch(reg, -1)
	if len(consts) != len(events)+len(codes) {
		t.Errorf("codes.go declares %d constants, the registry has %d entries", len(consts), len(events)+len(codes))
	}
	seen := map[string]bool{}
	for _, c := range consts {
		name, kind, value := c[1], c[2], c[3]
		if seen[value] {
			t.Errorf("%s is registered twice", value)
		}
		seen[value] = true
		if kind == "Event" {
			e, ok := LookupEvent(Event(value))
			switch {
			case !ok:
				t.Errorf("event %s (%s) is not in the registry", value, name)
			case !eventRE.MatchString(value) || !strings.HasPrefix(value, e.Component+"."):
				t.Errorf("event %s: want <component>.<thing>.<what happened> with component %s", value, e.Component)
			case e.Level == "" || e.Meaning == "" || e.Next == "":
				t.Errorf("event %s: level, meaning, and next step are required", value)
			}
		} else {
			c, ok := LookupCode(Code(value))
			switch {
			case !ok:
				t.Errorf("code %s (%s) is not in the registry", value, name)
			case !codeRE.MatchString(value):
				t.Errorf("code %s: want <area>.<reason>", value)
			case c.Meaning == "" || c.Next == "":
				t.Errorf("code %s: meaning and next step are required", value)
			}
		}
		used := false
		for path, text := range src {
			if path == "internal/diag/codes.go" {
				continue
			}
			pat := `\b` + name + `\b`
			if !strings.HasPrefix(path, "internal/diag/") {
				pat = `\bdiag\.` + name + `\b`
			}
			if regexp.MustCompile(pat).MatchString(text) {
				used = true
				break
			}
		}
		if !used {
			t.Errorf("%s (%s) is registered but never used", value, name)
		}
	}
	var bad []string
	for path, text := range src {
		if path == "internal/diag/codes.go" {
			continue
		}
		if literalRE.MatchString(text) {
			bad = append(bad, path)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("events and codes must be named by their diag constants, not strings: %v", bad)
	}
}

// TestRegistryDoc: docs/logging.md's registry tables are the registry.
// AGGY_UPDATE_DOCS=1 rewrites them.
func TestRegistryDoc(t *testing.T) {
	path := filepath.Join(repoRoot(t), "docs", "logging.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const begin, end = "<!-- registry:begin -->\n", "<!-- registry:end -->"
	doc := string(b)
	i, j := strings.Index(doc, begin), strings.Index(doc, end)
	if i < 0 || j < i {
		t.Fatalf("docs/logging.md has no %q ... %q section", begin, end)
	}
	want := doc[:i+len(begin)] + Markdown() + doc[j:]
	if os.Getenv("AGGY_UPDATE_DOCS") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if doc != want {
		t.Error("docs/logging.md's registry tables differ from internal/diag/codes.go; run AGGY_UPDATE_DOCS=1 go test ./internal/diag -run TestRegistryDoc")
	}
}
