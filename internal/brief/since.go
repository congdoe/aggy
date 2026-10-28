package brief

import (
	"errors"
	"fmt"
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
// turns others added after since: others in the order given (oldest
// session first), each turn's role headed by its vendor ("codex
// assistant"). Path holds every transcript's path, one a line, and the
// error joins those that could not be read. A turn with a time counts
// only when it is after since.
//
// shortcut: Claude and Grok turns carry no time aggy reads (only Codex's
// timestamp is verified, docs/adapters.md), so every turn of such a
// session that was active since is taken, still within MaxTurnBytes;
// filter them too once their timestamps are verified.
func Since(since time.Time, others []Other) (adapter.Transcript, error) {
	var out adapter.Transcript
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
			if !t.Time.IsZero() && !t.Time.After(since) {
				continue
			}
			t.Role = string(o.Vendor) + " " + t.Role
			out.Turns = append(out.Turns, t)
		}
	}
	out.Path = strings.Join(paths, "\n")
	return out, errors.Join(errs...)
}
