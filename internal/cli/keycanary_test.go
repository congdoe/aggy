package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/congdoe/aggy/internal/redact"
)

// The key tests' canaries (#138 review M2): shapes the redactor does not
// know, so a leak is not hidden by redact dropping the line. keyCanary looks
// like a Groq key; hexCanary like a plain 32-hex token.
const (
	keyCanary = "gsk_ab12CD34ef56GH78ij90KL12mn34OP56"
	hexCanary = "9f8e7d6c5b4a39281706f5e4d3c2b1a0"
)

// canaryForms are canary and its hex, base64, and base64url spellings, so a
// key written encoded is caught too.
func canaryForms(canary string) [][]byte {
	b := []byte(canary)
	return [][]byte{
		b,
		[]byte(hex.EncodeToString(b)),
		[]byte(base64.StdEncoding.EncodeToString(b)),
		[]byte(base64.RawStdEncoding.EncodeToString(b)),
		[]byte(base64.URLEncoding.EncodeToString(b)),
		[]byte(base64.RawURLEncoding.EncodeToString(b)),
	}
}

// holdsCanary reports whether data holds canary in any of its forms.
func holdsCanary(data []byte, canary string) bool {
	for _, f := range canaryForms(canary) {
		if bytes.Contains(data, f) {
			return true
		}
	}
	return false
}

// TestCanariesNotRedacted: the canaries pass the redactor unchanged, alone and
// in plain words, so a test that looks for one in the diagnostic log, a brief,
// or a report would see a leak rather than a dropped line. (Beside a secret
// setting's name, key= or Authorization:, any value is dropped, as it should
// be; that is the setting's shape, not the canary's.)
func TestCanariesNotRedacted(t *testing.T) {
	for _, c := range []string{keyCanary, hexCanary} {
		for _, line := range []string{c, "the value " + c + " here", "provider anthropic " + c} {
			if got, n := redact.Each(line); got != line || n != 0 {
				t.Errorf("redact.Each(%q) = %q, %d", line, got, n)
			}
			if got, n := redact.Lines(line); got != line || n != 0 {
				t.Errorf("redact.Lines(%q) = %q, %d", line, got, n)
			}
			if redact.Secret(line) {
				t.Errorf("redact.Secret(%q)", line)
			}
		}
		if !holdsCanary([]byte("x"+base64.StdEncoding.EncodeToString([]byte(c))+"x"), c) {
			t.Errorf("holdsCanary misses the base64 form of %q", c)
		}
	}
}
