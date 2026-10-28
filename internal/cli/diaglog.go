package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/termsafe"
)

// dlog is this process's diagnostic log (docs/logging.md): one run id per
// aggy invocation, and the trace from AGGY_TRACE or a new one.
var dlog = diag.New(version(), os.Stderr)

// refuse logs a refusal or error under ev and code, with rec's ids, and
// prints it as aggy prints every refusal and error: "aggy <cmd>: <msg>
// [<code> · run <id>]". It returns the record id ("" when not stored).
func refuse(ctx context.Context, stderr io.Writer, cmd string, ev diag.Event, code diag.Code, msg string, rec diag.Rec) string {
	return refuseWith(dlog, ctx, stderr, cmd, ev, code, msg, rec)
}

// refuseWith is refuse through w. A nil w (a dry run, which writes
// nothing) prints the message alone.
func refuseWith(w *diag.Writer, ctx context.Context, stderr io.Writer, cmd string, ev diag.Event, code diag.Code, msg string, rec diag.Rec) string {
	// The record's message is the code's registry text, never msg: an
	// error can quote argv, a path, an environment value, git's or a
	// vendor CLI's own text, or a file an edit child wrote (G67-1).
	rec.Event, rec.Code, rec.Msg = ev, code, fixedMsg(code)
	id := w.Log(ctx, rec)
	if tag := w.Tag(code); tag != "" {
		msg = termsafe.Field(msg) + " " + tag
	} else {
		msg = termsafe.Field(msg)
	}
	fmt.Fprintf(stderr, "aggy %s: %s\n", cmd, msg)
	return id
}

// fixedMsg is a record's message for code: its registry text.
func fixedMsg(code diag.Code) string {
	c, _ := diag.LookupCode(code)
	return c.Meaning
}

// codedError is an error with its reason code, for the paths that can
// fail for several reasons; its text is the error's own.
type codedError struct {
	code diag.Code
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// withCode gives err the reason code c, unless it has one already.
func withCode(c diag.Code, err error) error {
	var ce *codedError
	if err == nil || errors.As(err, &ce) {
		return err
	}
	return &codedError{c, err}
}

// codeOf is err's reason code, or def when it has none.
func codeOf(err error, def diag.Code) diag.Code {
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	return def
}
