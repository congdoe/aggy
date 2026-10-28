package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/devfix"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const doctorUsage = `Usage: aggy doctor [--report]

Checks each vendor CLI (installed, version against the versions aggy was
tested on, sign-in), API-key and base-URL variables and vendor config
settings that can bill an API instead of the subscription, and whether
config files aggy changed have changed since. It changes nothing.

  --report   print a version safe to paste into a public issue: home
             paths, user and host names, emails, and token-shaped text
             are scrubbed, and no environment value or file content is
             included

Levels: error means aggy cannot work as set up (no vendor CLI, a CLI
that fails its version check, a changed file aggy cannot check);
warning may cost money or surprise you (API keys, base URLs, an untested
version, not signed in, a changed file); info needs no action. The exit
status is 1 when any finding is an error.
`

// Finding codes doctor adds to checkup's.
const (
	codeDrift          = "config-drift"     // warning: a file aggy changed has changed since
	codeDriftUnchecked = "drift-unchecked"  // error: a file aggy changed cannot be checked
	codeNoDrift        = "no-drift"         // info
	codeDriftBlocked   = "drift-blocked"    // info: an older change waits on a newer one
	codeIndex          = "index-unreadable" // error: the index cannot be read
	codeIndexOld       = "index-old"        // warning: the index predates drift checks
	codeNoIndex        = "no-index"         // info: aggy has recorded nothing yet
)

// driftSchema is the oldest index schema whose changes have a kind.
const driftSchema = 5

// doctorSection is a heading and its findings.
type doctorSection struct {
	title    string
	findings []checkup.Finding
}

// doctorResult is what aggy doctor found.
type doctorResult struct {
	sections []doctorSection
	versions map[adapter.Vendor]string // "" when not installed; "unreadable" when unreadable
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	return doctorWith(args, defaultCheckEnv(), stdout, stderr)
}

func doctorWith(args []string, env checkEnv, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	report := fl.Bool("report", false, "")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, doctorUsage)
			return 0
		}
		fmt.Fprintf(stderr, "aggy doctor: %v\n\n%s", err, doctorUsage)
		return 2
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "aggy doctor: unexpected arguments\n\n%s", doctorUsage)
		return 2
	}
	res := doctor(context.Background(), env)
	if *report {
		id := devfix.CurrentIdentity()
		if env.identity != nil {
			id = *env.identity
		}
		printReport(stdout, res, id)
	} else {
		printDoctor(stdout, res)
	}
	for _, s := range res.sections {
		for _, f := range s.findings {
			if f.Level == checkup.LevelError {
				return 1
			}
		}
	}
	return 0
}

// doctor runs every check. It writes nothing.
func doctor(ctx context.Context, env checkEnv) doctorResult {
	res := doctorResult{versions: map[adapter.Vendor]string{}}
	envFindings := checkup.Environment(env.environ)
	installed := 0
	for _, a := range env.adapters {
		v := a.Vendor()
		name := vendorNames[v]
		vr := checkup.CheckVendor(ctx, a, env.run)
		if vr.Installed {
			installed++
			res.versions[v] = vr.Version
			if vr.Version == "" {
				res.versions[v] = "unreadable"
			}
		} else {
			res.versions[v] = ""
		}
		fs := checkup.VendorFindings(vr, name, a.SignIn().Steps, env.home)
		fs = append(fs, checkup.ForVendor(envFindings, v)...)
		res.sections = append(res.sections, doctorSection{title: name, findings: fs})
	}
	var general []checkup.Finding
	if installed == 0 {
		general = append(general, checkup.Finding{Code: checkup.CodeNoCLI, Level: checkup.LevelError,
			Message: "no vendor CLI is installed: install Claude Code, Codex, or Grok Build, then run aggy setup"})
	}
	general = append(general, checkup.ForVendor(envFindings, "")...)
	if len(general) > 0 {
		res.sections = append(res.sections, doctorSection{title: "General", findings: general})
	}
	res.sections = append(res.sections, doctorSection{title: "Config aggy changed", findings: driftFindings(ctx, env.home)})
	return res
}

// driftFindings checks the files aggy changed, reading the index
// read-only: doctor never creates or migrates it.
func driftFindings(ctx context.Context, home string) []checkup.Finding {
	f := func(code string, l checkup.Level, msg string) []checkup.Finding {
		return []checkup.Finding{{Code: code, Level: l, Message: msg}}
	}
	path, err := store.DefaultPath()
	if err != nil {
		return f(codeIndex, checkup.LevelError, "cannot find the index: "+err.Error())
	}
	if _, err := safefs.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return f(codeNoIndex, checkup.LevelInfo, "aggy has not changed any config")
	} else if err != nil {
		return f(codeIndex, checkup.LevelError, "cannot read the index: "+err.Error())
	}
	s, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return f(codeIndex, checkup.LevelError, "cannot read the index: "+err.Error())
	}
	defer func() { _ = s.Close() }()
	if v, err := s.Version(ctx); err != nil {
		return f(codeIndex, checkup.LevelError, "cannot read the index: "+store.ReadError(err).Error())
	} else if v < driftSchema {
		return f(codeIndexOld, checkup.LevelWarn, "the index is older than drift checks need; run aggy setup to upgrade it")
	}
	m, err := manifest.Open(s)
	if err != nil {
		return f(codeIndex, checkup.LevelError, err.Error())
	}
	ds, err := m.Drift(ctx)
	if err != nil {
		return f(codeIndex, checkup.LevelError, "cannot read the index: "+store.ReadError(err).Error())
	}
	var out []checkup.Finding
	add := func(code string, l checkup.Level, format string, a ...any) {
		out = append(out, checkup.Finding{Code: code, Level: l, Message: fmt.Sprintf(format, a...)})
	}
	for _, d := range ds {
		file, id := checkup.ShowPath(d.Change.File, home), d.Change.ChangeID
		switch d.Outcome {
		case manifest.Drifted:
			add(codeDrift, checkup.LevelWarn, "%s changed after aggy wrote it (change %s); aggy unwire will ask for --force", file, id)
		case manifest.Invalid:
			add(codeDriftUnchecked, checkup.LevelError, "change %s is not a change aggy could have made: %s", id, file)
		case manifest.Failed:
			if errors.Is(d.Err, safefs.ErrAuthPath) {
				add(codeDriftUnchecked, checkup.LevelError, "change %s names a vendor credential file, which aggy never reads: %s", id, file)
			} else {
				add(codeDriftUnchecked, checkup.LevelError, "%s cannot be checked (change %s): %v", file, id, d.Err)
			}
		case manifest.Blocked:
			// Only an older change waiting on a newer one; --all
			// handles it. Info, not a problem.
			add(codeDriftBlocked, checkup.LevelInfo, "change %s to %s waits for a newer change to the file; aggy unwire --all undoes them in order", id, file)
		}
		// Restored, Removed, Cleaned, Superseded: unwire undoes them as
		// they are. Not drift.
	}
	if len(out) == 0 {
		n := len(ds)
		msg := fmt.Sprintf("%d %s aggy made %s undone cleanly by aggy unwire", n, plural(n, "change", "changes"), plural(n, "can be", "can all be"))
		if n == 0 {
			msg = "no config change of aggy's is in effect"
		}
		return f(codeNoDrift, checkup.LevelInfo, msg)
	}
	return out
}

// printDoctor prints the findings by section. Every message may hold
// text from a CLI or the disk, so it goes through termsafe.
func printDoctor(w io.Writer, res doctorResult) {
	counts := map[checkup.Level]int{}
	for i, s := range res.sections {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, s.title)
		for _, f := range s.findings {
			counts[f.Level]++
			fmt.Fprintf(w, "  %-7s %s: %s\n", f.Level, f.Code, termsafe.Field(f.Message))
		}
	}
	fmt.Fprintf(w, "\n%d %s, %d %s\n", counts[checkup.LevelError], plural(counts[checkup.LevelError], "error", "errors"),
		counts[checkup.LevelWarn], plural(counts[checkup.LevelWarn], "warning", "warnings"))
}

// printReport prints the findings for a public issue: aggy's version,
// the OS and architecture, each vendor's CLI version, and each finding's
// level, code, vendor, and message, scrubbed (reportScrub, then devfix's
// scrubber: home paths, user and host names, emails, UUIDs, and
// token-shaped strings), then escaped.
// Messages never hold environment values or file contents.
func printReport(w io.Writer, res doctorResult, id devfix.Identity) {
	sc := devfix.NewScrubber(id)
	clean := func(s string) string { return termsafe.Field(sc.String(reportScrub(s, id))) }
	var b strings.Builder
	fmt.Fprintln(&b, "aggy doctor report")
	fmt.Fprintf(&b, "aggy: %s\n", clean(Version))
	fmt.Fprintf(&b, "os: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	for _, v := range adapter.Vendors() {
		ver, ok := res.versions[v]
		switch {
		case !ok:
			continue
		case ver == "":
			ver = "not installed"
		}
		fmt.Fprintf(&b, "%s: %s\n", v, clean(ver))
	}
	fmt.Fprintln(&b, "findings:")
	for _, s := range res.sections {
		for _, f := range s.findings {
			vendor := ""
			if f.Vendor != "" {
				vendor = " (" + string(f.Vendor) + ")"
			}
			fmt.Fprintf(&b, "  %s %s%s: %s\n", f.Level, f.Code, vendor, clean(f.Message))
		}
	}
	fmt.Fprint(w, b.String())
}

// absPathRE matches an absolute path of two or more elements that starts
// a word: not after a letter, digit, or one of "_.~-/". A one-element
// "/login" is left alone.
var absPathRE = regexp.MustCompile(`(^|[^A-Za-z0-9_.~/-])(/[^\s/'"` + "`" + `(),;]+(?:/[^\s/'"` + "`" + `(),;]+)+)`)

// genericUsers are account names too common to identify anyone, which
// reportScrub leaves in place rather than mangle ordinary words.
var genericUsers = []string{"root", "runner", "user", "users", "admin", "ubuntu", "vscode", "codespace", "build", "home", "bin", "local"}

// reportScrub removes what devfix's fixture scrubber does not, for
// doctor --report: the home directory becomes ~; any other absolute path
// becomes <path>/<its base name>, since paths such as
// /etc/profiles/per-user/<user>/bin or a relocated vendor home hold the
// user name; and the user name is replaced wherever it stands as a word
// (bounded by anything but a letter, digit, or underscore, so "cc-<user>"
// and "/<user>/" match), unless it is shorter than 3 characters or
// generic.
func reportScrub(s string, id devfix.Identity) string {
	if h := strings.TrimRight(id.Home, "/"); len(h) > 1 {
		s = strings.ReplaceAll(s, h+"/", "~/")
		s = regexp.MustCompile(regexp.QuoteMeta(h)+`($|[^A-Za-z0-9_.-])`).ReplaceAllString(s, "~$1")
	}
	s = absPathRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := absPathRE.FindStringSubmatch(m)
		return sub[1] + "<path>/" + filepath.Base(sub[2])
	})
	u := id.User
	if len(u) >= 3 && !strings.ContainsAny(u, "/\\") && !slices.ContainsFunc(genericUsers, func(g string) bool { return strings.EqualFold(g, u) }) {
		re := regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(u) + `($|[^A-Za-z0-9_])`)
		// A match consumes its boundary, so adjacent names need passes.
		for range 8 {
			out := re.ReplaceAllString(s, "${1}"+devfix.UserPlaceholder+"${2}")
			if out == s {
				break
			}
			s = out
		}
	}
	return s
}
