package adapter

import "github.com/congdoe/aggy/internal/screen"

// ScreenState is what a vendor's screen classifier reads off a copy of the
// CLI's screen (internal/screen, T7016). Each state other than ScreenOther
// is given only when all of its marks hold, at a version a live recording
// covers; anything else is ScreenOther. See docs/adapters.md, "Reading the
// screen".
type ScreenState int

// The screen states.
const (
	// ScreenOther: a dialog, a menu, another view, or anything not recognised.
	ScreenOther ScreenState = iota
	// ScreenPromptEmpty: the CLI's composer, empty (its placeholder at most),
	// with the cursor in it and nothing open over it.
	ScreenPromptEmpty
	// ScreenPromptText: the composer, holding text.
	ScreenPromptText
	// ScreenBusy: the composer, with a turn running.
	ScreenBusy
)

// String is the state's fixed name.
func (s ScreenState) String() string {
	switch s {
	case ScreenPromptEmpty:
		return "prompt_empty"
	case ScreenPromptText:
		return "prompt_text"
	case ScreenBusy:
		return "busy"
	}
	return "other"
}

// ScreenClassifier classifies a copy of a CLI's screen at the CLI's version.
type ScreenClassifier func(version string, s *screen.Snapshot) ScreenState
