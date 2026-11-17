package cli

import (
	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/screen"
)

// screenClassifiers are the vendors whose screen the supervisor may read
// (T7016), each by its adapter's classifier. Only Claude Code has a recorded
// one (docs/adapters.md, "Reading the screen"); the others keep asking.
var screenClassifiers = map[adapter.Vendor]adapter.ScreenClassifier{
	adapter.Claude: claude.ClassifyScreen,
}

// screenReader is the supervisor's Options.ReadScreen for v at version, nil
// for a vendor with no classifier.
func screenReader(v adapter.Vendor, version string) func(*screen.Snapshot) adapter.ScreenState {
	c := screenClassifiers[v]
	if c == nil {
		return nil
	}
	return func(s *screen.Snapshot) adapter.ScreenState { return c(version, s) }
}
