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
	rec.Event, rec.Code, rec.Msg = ev, code, msg
	id := dlog.Log(ctx, rec)
	fmt.Fprintf(stderr, "aggy %s: %s %s\n", cmd, termsafe.Field(msg), dlog.Tag(code))
	return id
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
