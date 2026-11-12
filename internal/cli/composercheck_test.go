package cli

import (
	"context"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

// TestComposerCheckNote (#146 review): the launch says the composer's live
// checks are owed once for each Claude version, not at every launch, switch,
// and tool restart; the checked version and other CLIs say nothing.
func TestComposerCheckNote(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for _, c := range []struct {
		v       adapter.Vendor
		version string
		want    bool
	}{
		{adapter.Claude, "2.1.322", false},
		{adapter.Codex, "0.164.0", false},
		{adapter.Claude, "2.1.330", true},
		{adapter.Claude, "2.1.330", false},
		{adapter.Claude, "2.1.331", true},
	} {
		if got := composerCheckNote(ctx, s, c.v, c.version); (got != "") != c.want {
			t.Errorf("%s %q: %q, want a note %v", c.v, c.version, got, c.want)
		}
	}
	if composerCheckNote(ctx, nil, adapter.Claude, "2.1.330") == "" {
		t.Error("with no index the note was not said")
	}
}
