package diag

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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

// tok is one token of a Go file; comments are skipped.
type tok struct {
	t   token.Token
	lit string
}

func tokens(t *testing.T, name, src string) []tok {
	t.Helper()
	fset := token.NewFileSet()
	file := fset.AddFile(name, -1, len(src))
	var s scanner.Scanner
	s.Init(file, []byte(src), func(pos token.Position, msg string) { t.Errorf("%s: %s", pos, msg) }, 0)
	var out []tok
	for {
		_, tk, lit := s.Scan()
		if tk == token.EOF {
			return out
		}
		if tk == token.IDENT || tk == token.PERIOD || tk == token.LBRACE || tk == token.RBRACE {
			out = append(out, tok{tk, lit})
		} else {
			out = append(out, tok{tk, ""})
		}
	}
}

// uses counts, per identifier, the references to the registry's
// accessors in code (never in comments): diag.X outside the package, X
// inside it. It also returns where package diag builds an Event or Code
// from a value outside codes.go (Event{} and Code{}, the zero values, are
// allowed).
func uses(t *testing.T, src map[string]string) (map[string]int, []string) {
	t.Helper()
	n := map[string]int{}
	var built []string
	for path, text := range src {
		if path == "internal/diag/codes.go" {
			continue
		}
		inPkg := strings.HasPrefix(path, "internal/diag/")
		ts := tokens(t, path, text)
		for i, x := range ts {
			if x.t != token.IDENT {
				continue
			}
			switch {
			case !inPkg && x.lit == "diag" && i+2 < len(ts) && ts[i+1].t == token.PERIOD && ts[i+2].t == token.IDENT:
				n[ts[i+2].lit]++
			case inPkg && (i == 0 || ts[i-1].t != token.PERIOD):
				n[x.lit]++
				if (x.lit == "Event" || x.lit == "Code") && builds(ts[i+1:]) {
					built = append(built, path)
				}
			}
		}
	}
	return n, built
}

// builds reports whether the tokens after an Event or Code are a
// composite literal with a value: {"x"}, {s}, or {name: s}. A function
// body after a result type (func f() Event {) is not one.
func builds(ts []tok) bool {
	if len(ts) < 3 || ts[0].t != token.LBRACE {
		return false
	}
	return ts[1].t == token.STRING || ts[1].t == token.IDENT && (ts[2].t == token.RBRACE || ts[2].t == token.COLON)
}

var (
	varRE = regexp.MustCompile(`(?m)^func ((?:Ev|Code)[A-Z]\w*)\(\) (Event|Code)\s+\{ return (?:Event|Code)\{"([^"]+)"\} \}$`)
	// sealRE finds a registry entry declared as a variable, which any
	// package could reassign.
	sealRE  = regexp.MustCompile(`(?m)^\s*(?:var\s+)?(?:Ev|Code)[A-Z]\w*\s*(?:Event|Code)?\s*=`)
	eventRE = regexp.MustCompile(`^[a-z]+(\.[a-z]+)?\.[a-z_]+\.[a-z_]+$`)
	codeRE  = regexp.MustCompile(`^[a-z]+\.[a-z_]+$`)
)

// TestRegistry: every event and code accessor is registered once, with a
// name that follows the naming rules, a level, a meaning, and a next
// step; every one is used in code outside the registry (a mention in a
// comment does not count); and nothing outside codes.go builds an Event
// or Code from a value. Elsewhere, the type system refuses one: the
// field is unexported.
func TestRegistry(t *testing.T) {
	src := sources(t, repoRoot(t))
	vars := varRE.FindAllStringSubmatch(src["internal/diag/codes.go"], -1)
	if len(vars) != len(events)+len(codes) {
		t.Errorf("codes.go declares %d accessors, the registry has %d entries", len(vars), len(events)+len(codes))
	}
	if m := sealRE.FindAllString(src["internal/diag/codes.go"], -1); len(m) > 0 {
		t.Errorf("registry entries must be accessor functions, not variables: %q", m)
	}
	used, built := uses(t, src)
	if len(built) > 0 {
		t.Errorf("only codes.go may build an Event or Code: %v", built)
	}
	seen := map[string]bool{}
	for _, v := range vars {
		name, kind, value := v[1], v[2], v[3]
		if seen[value] {
			t.Errorf("%s is registered twice", value)
		}
		seen[value] = true
		if kind == "Event" {
			e, ok := EventNamed(value)
			switch {
			case !ok:
				t.Errorf("event %s (%s) is not in the registry", value, name)
			case !eventRE.MatchString(value) || !strings.HasPrefix(value, e.Component+"."):
				t.Errorf("event %s: want <component>.<thing>.<what happened> with component %s", value, e.Component)
			case e.Level == "" || e.Meaning == "" || e.Next == "":
				t.Errorf("event %s: level, meaning, and next step are required", value)
			}
		} else {
			c, ok := CodeNamed(value)
			switch {
			case !ok:
				t.Errorf("code %s (%s) is not in the registry", value, name)
			case !codeRE.MatchString(value):
				t.Errorf("code %s: want <area>.<reason>", value)
			case c.Meaning == "" || c.Next == "":
				t.Errorf("code %s: meaning and next step are required", value)
			}
		}
		if used[name] == 0 {
			t.Errorf("%s (%s) is registered but never used", value, name)
		}
	}
}

// TestRegistryUses checks the usage counter itself: a comment does not
// count, code does, and building an Event from a string in package diag
// is caught.
func TestRegistryUses(t *testing.T) {
	src := map[string]string{
		"internal/cli/a.go":  "package cli\n// diag.EvGCRemoved() in a comment\n/* diag.CodeUsage() */\nvar _ = diag.CodeStore()\n",
		"internal/diag/b.go": "package diag\n// EvGCRefused\nvar _ = EvGCRemoved\nvar _ = Event{\"x.y.z\"}\nvar _ = Code{}\n",
	}
	used, built := uses(t, src)
	if used["EvGCRemoved"] != 1 || used["CodeUsage"] != 0 || used["CodeStore"] != 1 || used["EvGCRefused"] != 0 {
		t.Errorf("uses = %v", used)
	}
	if len(built) != 1 || built[0] != "internal/diag/b.go" {
		t.Errorf("built = %v", built)
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
