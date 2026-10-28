package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const setupUsage = `Usage: aggy setup [--yes] [--only VENDORS]

Checks Claude Code, Codex, and Grok Build one at a time: finds each CLI
on PATH, reads its version and capabilities, and asks whether it is
signed in. You can skip any vendor and run setup again later. It
records only, per vendor, whether the CLI is signed in, its version, and
when it was checked. aggy never signs in for you and never reads, copies,
or stores a login; each CLI signs in with its own flow.

  --yes          check without asking (also when stdin is not a terminal)
  --only LIST    check only these vendors, e.g. --only claude,codex
`

// probeTimeout bounds each run of a vendor CLI during setup and doctor.
const probeTimeout = 30 * time.Second

// vendorNames are the vendors' product names, for output.
var vendorNames = map[adapter.Vendor]string{
	adapter.Claude: "Claude Code",
	adapter.Codex:  "Codex",
	adapter.Grok:   "Grok Build",
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
}

// defaultCheckEnv is the real world: the three adapters, the process
// runner, and the process's stdin.
func defaultCheckEnv() checkEnv {
	return checkEnv{
		adapters: []adapter.Adapter{claude.New(), codex.New(), grok.New()},
		run:      execrun.ExecRunner(probeTimeout),
		in:       os.Stdin,
		tty:      isTerminal(os.Stdin),
		home:     os.Getenv("HOME"),
		environ:  os.Environ(),
		now:      time.Now,
	}
}

// isTerminal reports whether f is a character device, as a terminal is.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
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
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, setupUsage)
			return 0
		}
		fmt.Fprintf(stderr, "aggy setup: %v\n\n%s", err, setupUsage)
		return 2
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "aggy setup: unexpected arguments\n\n%s", setupUsage)
		return 2
	}
	selected, err := parseOnly(*only, env.adapters)
	if err != nil {
		fmt.Fprintf(stderr, "aggy setup: %v\n", termsafe.Field(err.Error()))
		return 2
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aggy setup: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	defer func() { _ = s.Close() }()

	st := &setupRun{env: env, ask: env.tty && !*yes, in: bufio.NewReader(env.in), out: stdout, store: s,
		findings: checkup.Environment(env.environ)}
	if st.ask {
		fmt.Fprintln(stdout, "aggy setup checks each vendor CLI in turn. Answer n to skip one; run aggy setup again any time.")
	}
	code := 0
	for _, a := range selected {
		c, stop := st.vendor(ctx, a)
		if c != 0 {
			fmt.Fprintf(stderr, "aggy setup: %s\n", termsafe.Field(st.err.Error()))
			code = c
		}
		if stop {
			break
		}
	}
	fmt.Fprintln(stdout, "\nKeeping instructions, memory, and safety rules in step across the CLIs comes in a later release.")
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

// answer prints prompt and reads one line, lower-cased and trimmed. ok
// is false at the end of input.
func (r *setupRun) answer(prompt string) (string, bool) {
	fmt.Fprint(r.out, prompt)
	line, err := r.in.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(r.out)
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(line)), true
}

// vendor sets up one vendor. It returns a non-zero code when recording
// failed, and stop when the person's input ended.
func (r *setupRun) vendor(ctx context.Context, a adapter.Adapter) (code int, stop bool) {
	v := a.Vendor()
	name := vendorNames[v]
	fmt.Fprintf(r.out, "\n%s\n", name)
	if r.ask {
		ans, ok := r.answer(fmt.Sprintf("  Check %s now? [Y/n] ", name))
		if !ok {
			return 0, true
		}
		if ans == "n" || ans == "no" || ans == "s" || ans == "skip" {
			fmt.Fprintln(r.out, "  skipped")
			return 0, false
		}
	}
	res := checkup.CheckVendor(ctx, a, r.env.run)
	r.printVendor(res)
	for _, f := range checkup.ForVendor(r.findings, v) {
		fmt.Fprintf(r.out, "  %s: %s\n", f.Level, termsafe.Field(f.Message))
	}
	steps := a.SignIn().Steps
	switch {
	case !res.Installed:
		fmt.Fprintf(r.out, "  Install %s, then run aggy setup again.\n", name)
	case res.SignIn == adapter.SignedOut:
		fmt.Fprintf(r.out, "  Sign in with %s's own flow: %s. aggy never sees your login.\n", name, steps)
		for r.ask && res.SignIn == adapter.SignedOut {
			ans, ok := r.answer("  Press Enter to check again once you have signed in, or type s to skip: ")
			if !ok {
				stop = true
				break
			}
			if ans == "s" || ans == "skip" {
				break
			}
			res = checkup.CheckVendor(ctx, a, r.env.run)
			fmt.Fprintf(r.out, "  sign-in: %s\n", signInText(res))
		}
	case res.SignIn == adapter.SignInUnknown && res.Err == nil:
		fmt.Fprintf(r.out, "  aggy has no verified way to ask %s whether it is signed in. If it is not: %s.\n", name, steps)
	}
	rec := store.VendorStatus{
		Vendor: string(v), Connected: string(res.Connected()), Version: res.Version,
		CheckedAt: r.env.now().UTC(),
	}
	if err := r.store.PutVendorStatus(ctx, rec); err != nil {
		r.err = err
		return 1, stop
	}
	fmt.Fprintf(r.out, "  recorded: %s\n", recordText(rec))
	return 0, stop
}

// printVendor prints what a check found. The version, path, warning, and
// error come from the CLI or the disk, so they go through termsafe.
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
	if res.VersionWarning != "" {
		fmt.Fprintf(r.out, "  warning: %s\n", termsafe.Field(res.VersionWarning))
	}
	if res.Err != nil {
		fmt.Fprintf(r.out, "  error: %s\n", termsafe.Field(res.Err.Error()))
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
