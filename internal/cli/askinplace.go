package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/termsafe"
)

// Asking in place (T6979, docs/design.md section 10, "Asking in place"):
// aggy guards against mistakes, so when the person at the keyboard
// deliberately wants something a refusal used to stop (the Codex rules file
// not written, a harness turned off, a loosening flag), aggy asks and goes on
// instead of naming a command to run and making them start again. Every such
// question goes through askInPlace, so they all ask the same way.

// inPlace is one question asked in place of a refusal.
type inPlace struct {
	// change is one plain line: what a yes changes.
	change string
	// undo says how to undo it ("" when nothing is kept: a yes for this
	// launch only).
	undo string
	// ask is the question, with no [y/N].
	ask string
}

// harnessOnInPlace offers, at a launch of a harness the person turned off, to
// turn it on in their own config, as aggy config set harness.<v> on does, and
// reports whether it is on now.
func harnessOnInPlace(ctx context.Context, v adapter.Vendor, getenv func(string) string, in io.Reader, out io.Writer) (bool, error) {
	key := "harness." + string(v)
	ps, already, _, err := configChange(key, "on")
	if err != nil || already {
		return already, err
	}
	yes, asked := askInPlace(getenv, in, out, inPlace{
		change: fmt.Sprintf("%s is turned off in your aggy config. aggy can turn it on (aggy config set %s on).", v, key),
		undo:   "aggy unwire undoes it, or aggy config set " + key + " off.",
		ask:    fmt.Sprintf("Turn %s on and start it?", v),
	})
	if !asked || !yes {
		return false, nil
	}
	runID, err := applyPatches(ctx, ps)
	if err != nil {
		return false, err
	}
	fmt.Fprintf(out, "Done (run %s). aggy unwire --run %s undoes it.\n", runID, runID)
	return true, nil
}

// pickInPlace asks the person to pick one of options by number, as askInPlace
// asks (the same gate, typed-ahead and pasted answers dropped), and returns
// its index, or -1 for none, the default. asked is false with no one to ask.
func pickInPlace(getenv func(string) string, in io.Reader, out io.Writer, intro string, options []string) (int, bool) {
	if !trustPerson(getenv) || len(options) == 0 {
		return -1, false
	}
	fmt.Fprintln(out, termsafe.Lines(intro))
	for i, o := range options {
		fmt.Fprintf(out, "  %d  %s\n", i+1, termsafe.Line(o))
	}
	fmt.Fprint(out, "Start which? A number, or Enter for none: ")
	line := readAnswer(in, time.Now())
	if strings.Contains(line, pasteStart) {
		fmt.Fprintln(out, "A paste answers nothing here: none.")
		return -1, true
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(line), "%d", &n); err != nil || n < 1 || n > len(options) || fmt.Sprint(n) != strings.TrimSpace(line) {
		return -1, true
	}
	return n - 1, true
}

// askInPlace asks q of the person at the terminal and reports a yes. It
// asks only a person (trustPerson: not a borrow child, not a session's own
// tool shell, stdin and stderr terminals); with no one to ask, asked is
// false and the caller's refusal stands. The answer is one line read from
// in: a line typed ahead of the question is dropped (readAnswer), a pasted
// answer answers nothing, and anything but y or yes is no. A dry run never
// calls it.
func askInPlace(getenv func(string) string, in io.Reader, out io.Writer, q inPlace) (yes, asked bool) {
	if !trustPerson(getenv) {
		return false, false
	}
	fmt.Fprintln(out, termsafe.Lines(q.change))
	if q.undo != "" {
		fmt.Fprintln(out, termsafe.Lines(q.undo))
	}
	fmt.Fprintf(out, "%s [y/N] ", termsafe.Line(q.ask))
	line := readAnswer(in, time.Now())
	if strings.Contains(line, pasteStart) {
		// The rest of the paste is read and dropped with it, so none of
		// it reaches the CLI as typing.
		for !strings.Contains(line, pasteEnd) {
			next, err := readLine(in, maxAnswer)
			if err != nil {
				break
			}
			line = next
		}
		fmt.Fprintln(out, "A paste answers nothing here: no.")
		return false, true
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes", true
}
