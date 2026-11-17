package adapter

import (
	"strings"
	"testing"
)

// TestLiveChecksByVersion (#146 review; T6982): the composer's live checks
// hold at the versions they ran on and at later patch versions of the same
// major.minor line; an older version, another minor or major, or an
// unreadable or pre-release version gets none of them. aggy doctor says a
// check is owed for those, and for a covered version newer than the last
// check.
func TestLiveChecksByVersion(t *testing.T) {
	for _, c := range []struct {
		v              Vendor
		version        string
		clear, exit    bool
		down, up, bksp bool
		owed           bool
	}{
		{Claude, "2.1.322", true, true, true, false, true, false},
		{Claude, "2.1.326", true, true, true, false, true, false},
		{Claude, "2.1.324", true, true, true, false, true, false},
		{Claude, "2.1.330", true, true, true, false, true, true},
		{Claude, "2.1.311", false, false, false, false, false, true},
		{Claude, "2.1.32", false, false, false, false, false, true},
		{Claude, "2.2.0", false, false, false, false, false, true},
		{Claude, "3.1.330", false, false, false, false, false, true},
		{Claude, "2.1.330-beta", false, false, false, false, false, true},
		{Claude, "", false, false, false, false, false, true},
		{Codex, "0.164.0", false, false, false, false, false, false},
		{Grok, "2.1.322", false, false, false, false, false, false},
		{OpenCode, "2.2.5", false, true, false, false, false, false},
	} {
		if got := FirstTurnSwitch(c.v, c.version); got != c.clear {
			t.Errorf("%s %q: FirstTurnSwitch = %v", c.v, c.version, got)
		}
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
		// A blocked prompt's empty composer was checked with the rest.
		if got := BlockedPromptEmpty(c.v, c.version); got != c.clear {
			t.Errorf("%s %q: BlockedPromptEmpty = %v", c.v, c.version, got)
		}
		owed := ComposerCheckOwed(c.v, c.version)
		if (owed != "") != c.owed {
			t.Errorf("%s %q: ComposerCheckOwed = %q", c.v, c.version, owed)
		}
		if c.owed && !strings.Contains(owed, "2.1.32") {
			t.Errorf("%s %q: the note names no checked version: %q", c.v, c.version, owed)
		}
	}
	if _, verified := ExitCommand(Claude); verified {
		t.Error("ExitCommand(Claude) is verified with no version")
	}
}
