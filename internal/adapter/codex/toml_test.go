package codex

import (
	"testing"

	"github.com/BurntSushi/toml"
)

// TestTOMLStringRoundTrips: Codex parses a -c value as TOML, so the
// quoted memory must decode to exactly the text.
func TestTOMLStringRoundTrips(t *testing.T) {
	for _, s := range []string{"", "plain", `q"uo\te`, "multi\nline\ttab", "ctl\x00\x1b\x7f", "漢字🙂", `'''`, `"""`, "\\u0041"} {
		var doc struct{ V string }
		if _, err := toml.Decode("V = "+tomlString(s), &doc); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if doc.V != s {
			t.Errorf("%q decoded as %q", s, doc.V)
		}
	}
}
