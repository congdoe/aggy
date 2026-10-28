//go:build unix

package supervisor

import (
	"errors"
	"time"

	"github.com/congdoe/aggy/internal/diag"
)

// codedError is a refusal with its reason code (docs/logging.md); its
// text is the error's own.
type codedError struct {
	code diag.Code
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// coded gives err the reason code c, unless it has one already.
func coded(c diag.Code, err error) error {
	var ce *codedError
	if err == nil || errors.As(err, &ce) {
		return err
	}
	return &codedError{c, err}
}

// codeOf is err's reason code, or def.
func codeOf(err error, def diag.Code) diag.Code {
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	return def
}

// log queues r about the session: its parent, the on-screen child, and
// the vendor on screen unless r names another. It never waits.
func (s *session) log(r diag.Rec) string {
	r.Parent, r.Child = s.o.ParentID, s.o.ChildID
	if r.Vendor == "" {
		r.Vendor = string(s.o.Vendor)
	}
	return s.o.Diag.Log(r)
}

// logWait is log for the socket's goroutines, which are not the
// terminal loop: it waits up to sockLogWait for room in a full queue.
func (s *session) logWait(r diag.Rec) {
	r.Parent, r.Child = s.o.ParentID, s.o.ChildID
	if r.Vendor == "" {
		r.Vendor = string(s.o.Vendor)
	}
	s.o.Diag.LogWait(r, sockLogWait)
}

// sockLogWait bounds how long a socket goroutine waits for room in the
// diagnostic queue; a hook waits for the answer, up to 3 s.
const sockLogWait = 100 * time.Millisecond

// hookName is a hook event's name for a record: the names aggy acts on,
// else "other", since any process in the CLI's tree can pick the name.
func hookName(event string) string {
	switch event {
	case "Stop", "StopFailure", "StopCancelled", "SessionStart", "Notification":
		return event
	}
	return "other"
}

// switchAttrs are a switch's options as record attrs: never the model's
// name, only whether one was set.
func switchAttrs(c Choice, more map[string]any) map[string]any {
	m := map[string]any{"brief": c.Brief, "new": c.New, "model_set": c.Model != "", "child_set": c.Child != ""}
	for k, v := range more {
		m[k] = v
	}
	return m
}
