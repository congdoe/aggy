package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

// The first-run offer (T6966): bare aggy, at a terminal, on a machine where
// aggy setup never ran, offers to do in one step what setup and its follow-up
// commands do, then launches either way.

// setupOfferNote is the notes row that records the offer's answer, so it is
// asked once and a decline's note is shown once.
const setupOfferNote = "setup_offer"

// The answers recorded under setupOfferNote.
const (
	offerAccepted = "accepted"
	offerDeclined = "declined"
)

// setupOfferEnv, set to 0, turns the offer off (a scripted machine, the
// end-to-end tests).
const setupOfferEnv = "AGGY_SETUP_OFFER"

// offerTerminal reports whether both stdin and stderr are terminals, where a
// person can read the screen and answer; a test sets it.
var offerTerminal = func() bool { return isTerminal(os.Stdin) && isTerminal(os.Stderr) }

// offerIn is where the offer reads its answer; a test sets it.
var offerIn io.Reader = os.Stdin

// typeAhead is how soon after the question a line counts as typed before it
// (PR #127 review): a line already waiting, typed before aggy asked (say, the
// first prompt meant for the CLI), arrives at once and is dropped, so only an
// answer typed to the question answers it. aggy cannot flush the terminal's
// input itself (no termios ioctl: golang.org/x/sys and raw syscalls are
// banned), so this is a heuristic; a test sets it to 0.
var typeAhead = 150 * time.Millisecond

// readAnswer reads the line that answers a question asked at asked, dropping
// any whole line that arrives within typeAhead of it (typed ahead), and reads
// a byte at a time, so nothing past the answer is taken from the terminal.
func readAnswer(r io.Reader, asked time.Time) string {
	for {
		line, err := readLine(r, maxAnswer)
		if err != nil || time.Since(asked) >= typeAhead {
			return line
		}
	}
}

// shouldOffer reports whether bare aggy offers setup now: a person at a
// terminal (not a borrow child, not a session's own tool shell, not turned
// off), and no answer recorded and no vendor ever checked by aggy setup. It
// writes nothing: an index that is not there means a first run, and one that
// cannot be read means no offer.
func shouldOffer(ctx context.Context, getenv func(string) string) bool {
	for _, k := range []string{"AGGY_DELEGATED", "AGGY_SOCKET", "AGGY_PARENT"} {
		if getenv(k) != "" {
			return false
		}
	}
	if getenv(setupOfferEnv) == "0" || !offerTerminal() {
		return false
	}
	path, err := store.DefaultPath()
	if err != nil {
		return false
	}
	if _, err := safefs.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return true
	} else if err != nil {
		return false
	}
	s, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return false
	}
	defer func() { _ = s.Close() }()
	if _, ok, err := s.Note(ctx, setupOfferNote); ok || err != nil {
		return false
	}
	rows, err := s.VendorStatuses(ctx)
	return err == nil && len(rows) == 0
}

// setupDeclined reports whether the person declined the first-run offer, so
// the launch's line about hooks not installed at all, said in that note
// already, is not said on every launch.
func setupDeclined(ctx context.Context, s *store.Store) bool {
	v, ok, err := s.Note(ctx, setupOfferNote)
	return err == nil && ok && v == offerDeclined
}

// wirePlan is what setup wires besides its checks: aggy's hooks for each of
// Claude Code, Codex, and Grok Build found on PATH, and Codex's rules file
// (aggy's safety policy) when codex is found. Every change is a patch for
// the manifest, so aggy unwire takes it out.
type wirePlan struct {
	patches []patch.Patch
	// files are the files the patches change, as the screen names them.
	files []string
	// found are the vendor CLIs on PATH, in aggy's order.
	found []adapter.Vendor
	// left are steps aggy cannot do for the person, each said once.
	left []string
	// problems are what cannot be wired, with why.
	problems []string
}

// planWire plans setup's wiring. It writes nothing.
func planWire(home string) wirePlan {
	var w wirePlan
	for _, v := range []adapter.Vendor{adapter.Claude, adapter.Codex, adapter.Grok} {
		if _, err := probe.LookPath(vendorBin(v)); err == nil {
			w.found = append(w.found, v)
		}
	}
	if len(w.found) == 0 {
		return w
	}
	bin, err := aggyBin()
	if err != nil {
		w.problems = append(w.problems, "aggy's hooks: "+err.Error())
	}
	for _, v := range w.found {
		if err == nil {
			hp, herr := planHooks(v, bin)
			if herr != nil {
				w.problems = append(w.problems, fmt.Sprintf("%s's hooks: %v", v, herr))
			} else {
				w.patches = append(w.patches, hp.patches...)
				if hp.note != "" {
					w.left = append(w.left, hp.note)
				}
			}
		}
		if v == adapter.Codex {
			path, perr := parity.CodexRulesPath()
			var p patch.Patch
			if perr == nil {
				p, perr = parity.CodexRulesPatch(path)
			}
			switch {
			case perr != nil:
				w.problems = append(w.problems, "Codex's rules file: "+perr.Error())
			case p.ExpectedHash != patch.Hash([]byte(parity.CodexRules)):
				w.patches = append(w.patches, p)
			}
		}
	}
	for _, p := range w.patches {
		if f := checkup.ShowPath(p.File, home); !slices.Contains(w.files, f) {
			w.files = append(w.files, f)
		}
	}
	w.left = append(w.left, "Each CLI may ask whether you trust a folder the first time it opens there; answer that in the CLI itself.")
	return w
}

// applyWire writes the plan through the manifest as one run, and returns
// its id; nothing to change is no run.
func applyWire(ctx context.Context, s *store.Store, w wirePlan) (string, error) {
	if len(w.patches) == 0 {
		return "", nil
	}
	m, err := manifest.Open(s)
	if err != nil {
		return "", err
	}
	runID := manifest.NewRunID()
	_, err = m.Run(ctx, manifest.Write, runID, w.patches, io.Discard)
	return runID, err
}

// offerScreen is the one screen the offer shows: what setup checks and every
// file it changes, by name.
func offerScreen(w wirePlan) string {
	var b strings.Builder
	b.WriteString("aggy is not set up on this machine yet. Setting up now will:\n")
	var found []string
	for _, v := range w.found {
		found = append(found, string(v))
	}
	if len(found) == 0 {
		b.WriteString("  - look for claude, codex, grok, and opencode (none of the first three is on PATH)\n")
	} else {
		fmt.Fprintf(&b, "  - check %s (on PATH): version and sign-in, recorded in aggy's own index\n", strings.Join(found, ", "))
	}
	if len(w.files) > 0 {
		b.WriteString("  - change these files, so aggy hears turn ends and limits and Codex gets aggy's safety rules:\n")
		for _, f := range w.files {
			fmt.Fprintf(&b, "      %s\n", termsafe.Field(f))
		}
		b.WriteString("    each change goes through aggy's manifest, and aggy unwire undoes it\n")
	} else if len(found) > 0 {
		b.WriteString("  - change no file: aggy's hooks and rules are in place already\n")
	}
	for _, p := range w.problems {
		fmt.Fprintf(&b, "  - not set up: %s\n", termsafe.Line(p))
	}
	return b.String()
}

// declinedNote is said once, when the person declines the offer.
const declinedNote = "setup skipped: without it aggy hears no turn end (a switch means quitting the CLI yourself), the switch picker lists no other vendor, and Codex refuses to start without aggy's rules file. aggy setup --wire does it all later; this note is not shown again."

// offerSetup is the first-run offer at bare aggy (T6966). It shows one screen,
// takes one answer (default no), and on yes runs setup's vendor checks and
// writes the wiring through the manifest; either way it records the answer and
// returns, and the launch goes on. Every failure is a line, never a refused
// launch.
func offerSetup(ctx context.Context, env checkEnv, out io.Writer) {
	w := planWire(env.home)
	fmt.Fprint(out, offerScreen(w))
	fmt.Fprint(out, "Set up now? [y/N] ")
	a := strings.ToLower(strings.TrimSpace(readAnswer(offerIn, time.Now())))
	declined := a != "y" && a != "yes"
	if declined {
		fmt.Fprintf(out, "aggy: %s\n", declinedNote)
	}
	s, err := store.OpenDefault(ctx)
	if err == nil {
		defer func() { _ = s.Close() }()
	}
	if declined {
		if err == nil {
			err = s.SetNote(ctx, setupOfferNote, offerDeclined, time.Now())
		}
		if err != nil {
			// Said once; the launch goes on, and the next bare aggy asks again.
			fmt.Fprintf(out, "aggy: that answer could not be saved, so the next aggy asks again: %s %s\n", termsafe.Line(err.Error()), dlog.Tag(diag.CodeStore))
		}
		return
	}
	if err != nil {
		fmt.Fprintf(out, "aggy: %s %s\n", termsafe.Line(err.Error()), dlog.Tag(diag.CodeStore))
		return
	}
	if err := setupAndWire(ctx, s, env, w, out); err != nil {
		// Not recorded: the next bare aggy offers again.
		fmt.Fprintf(out, "aggy: setup did not finish: %s %s\n", termsafe.Line(err.Error()), dlog.Tag(diag.CodeWireFailed))
		return
	}
	_ = s.SetNote(ctx, setupOfferNote, offerAccepted, time.Now())
}

// setupAndWire is setup's vendor checks, without asking, and then w written
// through the manifest, with the steps left to the person said once. aggy setup
// --wire and the first-run offer share it.
func setupAndWire(ctx context.Context, s *store.Store, env checkEnv, w wirePlan, out io.Writer) error {
	st := &setupRun{env: env, in: bufio.NewReader(strings.NewReader("")), out: out, store: s,
		findings: checkup.Environment(env.environ)}
	for _, a := range env.adapters {
		if st.vendor(ctx, a) != 0 {
			return st.err
		}
	}
	runID, err := applyWire(ctx, s, w)
	if err != nil {
		return err
	}
	reportWire(out, env.home, w, runID)
	return nil
}

// reportWire says what applyWire changed, how to undo it, and the steps left
// to the person, once.
func reportWire(out io.Writer, home string, w wirePlan, runID string) {
	for _, p := range w.patches {
		fmt.Fprintf(out, "changed %s\n", termsafe.Field(checkup.ShowPath(p.File, home)))
	}
	if runID != "" {
		fmt.Fprintf(out, "Set up (run %s). aggy unwire --run %s undoes the changes.\n", runID, runID)
	}
	for _, p := range w.problems {
		fmt.Fprintf(out, "Not set up: %s\n", termsafe.Line(p))
	}
	for _, l := range w.left {
		fmt.Fprintf(out, "Left to you: %s\n", termsafe.Line(l))
	}
}
