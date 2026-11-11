package safefs

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// normPathRef is normPath as it was before its ASCII shortcut (T6954).
func normPathRef(p string) string {
	p = norm.NFC.String(p)
	prefix := dataVolume
	if foldCase {
		p = norm.NFC.String(cases.Fold().String(p))
		prefix = foldedDataVolume
	}
	if stripFirmlink && strings.HasPrefix(p, prefix+string(filepath.Separator)) {
		p = p[len(prefix):]
	}
	return p
}

// TestNormPathASCII (T6954): the ASCII shortcut gives the same answer as
// NFC and full case folding, with case folded or not, for every ASCII byte
// and for the names the guard holds, and non-ASCII paths still take the
// full path.
func TestNormPathASCII(t *testing.T) {
	var all strings.Builder
	for b := 1; b < 0x80; b++ {
		all.WriteByte(byte(b))
	}
	paths := []string{
		all.String(),
		"/System/Volumes/Data/Users/X/.Codex/Auth.JSON",
		"/SYSTEM/volumes/DATA/x",
		"/Users/x/.local/share/opencode/opencode.db-wal",
		"/Users/x/.Hermes/profiles/P/.env",
		"/Syſtem/Volumes/Data/Users/x",
		"/Users/x/KK/straße/é",
	}
	for _, fold := range []bool{false, true} {
		for _, strip := range []bool{false, true} {
			oldF, oldS := foldCase, stripFirmlink
			foldCase, stripFirmlink = fold, strip
			for _, p := range paths {
				if got, want := normPath(p), normPathRef(p); got != want {
					t.Errorf("fold %v strip %v: normPath(%q) = %q, want %q", fold, strip, p, got, want)
				}
			}
			foldCase, stripFirmlink = oldF, oldS
		}
	}
}
