package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	if cmd == "" {
		fmt.Fprintf(stderr, "aggy: %s\n", msg)
	} else {
		fmt.Fprintf(stderr, "aggy %s: %s\n", cmd, msg)
	}
	return id
}

// cmdErr prints and records a command's error under its code; exit 1.
func cmdErr(stderr io.Writer, cmd string, code diag.Code, err error) int {
	refuse(context.Background(), stderr, cmd, diag.EvCommandRefused, code, err.Error(), errRec(diag.Rec{}, err))
	return 1
}

// cmdFail is cmdErr with aggy's own words.
func cmdFail(stderr io.Writer, cmd string, code diag.Code, msg string) int {
	refuse(context.Background(), stderr, cmd, diag.EvCommandRefused, code, msg, diag.Rec{})
	return 1
}

// cmdUsage prints and records a usage error, with the usage text after the
// tagged line unless it is empty; exit 2.
func cmdUsage(stderr io.Writer, cmd, msg, usage string) int {
	refuse(context.Background(), stderr, cmd, diag.EvCommandRefused, diag.CodeUsage, msg, diag.Rec{})
	if usage != "" {
		fmt.Fprintf(stderr, "\n%s", usage)
	}
	return 2
}

// cmdErrDry is cmdErr for a command that may be a dry run: one prints the
// error alone, and writes no record.
func cmdErrDry(stderr io.Writer, cmd string, code diag.Code, err error, dryRun bool) int {
	refuseWith(writerFor(dryRun), context.Background(), stderr, cmd, diag.EvCommandRefused, code, err.Error(), errRec(diag.Rec{}, err))
	return 1
}

// writerFor is the log a command's refusal goes through: none for a dry
// run, which writes nothing, so it prints no tag either.
func writerFor(dryRun bool) *diag.Writer {
	if dryRun {
		return nil
	}
	return dlog
}

// fixedMsg is a record's message for code: its registry text.
func fixedMsg(code diag.Code) string {
	c, _ := diag.LookupCode(code)
	return c.Meaning
}

// errRec is rec with attrs.error_class, err's class: a fixed word, since
// the record's message is the code's text and never the error's
// (G67-R4).
func errRec(rec diag.Rec, err error) diag.Rec {
	attrs := map[string]any{"error_class": errClass(err)}
	for k, v := range rec.Attrs {
		attrs[k] = v
	}
	rec.Attrs = attrs
	return rec
}

// errClass is err's class for a record: what it is, never what it says.
func errClass(err error) string {
	var pe *fs.PathError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not_exist"
	case errors.Is(err, fs.ErrPermission):
		return "permission"
	case errors.Is(err, fs.ErrExist):
		return "exist"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &pe):
		return "path"
	}
	return "other"
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
