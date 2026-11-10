package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/trustlist"
)

// The trust list (T6977, docs/design.md section 10, "Trust"): the first
// on-screen start in a folder aggy has no answer for, at a terminal, shows one
// list of what may run there and where its code may go; aggy trust <dir> and
// /aggy trust show the same list. Its rows are registered in trustrows.go. The
// answer, which rows were shown and which ticked, is kept in aggy's own trust
// file, through the manifest, so aggy unwire takes it back with what ticking
// changed, and a dry run writes nothing.

// trustAskEnv, set to 0, turns the start's list off, and its note with it (a
// scripted machine, the end-to-end tests).
const trustAskEnv = "AGGY_TRUST_ASK"

// trustTarget is the folder a list is for.
type trustTarget struct {
	dir    string // the folder asked about, by its real path
	root   string // its work-tree root, or dir outside a repo
	main   string // the repo's main checkout, or dir outside a repo
	key    string // the answer's key: the repo key, or dir's canonical path
	inRepo bool
}

// trustTargetFor finds the folder for dir, refusing / and the home folder.
func trustTargetFor(dir string) (trustTarget, error) {
	real, err := safefs.RealPath(dir)
	if err != nil {
		return trustTarget{}, err
	}
	tg := trustTarget{dir: real, root: real, main: real}
	repo, err := toolRepo(real, false)
	if err != nil {
		return trustTarget{}, err
	}
	if repo.main != "" {
		tg.root, tg.main, tg.key, tg.inRepo = repo.root, repo.main, repo.key, true
	} else if tg.key, err = safefs.CanonicalPath(real); err != nil {
		return trustTarget{}, err
	}
	if err := tooBroadToTrust(tg.dir, tg.root, tg.main); err != nil {
		return trustTarget{}, err
	}
	return tg, nil
}

// readTrustRecord reads aggy's trust file; one not there is empty.
func readTrustRecord(path string) (trustlist.File, error) {
	var f trustlist.File
	b, err := safefs.ReadConfig(path, config.MaxFileSize)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return f, nil
	case err != nil:
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("%s is not valid: %w", path, err)
	}
	return f, nil
}

// trustFileChange plans a change to a file of trustlist.File's shape: key
// added to its repos (repo > 0) or taken out (repo < 0), and folder's answer
// set when ans is set. changes is false when the file already says so.
func trustFileChange(path, key string, repo int, folder string, ans *trustlist.Folder) (p patch.Patch, changes bool, err error) {
	h := patch.NoFile
	b, err := safefs.ReadConfig(path, config.MaxFileSize)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		b = nil
	case err != nil:
		return p, false, err
	default:
		h = patch.Hash(b)
	}
	transform := func(src []byte) ([]byte, error) {
		var t trustlist.File
		if len(src) > 0 {
			if err := json.Unmarshal(src, &t); err != nil {
				return nil, fmt.Errorf("%s is not valid: %w", path, err)
			}
		}
		switch {
		case repo > 0 && !slices.Contains(t.Repos, key):
			t.Repos = append(t.Repos, key)
		case repo < 0:
			t.Repos = slices.DeleteFunc(t.Repos, func(r string) bool { return r == key })
		}
		slices.Sort(t.Repos)
		if t.Repos == nil {
			t.Repos = []string{}
		}
		if ans != nil {
			if t.Folders == nil {
				t.Folders = map[string]trustlist.Folder{}
			}
			t.Folders[folder] = *ans
		}
		out, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(out, '\n'), nil
	}
	got, err := transform(b)
	if err != nil {
		return p, false, err
	}
	// Only what the change means counts: a file that already says it (or no
	// file, for a change that adds nothing) is no change.
	var was, now trustlist.File
	if b != nil {
		_ = json.Unmarshal(b, &was)
	}
	_ = json.Unmarshal(got, &now)
	if was.Repos == nil {
		was.Repos = []string{}
	}
	slices.Sort(was.Repos)
	if string(mustJSON(was)) == string(mustJSON(now)) {
		return p, false, nil
	}
	return patch.Patch{File: path, ExpectedHash: h, Mode: 0o600, Transform: transform}, true, nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// trustRepoPatch adds key to (on) or takes it out of a repos file at path.
func trustRepoPatch(path, key string, on bool) (patch.Patch, bool, error) {
	repo := -1
	if on {
		repo = 1
	}
	return trustFileChange(path, key, repo, "", nil)
}

// trustListAt is one folder's list, being answered.
type trustListAt struct {
	tg       trustTarget
	defs     []trustRowDef
	prev     trustlist.Folder
	answered bool
	st       *trustlist.State
}

// openTrustList builds tg's list from the rows cfg offers (every harness when
// all is set) and what is recorded.
func openTrustList(tg trustTarget, cfg config.Config, all bool) (*trustListAt, error) {
	path, err := trustFile()
	if err != nil {
		return nil, err
	}
	f, err := readTrustRecord(path)
	if err != nil {
		return nil, err
	}
	l := &trustListAt{tg: tg, defs: trustRows(cfg, all)}
	l.prev, l.answered = f.Folders[tg.key]
	var rows []trustlist.Row
	for _, d := range l.defs {
		rows = append(rows, d.Row)
	}
	l.st = trustlist.NewState(rows, l.prev, l.answered, func(id string) bool {
		d, ok := l.def(id)
		return ok && d.current != nil && d.current(tg)
	})
	return l, nil
}

func (l *trustListAt) def(id string) (trustRowDef, bool) {
	for _, d := range l.defs {
		if d.ID == id {
			return d, true
		}
	}
	return trustRowDef{}, false
}

// pending are the rows a start would ask about.
func (l *trustListAt) pending() []string {
	var rows []trustlist.Row
	for _, d := range l.defs {
		rows = append(rows, d.Row)
	}
	return trustlist.Pending(rows, l.prev)
}

// tick sets which rows are ticked to ids.
func (l *trustListAt) tick(ids []string) {
	for i, r := range l.st.Rows {
		l.st.On[i] = slices.Contains(ids, r.ID)
	}
}

// aggyLine says what the answer means for aggy's own rules here.
func (l *trustListAt) aggyLine() string {
	if !l.tg.inRepo {
		return "this folder is in no git repo, so your allow rules from aggy do not apply here, trusted or not; the answer is kept for this folder."
	}
	return "ticking a harness trusts this repo for aggy's rules: your allow rules beyond the known read-only ones, those in its .claude/settings.local.json and your approvals from Codex among them, apply here."
}

// trustHeader is what the list says above its rows.
func (l *trustListAt) header(start bool) string {
	var b strings.Builder
	if start {
		fmt.Fprintf(&b, "aggy has no answer yet for %s.\n", termsafe.Field(l.tg.root))
		if l.answered {
			fmt.Fprintf(&b, "Something new is set up since you answered for %s; your earlier answers are kept.\n", termsafe.Field(l.tg.root))
		}
	} else {
		fmt.Fprintf(&b, "Trust %s?\n", termsafe.Field(l.tg.root))
	}
	b.WriteString("Pick what may run here, and where this folder's code may be sent.\n\n")
	fmt.Fprintf(&b, "- aggy: %s\n", l.aggyLine())
	// TODO(T6895): record the folder for Grok too, as a row, once a live check
	// says where Grok keeps a folder's trust (unverified).
	b.WriteString("- Grok is not on this list: aggy does not know where Grok keeps a folder's trust, so it asks you\n  itself. Once it trusts this folder, it loads the repo's committed .claude/settings.json, hooks\n  and allow rules included, which aggy cannot filter.\n")
	b.WriteString("- aggy gives guardrails against mistakes, not protection from a hostile repo or a tricked\n  model. A model's shell runs as you with no OS sandbox.\n\n")
	return b.String()
}

// plan is what the list as ticked changes: the answer, with never, and each
// row's change; explicit (aggy trust, /aggy trust) also changes Codex's key as
// aggy trust always has. Claude's file comes last.
func (l *trustListAt) plan(never, explicit bool) ([]patch.Patch, []string, error) {
	ans := l.st.Answer(l.prev, never)
	harness, harnessShown := false, false
	for i, r := range l.st.Rows {
		if r.Group == trustlist.Harness {
			harnessShown = true
			harness = harness || l.st.On[i]
		}
	}
	repo := 0
	var lines []string
	if !never && l.tg.inRepo && harnessShown {
		switch {
		case harness:
			repo = 1
		case repoTrusted(l.tg.key):
			repo = -1
			lines = append(lines, "aggy: no harness ticked, so this repo is no longer trusted for aggy's rules.")
		}
	}
	path, err := trustFile()
	if err != nil {
		return nil, nil, err
	}
	p, changes, err := trustFileChange(path, l.tg.key, repo, l.tg.key, &ans)
	if err != nil {
		return nil, nil, err
	}
	var ps, last []patch.Patch
	if changes {
		ps = append(ps, p)
	}
	if never {
		return ps, append(lines, "Never for this folder: aggy does not ask here again, and rows not ticked count as off. aggy trust "+l.tg.root+" shows the list again."), nil
	}
	for i, r := range l.st.Rows {
		d, _ := l.def(r.ID)
		if d.plan == nil {
			continue
		}
		rp, err := d.plan(l.tg, l.st.On[i])
		if err != nil {
			return nil, nil, err
		}
		ps, last = append(ps, rp.patches...), append(last, rp.last...)
		lines = append(lines, rp.lines...)
	}
	if explicit && harness {
		codexLine, codexPatch := codexTrust(l.tg.main)
		if codexPatch != nil {
			ps = append(ps, *codexPatch)
		}
		lines = append(lines, "Codex: "+codexLine)
	}
	return append(ps, last...), lines, nil
}

// readListAnswer reads one line typed to the list, dropping a line typed ahead
// of it (readAnswer). ok is false at the end of input, which answers nothing.
func readListAnswer(r io.Reader, asked time.Time) (string, bool) {
	for {
		line, err := readLine(r, maxAnswer)
		if err != nil {
			return "", false
		}
		if time.Since(asked) >= typeAhead {
			return line, true
		}
	}
}

// pasteStart and pasteEnd mark a bracketed paste, which picks nothing.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// askTrustList shows the list and reads lines until one confirms, says not
// now, or says never. A pasted line (bracketed), or the end of input, picks
// nothing; the end of input is not now.
func askTrustList(l *trustListAt, header string, in io.Reader, out io.Writer) trustlist.Choice {
	fmt.Fprint(out, header)
	pasting := false
	for range 64 {
		l.st.Write(out)
		fmt.Fprintf(out, "\n%s\n> ", trustlist.Keys)
		line, ok := readListAnswer(in, time.Now())
		if !ok {
			fmt.Fprintln(out)
			return trustlist.NotNow
		}
		if strings.Contains(line, pasteStart) {
			pasting = true
		}
		if pasting {
			pasting = !strings.Contains(line, pasteEnd)
			fmt.Fprintln(out, "A paste picks nothing here.")
			continue
		}
		switch c := l.st.Read(line); c {
		case trustlist.Confirm, trustlist.NotNow, trustlist.Never:
			return c
		case trustlist.Nothing:
			fmt.Fprintln(out, "That is not on the list.")
		}
	}
	return trustlist.NotNow
}

// applyTrustList writes the answer, with never, through the manifest as one
// run, and says what it did; dryRun only shows it. It returns the exit code.
func applyTrustList(ctx context.Context, l *trustListAt, never, explicit, dryRun bool, out, errw io.Writer) int {
	ps, lines, err := l.plan(never, explicit)
	if err != nil {
		refuse(ctx, errw, "trust", diag.EvTrustRefused, diag.CodeTrustBadFile, err.Error(), diag.Rec{})
		return 1
	}
	for _, line := range lines {
		fmt.Fprintln(out, termsafe.Lines(line))
	}
	if dryRun {
		if _, err := manifest.Preview(out, ps); err != nil {
			return 1
		}
		return 0
	}
	if len(ps) == 0 {
		fmt.Fprintln(out, "Nothing to change: this folder's answer is as it was.")
		return 0
	}
	runID, err := applyPatchesRun(ctx, ps)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, patch.ErrDrift) {
			msg = "a file changed while aggy was writing it (Claude rewrites its own config while it runs): " + msg + "; run aggy trust " + l.tg.root + " again"
		}
		if runID != "" {
			msg += "; what was written before it is run " + runID + ", which aggy unwire --run " + runID + " undoes"
		}
		refuse(ctx, errw, "trust", diag.EvTrustRefused, diag.CodeTrustBadFile, msg, diag.Rec{})
		return 1
	}
	fmt.Fprintf(out, "Saved this folder's answer (run %s). aggy unwire --run %s undoes it, once any trust made after it is undone.\n", runID, runID)
	return 0
}

// applyPatchesRun is applyPatches that also returns the run's id when a
// patch after the first is refused, so what was written can be named.
func applyPatchesRun(ctx context.Context, ps []patch.Patch) (string, error) {
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		return "", err
	}
	runID := manifest.NewRunID()
	cs, err := m.Run(ctx, manifest.Write, runID, ps, io.Discard)
	if err != nil {
		for _, c := range cs {
			if c.ChangeID != "" {
				return runID, err
			}
		}
		return "", err
	}
	return runID, nil
}

// trustLabels names rows by their labels.
func (l *trustListAt) labels(ids []string) string {
	var names []string
	for _, id := range ids {
		if d, ok := l.def(id); ok {
			names = append(names, d.Label)
		}
	}
	return strings.Join(names, ", ")
}

// trustPerson reports whether a person at a terminal can answer at a start:
// not a borrow child, not a session's own tool shell, and stdin and stderr
// terminals (as the first-run offer decides, offerTerminal).
func trustPerson(getenv func(string) string) bool {
	for _, k := range []string{"AGGY_DELEGATED", "AGGY_SOCKET", "AGGY_PARENT", "AGGY_VENDOR"} {
		if getenv(k) != "" {
			return false
		}
	}
	return offerTerminal()
}

// trustAtStart is the list at an on-screen start in cwd: asked when the folder
// has rows its record has not shown and a person is at the terminal; otherwise
// those rows count as off, and the line it returns says so for the launch's
// notes. / and the home folder are never offered. It returns "" when there is
// nothing to say.
func trustAtStart(ctx context.Context, cwd string, cfg config.Config, getenv func(string) string, in io.Reader, out io.Writer) string {
	if getenv(trustAskEnv) == "0" || getenv("AGGY_DELEGATED") != "" {
		return ""
	}
	tg, err := trustTargetFor(cwd)
	if err != nil {
		return ""
	}
	l, err := openTrustList(tg, cfg, false)
	if err != nil {
		return "aggy's trust file cannot be read, so nothing new counts as trusted here: " + err.Error()
	}
	pending := l.pending()
	if len(pending) == 0 {
		return ""
	}
	if !trustPerson(getenv) {
		return unansweredNote(l, pending)
	}
	switch askTrustList(l, l.header(true), in, out) {
	case trustlist.Confirm:
		applyTrustList(ctx, l, false, false, false, out, out)
	case trustlist.Never:
		applyTrustList(ctx, l, true, false, false, out, out)
	default:
		fmt.Fprintln(out, "Not now: aggy asks again at the next start here; until then what is not ticked counts as off.")
	}
	return ""
}

// unansweredNote is the launch's one line for rows not answered, with no one
// to ask.
func unansweredNote(l *trustListAt, pending []string) string {
	return fmt.Sprintf("this folder's trust list is not answered (no one at a terminal to ask), so %s count as off here; aggy trust %s answers it",
		l.labels(pending), l.tg.root)
}

// trustNoteFor is that line for a launch that does not ask (print mode): ""
// when there is nothing unanswered or the list is turned off.
func trustNoteFor(cwd string, cfg config.Config, getenv func(string) string) string {
	if getenv(trustAskEnv) == "0" || getenv("AGGY_DELEGATED") != "" {
		return ""
	}
	tg, err := trustTargetFor(cwd)
	if err != nil {
		return ""
	}
	l, err := openTrustList(tg, cfg, false)
	if err != nil {
		return ""
	}
	if p := l.pending(); len(p) > 0 {
		return unansweredNote(l, p)
	}
	return ""
}

// openCodeFreeHere reports whether this folder's list has OpenCode's free
// models ticked.
func openCodeFreeHere(dir string) bool {
	tg, err := trustTargetFor(dir)
	if err != nil {
		return false
	}
	path, err := trustFile()
	if err != nil {
		return false
	}
	f, err := readTrustRecord(path)
	if err != nil {
		return false
	}
	return slices.Contains(f.Folders[tg.key].On, trustRowOpenCodeFree)
}
