package adapter

import (
	"strings"
	"testing"
)

// TestLiveChecksByVersion (#146 review): the composer's live checks hold only
// at the version they ran on; any other version, or an unreadable one, gets
// none of them and is told a check is owed.
func TestLiveChecksByVersion(t *testing.T) {
	for _, c := range []struct {
		v              Vendor
		version        string
		clear, exit    bool
		down, up, bksp bool
		owed           bool
	}{
		{Claude, "2.1.322", true, true, true, false, true, false},
		{Claude, "2.1.330", false, false, false, false, false, true},
		{Claude, "2.1.311", false, false, false, false, false, true},
		{Claude, "", false, false, false, false, false, true},
		{Codex, "0.164.0", false, false, false, false, false, false},
		{Grok, "2.1.322", false, false, false, false, false, false},
		{OpenCode, "2.2.5", false, true, false, false, false, false},
	} {
		if got := ClearLineChecked(c.v, c.version); got != c.clear {
			t.Errorf("%s %q: ClearLineChecked = %v", c.v, c.version, got)
		}
		if _, got := ExitCommandAt(c.v, c.version); got != c.exit {
			t.Errorf("%s %q: ExitCommandAt verified = %v", c.v, c.version, got)
		}
		for _, k := range []struct {
			key  string
			want bool
		}{{"down", c.down}, {"up", c.up}, {"backspace", c.bksp}, {"ctrl+u", c.bksp}, {"delete", c.bksp}, {"left", false}} {
			if got := EmptyComposerKey(c.v, c.version, k.key); got != k.want {
				t.Errorf("%s %q: EmptyComposerKey(%s) = %v", c.v, c.version, k.key, got)
			}
		}
		owed := ComposerCheckOwed(c.v, c.version)
		if (owed != "") != c.owed {
			t.Errorf("%s %q: ComposerCheckOwed = %q", c.v, c.version, owed)
		}
		if c.owed && !strings.Contains(owed, "2.1.322") {
			t.Errorf("%s %q: the note names no checked version: %q", c.v, c.version, owed)
		}
	}
	if _, verified := ExitCommand(Claude); verified {
		t.Error("ExitCommand(Claude) is verified with no version")
	}
}
