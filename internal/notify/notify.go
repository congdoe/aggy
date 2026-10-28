// Package notify shows desktop notifications: a CLI waiting for the
// person, a borrow finished, a vendor at its limit. It is off unless the
// user config turns it on ([notify] desktop = true); a repo's config
// cannot.
//
// macOS runs /usr/bin/osascript and Linux runs notify-send, both through
// the adapter.Runner the caller passes (internal/execrun). Text can come
// from places aggy does not control, so it never becomes code or markup:
//
//   - osascript gets a fixed script ("on run argv") and the title and
//     body as its arguments, which AppleScript reads as strings and
//     never compiles;
//   - notify-send gets them as separate arguments after "--", and the
//     characters of its body markup (< > &) are dropped;
//   - control and format characters are dropped, a leading "-" is
//     trimmed so no text reads as an option, and both are capped.
//
// Callers put no session id, path, or environment value in the text.
// When the tool is missing or fails, nothing is shown and nothing is said.
package notify

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/congdoe/aggy/internal/adapter"
)

// Caps on the text, in runes.
const (
	MaxTitle = 64
	MaxBody  = 200
)

// Osascript is the macOS tool's fixed path.
const Osascript = "/usr/bin/osascript"

// osaScript reads the title and body from argv, so neither is compiled.
var osaScript = []string{
	"-e", "on run argv",
	"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
	"-e", "end run",
}

// Command returns the command that shows title and body on goos with the
// tool at path, or false when goos has none.
func Command(goos, path, title, body string) (adapter.Command, bool) {
	title, body = Clean(title, MaxTitle), Clean(body, MaxBody)
	if title == "" {
		title = "aggy"
	}
	switch goos {
	case "darwin":
		return adapter.Command{Path: path, Args: append(append([]string{}, osaScript...), title, body), Dir: "/"}, true
	case "linux", "freebsd", "openbsd", "netbsd":
		return adapter.Command{Path: path, Args: []string{"--", title, body}, Dir: "/"}, true
	}
	return adapter.Command{}, false
}

// Clean drops control and format characters (bidi overrides among them)
// and notify-send's markup characters, folds whitespace runs to one
// space, trims a leading "-", and caps the result at max runes.
func Clean(s string, max int) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range s {
		if n >= max {
			break
		}
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == '<', r == '>', r == '&', r == unicode.ReplacementChar:
			continue
		case r == '-' && b.Len() == 0:
			continue
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
			if n >= max {
				break
			}
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimRight(b.String(), " ")
}

// Notifier shows notifications. A nil *Notifier shows nothing.
type Notifier struct {
	Run  adapter.Runner
	GOOS string
	Path string // the tool's absolute path
	// Every is the least time between two notifications with one key.
	Every time.Duration
	Now   func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
	busy bool
}

// Show shows title and body unless one with key was shown within Every
// or another is still running. It waits for the tool; failures are
// ignored.
func (n *Notifier) Show(ctx context.Context, key, title, body string) {
	if n == nil || n.Run == nil {
		return
	}
	c, ok := Command(n.GOOS, n.Path, title, body)
	if !ok {
		return
	}
	now := time.Now
	if n.Now != nil {
		now = n.Now
	}
	n.mu.Lock()
	t := now()
	if n.busy || !n.last[key].IsZero() && t.Sub(n.last[key]) < n.Every {
		n.mu.Unlock()
		return
	}
	if n.last == nil {
		n.last = map[string]time.Time{}
	}
	n.last[key], n.busy = t, true
	n.mu.Unlock()
	_, _ = n.Run(ctx, c)
	n.mu.Lock()
	n.busy = false
	n.mu.Unlock()
}
