package brief

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// Other is another CLI's session of the same parent, read for a return
// brief: Transcript as transcript.Tail returns it, or Err.
type Other struct {
	Vendor     adapter.Vendor
	Transcript adapter.Transcript
	Err        error
}

// Since returns, for a return brief to a child last closed at since, the
// turns others added after since, each turn's role headed by its vendor
// ("codex assistant"). Path holds every transcript's path, one a line,
// and the error joins those that could not be read. A turn with a time
// counts only when it is after since.
//
// Turns without a time come first, in the order others are given (oldest
// session first), and the dated turns last, oldest first: Build keeps the
// newest MaxTurnBytes, so a long undated session cannot push out the
// turns known to be new (G34-3).
//
// shortcut: Claude and Grok turns carry no time aggy reads (only Codex's
// timestamp is verified, docs/adapters.md), so every turn of such a
// session that was active since is taken, still within MaxTurnBytes;
// filter them too once their timestamps are verified.
func Since(since time.Time, others []Other) (adapter.Transcript, error) {
	var undated, dated []adapter.Turn
	var paths []string
	var errs []error
	for _, o := range others {
		if o.Err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", o.Vendor, o.Err))
			continue
		}
		if o.Transcript.Path != "" {
			paths = append(paths, o.Transcript.Path)
		}
		for _, t := range o.Transcript.Turns {
			t.Role = string(o.Vendor) + " " + t.Role
			switch {
			case t.Time.IsZero():
				undated = append(undated, t)
			case t.Time.After(since):
				dated = append(dated, t)
			}
		}
	}
	slices.SortStableFunc(dated, func(a, b adapter.Turn) int { return a.Time.Compare(b.Time) })
	return adapter.Transcript{Path: strings.Join(paths, "\n"), Turns: append(undated, dated...)}, errors.Join(errs...)
}
