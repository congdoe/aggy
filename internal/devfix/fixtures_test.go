package devfix

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/congdoe/aggy/internal/safefs"
)

// TestFixturesHoldNoSecrets fails when any file under testdata/fixtures
// holds a home path, an email, or a token-shaped string, in its content
// or its path. Documented false positives go in
// testdata/fixtures/scrub-allowlist.txt, which is itself not checked.
func TestFixturesHoldNoSecrets(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "fixtures")
	b, err := safefs.ReadFile(filepath.Join(root, "scrub-allowlist.txt"))
	if err != nil {
		t.Fatal(err)
	}
	allow, err := ParseAllowlist(b)
	if err != nil {
		t.Fatal(err)
	}
	id := CurrentIdentity()
	if id.Home == "" {
		id.Home, _ = os.UserHomeDir()
	}
	findings, err := Detect(root, id, allow)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.File == "scrub-allowlist.txt" {
			continue // it names the allowed matches
		}
		t.Errorf("testdata/fixtures/%s; run aggy dev scrub, or allowlist a documented false positive", f)
	}
}
