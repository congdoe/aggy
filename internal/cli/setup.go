package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/antigravity"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/adapter/hermes"
	"github.com/congdoe/aggy/internal/adapter/opencode"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/devfix"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/skills"
)

const setupUsage = `Usage: aggy setup [--yes] [--only VENDORS] [--wire [--dry-run]]

Checks Claude Code, Codex, and Grok Build one at a time: finds each CLI
on PATH, reads its version and capabilities, and asks whether it is
signed in. You can skip any vendor and run setup again later. It
records only, per vendor, whether the CLI is signed in, its version, and
when it was checked. aggy never signs in for you and never reads, copies,
or stores a login; each CLI signs in with its own flow.

Last, it updates aggy's linked skills to this version, and at a terminal
offers to link them (/aggy, /switch, /borrow, and /inbox in each CLI's
menu; aggy skill link); with --yes, or without a terminal, it only says
how.

  --yes          check without asking (also when stdin is not a terminal)
  --only LIST    check only these vendors, e.g. --only claude,codex
  --wire         then also do what aggy hooks install and aggy policy wire
                 do, for each of claude, codex, and grok on PATH: aggy's hooks
                 in each CLI's hooks file and Codex's rules file, through the
                 manifest (aggy unwire undoes it). Bare aggy offers the same
                 the first time it runs at a terminal.
  --dry-run      with --wire: show the changes and make none
`

// probeTimeout bounds each run of a vendor CLI during setup and doctor.
const probeTimeout = 30 * time.Second

// vendorNames are the vendors' product names, for output.
var vendorNames = map[adapter.Vendor]string{
	adapter.Claude:   "Claude Code",
	adapter.Codex:    "Codex",
	adapter.Grok:     "Grok Build",
	adapter.OpenCode: "OpenCode",
	adapter.Hermes:   "Hermes Agent",
	// T6957: the product is Antigravity, the plan Gemini's.
	adapter.Antigravity: "Antigravity",
}

// checkEnv is what setup and doctor need from the outside world, so tests
// can give them fake CLIs and a scripted terminal.
type checkEnv struct {
	adapters []adapter.Adapter
	run      adapter.Runner
	in       io.Reader // the person's answers
	tty      bool      // in is a terminal
	home     string    // HOME, to show paths as ~
	environ  []string  // aggy's environment; only names are ever reported
	now      func() time.Time
	identity *devfix.Identity // what doctor --report scrubs; nil means the running user's
	cwd      string           // where doctor looks for the repo's shared memory; "" skips it
}

// defaultCheckEnv is the real world: the three adapters, the process
// runner, and the process's stdin.
func defaultCheckEnv() checkEnv {
	cwd, _ := os.Getwd()
	return checkEnv{
		adapters: []adapter.Adapter{claude.New(), codex.New(), grok.New(), opencode.New(), hermes.New(), antigravity.New()},
		run:      execrun.ExecRunner(probeTimeout),
		in:       os.Stdin,
		tty:      isTerminal(os.Stdin),
		home:     os.Getenv("HOME"),
		environ:  os.Environ(),
		now:      time.Now,
		cwd:      cwd,
	}
}

// isTerminal reports whether f looks like a terminal: a character device
// that is not /dev/null. /dev/null is a character device too, and a
// setup run with </dev/null must not prompt. Without a termios ioctl
// (golang.org/x/sys and raw syscalls are banned) this is a heuristic:
// another character device such as /dev/zero passes. Its first answer
// then runs past maxAnswer with no newline, which setup, like the end of
// input, takes as no terminal.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := safefs.Stat(os.DevNull)
	return err != nil || !os.SameFile(fi, null)
}

// parseOnly returns the adapters named in list (comma-separated), in
// their usual order; an empty list is all of them.
func parseOnly(list string, all []adapter.Adapter) ([]adapter.Adapter, error) {
	if list == "" {
		return all, nil
	}
	want := map[adapter.Vendor]bool{}
	for _, s := range strings.Split(list, ",") {
		v, err := adapter.ParseVendor(strings.TrimSpace(s))
		if err != nil {
			return nil, err
		}
		want[v] = true
	}
	var out []adapter.Adapter
	for _, a := range all {
		if want[a.Vendor()] {
			out = append(out, a)
		}
	}
	return out, nil
}

func runSetup(args []string, stdout, stderr io.Writer) int {
	return setupWith(args, defaultCheckEnv(), stdout, stderr)
}

func setupWith(args []string, env checkEnv, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("setup", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	yes := fl.Bool("yes", false, "")
	only := fl.String("only", "", "")
	wire := fl.Bool("wire", false, "")
	dryRun := fl.Bool("dry-run", false, "")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, setupUsage)
			return 0
		}
		refuse(context.Background(), stderr, "setup", diag.EvSetupFailed, diag.CodeUsage, err.Error(), errRec(diag.Rec{}, err))
		fmt.Fprintf(stderr, "\n%s", setupUsage)
		return 2
	}
	if fl.NArg() > 0 {
		refuse(context.Background(), stderr, "setup", diag.EvSetupFailed, diag.CodeUsage, "unexpected arguments", diag.Rec{})
		fmt.Fprintf(stderr, "\n%s", setupUsage)
		return 2
	}
	selected, err := parseOnly(*only, env.adapters)
	if err != nil {
		refuse(context.Background(), stderr, "setup", diag.EvSetupFailed, diag.CodeUsage, err.Error(), errRec(diag.Rec{}, err))
		return 2
	}
	if *dryRun {
		// T6966: a dry run opens no index and writes nothing.
		if !*wire {
			refuse(context.Background(), stderr, "setup", diag.EvSetupFailed, diag.CodeUsage, "--dry-run goes with --wire", diag.Rec{})
			return 2
		}
		w := planWire(env.home)
		if _, err := manifest.Preview(stdout, w.patches); err != nil {
			refuse(context.Background(), stderr, "setup", diag.EvSetupFailed, diag.CodeWireFailed, err.Error(), errRec(diag.Rec{}, err))
			return 1
		}
		reportWire(stdout, env.home, wirePlan{problems: w.problems, left: w.left}, "")
		return 0
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		refuse(ctx, stderr, "setup", diag.EvSetupFailed, diag.CodeStore, err.Error(), errRec(diag.Rec{}, err))
		return 1
	}
	defer func() { _ = s.Close() }()

	st := &setupRun{env: env, ask: env.tty && !*yes, in: bufio.NewReader(env.in), out: stdout, store: s,
		findings: checkup.Environment(env.environ)}
	if st.ask {
		fmt.Fprintln(stdout, "aggy setup checks each vendor CLI in turn. Answer n to skip one; run aggy setup again any time.")
	}
	// Findings no one vendor owns, such as OPENAI_BASE_URL, which Codex
	// does not read but other OpenAI tools may.
	for _, f := range checkup.ForVendor(st.findings, "") {
		fmt.Fprintf(stdout, "%s: %s\n", f.Level, termsafe.Field(f.Text(env.home)))
	}
	code := 0
	for _, a := range selected {
		if c := st.vendor(ctx, a); c != 0 {
			refuse(ctx, stderr, "setup", diag.EvSetupFailed, diag.CodeStore, st.err.Error(), errRec(diag.Rec{Vendor: string(a.Vendor())}, st.err))
			code = c
		}
	}
	st.skills(ctx, selected)
	if *wire {
		// TODO(T6954): without a terminal, --wire writes without asking, as
		// aggy hooks install does; --dry-run shows the changes first.
		w := planWire(env.home)
		fmt.Fprintln(stdout)
		runID, err := applyWire(ctx, s, w)
		if err != nil {
			refuse(ctx, stderr, "setup", diag.EvSetupFailed, diag.CodeWireFailed, err.Error(), errRec(diag.Rec{}, err))
			return 1
		}
		reportWire(stdout, env.home, w, runID)
		fmt.Fprintln(stdout, "\nRun aggy memory import --from claude to keep memory in step across the CLIs.")
		return code
	}
	fmt.Fprintln(stdout, "\nRun aggy hooks install so aggy hears when a session starts, a turn ends, or a CLI hits a limit; aggy hooks status shows what is installed.")
	fmt.Fprintln(stdout, "\nRun aggy policy wire and aggy memory import --from claude to keep safety rules and memory in step across the CLIs.")
	return code
}

// setupRun is one aggy setup.
type setupRun struct {
	env   checkEnv
	ask   bool
	in    *bufio.Reader
	out   io.Writer
	store *store.Store
	err   error
	// findings are the environment and config checks, shown with the
	// vendor they concern.
	findings []checkup.Finding
}

// maxAnswer caps an answer line: a line of maxAnswer bytes or more
// before its newline is refused, so the longest answer taken is
// maxAnswer-1 bytes. A person types a few bytes; a device that never
// sends a newline (</dev/zero) must not be read forever.
const maxAnswer = 4 << 10

// errNotTerminal means the input ran past maxAnswer with no newline.
var errNotTerminal = errors.New("an answer longer than any a person types")

// readAnswer reads one line of at most maxAnswer bytes, without its
// newline.
func (r *setupRun) readAnswer() (string, error) {
	var b []byte
	for len(b) < maxAnswer {
		c, err := r.in.ReadByte()
		if err != nil {
			return string(b), err
		}
		if c == '\n' {
			return string(b), nil
		}
		b = append(b, c)
	}
	return "", errNotTerminal
}

// answer prints prompt and reads one answer, lower-cased and trimmed. At
// the end of input, or on a line longer than maxAnswer, it returns ok
// false and stops asking: setup goes on as with --yes, checking and
// recording the rest without prompts, so an input that ends never leaves
// vendors unrecorded.
func (r *setupRun) answer(prompt string) (string, bool) {
	fmt.Fprint(r.out, prompt)
	line, err := r.readAnswer()
	switch {
	case errors.Is(err, errNotTerminal):
		fmt.Fprintln(r.out)
		fmt.Fprintln(r.out, "  (the input is not a terminal: checking the rest without asking)")
		r.ask = false
		return "", false
	case err != nil && line == "":
		fmt.Fprintln(r.out)
		fmt.Fprintln(r.out, "  (no more input: checking the rest without asking)")
		r.ask = false
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(line)), true
}

// vendor sets up one vendor. It returns a non-zero code when recording
// failed.
func (r *setupRun) vendor(ctx context.Context, a adapter.Adapter) int {
	v := a.Vendor()
	name := vendorNames[v]
	fmt.Fprintf(r.out, "\n%s\n", name)
	if r.ask {
		ans, ok := r.answer(fmt.Sprintf("  Check %s now? [Y/n] ", name))
		if ok && (ans == "n" || ans == "no" || ans == "s" || ans == "skip") {
			fmt.Fprintln(r.out, "  skipped")
			return 0
		}
	}
	res := checkup.CheckVendor(ctx, a, r.env.run)
	r.printVendor(res)
	for _, f := range checkup.ForVendor(r.findings, v) {
		fmt.Fprintf(r.out, "  %s: %s\n", f.Level, termsafe.Field(f.Text(r.env.home)))
	}
	if v == adapter.OpenCode && res.Installed {
		fmt.Fprint(r.out, openCodeSetupNote)
	}
	if v == adapter.Antigravity && res.Installed {
		fmt.Fprint(r.out, antigravitySetupNote)
	}
	if v == adapter.Hermes && res.Installed {
		fmt.Fprint(r.out, hermesSetupNote)
		for _, f := range hermesFindings(ctx, res) {
			if f.Code == codeHermesAdopt {
				fmt.Fprintf(r.out, "  %s: %s\n", f.Level, termsafe.Field(f.Message))
			}
		}
	}
	steps := a.SignIn().Steps
	switch {
	case !res.Installed:
		fmt.Fprintf(r.out, "  Install %s, then run aggy setup again.\n", name)
	case res.SignIn == adapter.SignedOut:
		fmt.Fprintf(r.out, "  Sign in with %s's own flow: %s. aggy never sees your login.\n", name, steps)
		for r.ask && res.SignIn == adapter.SignedOut {
			ans, ok := r.answer("  Press Enter to check again once you have signed in, or type s to skip: ")
			if !ok || ans == "s" || ans == "skip" {
				break
			}
			res = checkup.CheckVendor(ctx, a, r.env.run)
			fmt.Fprintf(r.out, "  sign-in: %s\n", signInText(res))
		}
	case res.SignIn == adapter.SignInUnknown && res.ProbeErr == nil && res.SignInErr == nil:
		fmt.Fprintf(r.out, "  aggy has no verified way to ask %s whether it is signed in. If it is not: %s.\n", name, steps)
	}
	rec := store.VendorStatus{
		Vendor: string(v), Connected: string(res.Connected()), Version: res.Version,
		CheckedAt: r.env.now().UTC(),
	}
	if err := r.store.PutVendorStatus(ctx, rec); err != nil {
		r.err = err
		return 1
	}
	dlog.Log(ctx, diag.Rec{Event: diag.EvSetupChecked, Vendor: string(v),
		Attrs: map[string]any{"installed": res.Installed, "connected": rec.Connected, "version": rec.Version}})
	fmt.Fprintf(r.out, "  recorded: %s\n", recordText(rec))
	return 0
}

// printVendor prints what a check found. The version, path, and error come
// from the CLI or the disk, so they go through termsafe. A version outside
// the tested range is aggy doctor's to warn about (design section 7).
func (r *setupRun) printVendor(res checkup.Vendor) {
	if !res.Installed {
		fmt.Fprintf(r.out, "  not found on PATH\n")
		return
	}
	version := res.Version
	if version == "" {
		version = "unreadable"
	}
	fmt.Fprintf(r.out, "  found %s, version %s\n", termsafe.Field(checkup.ShowPath(res.Path, r.env.home)), termsafe.Field(version))
	for _, err := range []error{res.ProbeErr, res.SignInErr} {
		if err != nil {
			fmt.Fprintf(r.out, "  error: %s\n", termsafe.Field(err.Error()))
		}
	}
	fmt.Fprintf(r.out, "  sign-in: %s\n", signInText(res))
}

// signInText says what is known of a CLI's sign-in.
func signInText(res checkup.Vendor) string {
	switch {
	case !res.Installed:
		return "not installed"
	case res.SignIn == adapter.SignedIn:
		return "signed in"
	case res.SignIn == adapter.SignedOut:
		return "not signed in"
	}
	return "unknown"
}

// recordText describes a recorded row.
func recordText(v store.VendorStatus) string {
	version := v.Version
	if version == "" {
		version = "no version"
	}
	return strings.ReplaceAll(v.Connected, "_", " ") + ", " + termsafe.Field(version)
}

// skills refreshes aggy's linked skills (T6911) and offers to link them into
// the selected CLIs that take skills (Claude Code, Codex, Grok Build) that do
// not have them all yet. It asks first, default no, since a linked skill costs
// context every turn; without asking it only says how. A failure is a warning:
// setup's records stand.
func (r *setupRun) skills(ctx context.Context, selected []adapter.Adapter) {
	refreshSkills(ctx, r.out)
	var vendors []string
	for _, a := range selected {
		if v := string(a.Vendor()); slices.Contains(skillVendors, v) {
			vendors = append(vendors, v)
		}
	}
	ls, err := ownLayouts()
	if err != nil || len(vendors) == 0 {
		return
	}
	// A folder under one of aggy's names that aggy did not write is left alone
	// (R121), and says so.
	ls, notes, err := ownWritable(ctx, ls)
	if err != nil {
		return
	}
	for _, n := range notes {
		fmt.Fprintf(r.out, "note: %s.\n", n)
	}
	want := map[string]bool{}
	for _, v := range vendors {
		for _, l := range ls {
			if ok, err := l.linked(l.links[v]); err != nil || !ok {
				want[v] = true
			}
		}
	}
	if len(want) == 0 {
		return
	}
	cost := ownSkillCost(ls, slices.Sorted(maps.Keys(want)))
	fmt.Fprintf(r.out, "\naggy's skills put /aggy (help), /switch, /borrow, and /inbox in each CLI's menu ($aggy and so on in Codex); not linked yet for %s.\n", strings.Join(slices.Sorted(maps.Keys(want)), ", "))
	if !r.ask {
		fmt.Fprintf(r.out, "Run aggy skill link to link them (%s; aggy skill status shows each).\n", cost)
		return
	}
	ans, ok := r.answer(fmt.Sprintf("  Link them now? %s. [y/N] ", cost))
	if !ok || ans != "y" && ans != "yes" {
		fmt.Fprintln(r.out, "  not linked; aggy skill link does it later")
		return
	}
	if err := linkSkills(ctx, ls, want, r.out); err != nil {
		fmt.Fprintf(r.out, "  warning: the skills were not linked: %s\n", termsafe.Line(err.Error()))
	}
}

// ownSkillCost says what linking the skills of ls costs every turn in each of
// vendors, estimated offline (skills.Estimate): every skill's frontmatter,
// except a command skill's on Claude and Grok, whose disable-model-invocation
// keeps it out of the context there (docs/design.md section 5); Codex has no
// such key that is recorded, so there each one is paid.
func ownSkillCost(ls []skillLayout, vendors []string) string {
	var parts []string
	for _, v := range vendors {
		n := 0
		for _, l := range ls {
			if filepath.Base(l.store) != skillName && v != "codex" {
				continue
			}
			always, _ := skills.Estimate(l.content)
			n += always
		}
		parts = append(parts, fmt.Sprintf("about %d tokens every turn in %s", n, v))
	}
	return strings.Join(parts, ", ")
}
