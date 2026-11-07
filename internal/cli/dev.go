package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/devfix"
	"github.com/congdoe/aggy/internal/diag"
)

// devConfirmFlag must be given to aggy dev capture, which runs a real
// vendor CLI and may spend its quota.
const devConfirmFlag = "i-know-this-runs-the-real-cli"

const devUsage = `aggy dev is for maintainers recording vendor CLI fixtures.

Usage:
  aggy dev capture <vendor> --` + devConfirmFlag + ` (--name NAME | --out DIR)
                   [--bin PATH] [--env AGGY_X=V]... [--timeout 5m] -- <args>
  aggy dev scrub [--anywhere] <fixture dir>

Run them inside the aggy repo. capture runs the real CLI once (it may
spend quota), records its stdout, stderr, exit code, and the text files of
its session into testdata/fixtures/<vendor>/<name> (or --out, which must
also be under testdata/fixtures), and scrubs the result. scrub rewrites a
fixture under testdata/fixtures (anywhere with --anywhere), replacing home
paths, emails, usernames, hostnames, session UUIDs, and token-shaped
strings. Review a fixture before committing it.
`

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func runDev(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return cmdUsage(stderr, "dev", "name a subcommand", devUsage)
	}
	switch args[0] {
	case "capture":
		return runDevCapture(args[1:], stdout, stderr)
	case "scrub":
		return runDevScrub(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, devUsage)
		return 0
	}
	return cmdUsage(stderr, "dev", fmt.Sprintf("unknown command %q", args[0]), devUsage)
}

func runDevCapture(args []string, stdout, stderr io.Writer) int {
	var cliArgs []string
	for i, a := range args {
		if a == "--" {
			args, cliArgs = args[:i], args[i+1:]
			break
		}
	}
	fs := flag.NewFlagSet("aggy dev capture", flag.ContinueOnError)
	fs.SetOutput(stderr)
	confirm := fs.Bool(devConfirmFlag, false, "confirm running the real vendor CLI")
	name := fs.String("name", "", "fixture name under testdata/fixtures/<vendor>/")
	out := fs.String("out", "", "fixture directory, instead of --name")
	bin := fs.String("bin", "", "absolute path of the CLI (default: the vendor's program on PATH)")
	timeout := fs.Duration("timeout", 5*time.Minute, "time limit for the run")
	var env multi
	fs.Var(&env, "env", "extra AGGY_* variable for the CLI, KEY=VALUE (repeatable)")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return cmdUsage(stderr, "dev capture", "name a vendor", devUsage)
	}
	vendor, err := adapter.ParseVendor(args[0])
	if err != nil {
		return cmdUsage(stderr, "dev capture", err.Error(), "")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return cmdUsage(stderr, "dev capture", "put the CLI's arguments after --", "")
	}
	if !*confirm {
		return cmdUsage(stderr, "dev capture", fmt.Sprintf("this runs the real %s CLI and may spend its quota; rerun with --%s to confirm", vendor, devConfirmFlag), "")
	}
	dir := *out
	switch {
	case dir == "" && *name == "":
		return cmdUsage(stderr, "dev capture", "give --name or --out", "")
	case dir == "" && (*name != filepath.Base(*name) || strings.HasPrefix(*name, ".")):
		return cmdUsage(stderr, "dev capture", fmt.Sprintf("bad fixture name %q", *name), "")
	}
	root, err := devfix.RepoRoot(".")
	if err != nil {
		return cmdErr(stderr, "dev capture", diag.CodeCommandFailed, err)
	}
	if dir == "" {
		dir = filepath.Join(devfix.FixturesDir(root), string(vendor), *name)
	}
	if dir, err = devfix.UnderFixtures(root, dir); err != nil {
		return cmdUsage(stderr, "dev capture", err.Error(), "")
	}
	meta, err := devfix.Capture(context.Background(), devfix.CaptureOptions{
		Vendor: vendor, Bin: *bin, Args: cliArgs, Out: dir, Env: env, Timeout: *timeout,
	})
	if err != nil {
		return cmdErr(stderr, "dev capture", diag.CodeCommandFailed, err)
	}
	fmt.Fprintf(stdout, "captured %s: exit %d, %d files, %d skipped\nreview it, then run the fixture check: go test ./internal/devfix\n",
		dir, meta.ExitCode, len(meta.Files), len(meta.Skipped))
	return 0
}

func runDevScrub(args []string, stdout, stderr io.Writer) int {
	anywhere := len(args) > 0 && args[0] == "--anywhere"
	if anywhere {
		args = args[1:]
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return cmdUsage(stderr, "dev scrub", "give one fixture directory", devUsage)
	}
	dir, err := filepath.Abs(args[0])
	if err != nil {
		return cmdErr(stderr, "dev scrub", diag.CodeCommandFailed, err)
	}
	if !anywhere {
		root, err := devfix.RepoRoot(".")
		if err == nil {
			dir, err = devfix.UnderFixtures(root, dir)
		}
		if err != nil {
			return cmdUsage(stderr, "dev scrub", err.Error()+" (use --anywhere to scrub elsewhere)", "")
		}
	}
	changed, err := devfix.ScrubDir(dir, devfix.NewScrubber(devfix.CurrentIdentity()))
	if err != nil {
		return cmdErr(stderr, "dev scrub", diag.CodeCommandFailed, err)
	}
	for _, c := range changed {
		fmt.Fprintf(stdout, "scrubbed %s\n", c)
	}
	findings, err := devfix.Detect(dir, devfix.CurrentIdentity(), nil)
	if err != nil {
		return cmdErr(stderr, "dev scrub", diag.CodeCommandFailed, err)
	}
	for _, f := range findings {
		fmt.Fprintf(stderr, "still present: %s\n", f)
	}
	if len(findings) > 0 {
		return cmdFail(stderr, "dev scrub", diag.CodeCommandFailed, "secret-shaped text is still present")
	}
	return 0
}
