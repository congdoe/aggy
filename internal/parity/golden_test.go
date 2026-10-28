package parity

import (
	"flag"
	"os"
	"path/filepath"
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
