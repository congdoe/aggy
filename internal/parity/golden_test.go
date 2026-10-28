package parity

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/diff"
	"github.com/congdoe/aggy/internal/termsafe"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// golden compares the escaped unified diff from in to out with
// testdata/<name>.golden. The diff is escaped with termsafe, so a CR
// shows as \r and the golden files are plain LF text.
func golden(t *testing.T, name string, in, out []byte) {
	t.Helper()
	from := "a/" + name
	if in == nil {
		from = diff.DevNull
	}
	got := termsafe.Lines(diff.Unified(from, "b/"+name, in, out))
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// TestEveryPatchHasGolden: every patch aggy can make has golden cases,
// and every golden file belongs to a case, so none goes stale.
func TestEveryPatchHasGolden(t *testing.T) {
	cases := map[string]bool{}
	for _, c := range codexCases {
		cases[c.name] = true
	}
	for _, c := range excludeCases {
		cases[c.name] = true
	}
	for _, prefix := range []string{"codex_", "exclude_"} {
		found := false
		for n := range cases {
			found = found || strings.HasPrefix(n, prefix)
		}
		if !found {
			t.Errorf("no golden case for the %s patch", strings.TrimSuffix(prefix, "_"))
		}
	}
	files, err := filepath.Glob(filepath.Join("testdata", "*.golden"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if n := strings.TrimSuffix(filepath.Base(f), ".golden"); !cases[n] {
			t.Errorf("%s has no case", f)
		}
	}
	if len(files) != len(cases) {
		t.Errorf("%d golden files for %d cases", len(files), len(cases))
	}
}
