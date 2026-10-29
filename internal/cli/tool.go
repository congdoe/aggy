package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
)

const toolUsage = `Usage:
  aggy tool list
  aggy tool status [<name>] [--vendor V] [--repo DIR]
  aggy tool on <name> [--vendor V] [--repo DIR]
  aggy tool off <name> [--vendor V] [--repo DIR]
  aggy tool add <file> [--yes]
  aggy tool remove <name>

Managed tools are described by definitions: aggy's built-ins, and your
own in ~/.aggy/tools/<name>.toml. Definitions are never read from a repo.

  list     show each tool: name, built-in or user, default, estimated
           token cost per vendor here (0 where off), source file
  status   show whether each tool is on, per vendor, here (or in --repo
           DIR), and why; whether it is installed (its programs on PATH,
           its skill folder in place), ready (its skill pin matches), and
           wired into each vendor it is on for, as aggy doctor checks
  on, off  turn a tool on or off for every vendor, or one (--vendor),
           in every repo, or one (--repo DIR); the newest setting holds
           wherever it applies. A per-repo tool is turned on only with
           --repo. A repo's .aggy/config.toml can turn a tool off
           (tools_off) and never on. It takes effect at the next launch.
  add      validate a definition, show what it would run and wire per
           vendor, and copy it into ~/.aggy/tools after you answer y at a
           terminal (--yes skips the question; it never works without a
           terminal). A user tool starts off, and none of its commands
           runs while it is off.
  remove   delete a user definition; refused while the tool is on
`

func runTool(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "exec" {
		return runToolExec(args[1:], stderr) // a vendor CLI runs it; not in the usage
	}
	return toolWith(args, os.Stdin, isTerminal(os.Stdin), stdout, stderr)
}

// toolArgs are aggy tool's parsed arguments.
type toolArgs struct {
	pos          []string
	yes          bool
	vendor, repo string
	hasRepo      bool
}

func parseToolArgs(args []string) (toolArgs, bool) {
	var a toolArgs
	for i := 0; i < len(args); i++ {
		w := args[i]
		name, val, hasVal := strings.Cut(w, "=")
		switch {
		case w == "--yes":
			a.yes = true
		case name == "--vendor" || name == "--repo":
			if !hasVal {
				if i+1 >= len(args) {
					return a, false
				}
				i++
				val = args[i]
			}
			if name == "--vendor" {
				if a.vendor != "" {
					return a, false
				}
				a.vendor = val
			} else {
				if a.hasRepo || val == "" {
					return a, false
				}
				a.repo, a.hasRepo = val, true
			}
		case strings.HasPrefix(w, "-"):
			return a, false
		default:
			a.pos = append(a.pos, w)
		}
	}
	return a, true
}

func toolWith(args []string, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, toolUsage)
		return 2
	}
	ctx := context.Background()
	cmd := "tool " + args[0]
	usageErr := func() int {
		refuse(ctx, stderr, cmd, diag.EvToolRefused, diag.CodeUsage, "bad arguments", diag.Rec{})
		fmt.Fprintf(stderr, "\n%s", toolUsage)
		return 2
	}
	fail := func(err error) int {
		if !errors.Is(err, errDefsRefused) { // already printed, one line each
			toolRefuse(ctx, stderr, cmd, diag.EvToolRefused, err)
		}
		return 1
	}
	a, ok := parseToolArgs(args[1:])
	if !ok {
		return usageErr()
	}
	noSwitchFlags := a.vendor == "" && !a.hasRepo
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, toolUsage)
		return 0
	case "list":
		if len(a.pos) != 0 || a.yes || !noSwitchFlags {
			return usageErr()
		}
		return toolList(ctx, stdout, stderr)
	case "status":
		if len(a.pos) > 1 || a.yes {
			return usageErr()
		}
		if err := toolStatus(ctx, a, stdout, stderr); err != nil {
			return fail(err)
		}
		return 0
	case "on", "off":
		if len(a.pos) != 1 || a.yes {
			return usageErr()
		}
		if err := toolSwitch(ctx, a, args[0] == "on", stdout); err != nil {
			return fail(err)
		}
		return 0
	case "add":
		if len(a.pos) != 1 || !noSwitchFlags {
			return usageErr()
		}
		if err := toolAdd(a.pos[0], a.yes, in, tty, stdout); err != nil {
			return fail(err)
		}
		return 0
	case "remove":
		if len(a.pos) != 1 || a.yes || !noSwitchFlags {
			return usageErr()
		}
		if err := toolRemove(ctx, a.pos[0], stdout); err != nil {
			return fail(err)
		}
		return 0
	}
	fmt.Fprint(stderr, toolUsage)
	return 2
}

// toolCode is the reason code of an aggy tool error.
func toolCode(err error) diag.Code {
	var te *tooldef.Error
	switch {
	case errors.Is(err, errToolDelegated):
		return diag.CodeToolDelegated
	case errors.Is(err, tooldef.ErrUntrusted):
		return diag.CodeToolUntrusted
	case errors.As(err, &te):
		return diag.CodeToolInvalid
	}
	return codeOf(err, diag.CodeToolFailed)
}

// errToolDelegated refuses add, remove, on, and off from a borrow child:
// a delegated CLI must not change which tools aggy trusts or runs.
var errToolDelegated = errors.New("a borrow child cannot add, remove, or switch tools (" + borrow.Delegated + " is set)")

func delegated() error {
	if _, ok := os.LookupEnv(borrow.Delegated); ok {
		return errToolDelegated
	}
	return nil
}

func toolList(ctx context.Context, stdout, stderr io.Writer) int {
	r, refused, err := tooldef.LoadDefault()
	if err != nil {
		toolRefuse(ctx, stderr, "tool list", diag.EvToolRefused, err)
		return 1
	}
	b := switchboard.Board{Tools: r.List()}
	if b.Switches, err = toolSwitches(ctx); err != nil {
		// L83-9: the list still shows; only the costs need the settings.
		fmt.Fprintf(stderr, "aggy tool list: warning: the tool settings cannot be read (%s); costs are shown as if no tool were turned on\n", termsafe.Line(err.Error()))
	}
	repo := toolRepoAt{}
	if cwd, err := os.Getwd(); err == nil {
		repo, _ = toolRepo(cwd, false)
	}
	if repo.root != "" {
		if b.RepoOff, err = repoToolsOff(repo); err != nil {
			fmt.Fprintf(stderr, "aggy tool list: warning: %s; every tool is off in this repo until it is fixed\n", termsafe.Line(err.Error()))
			for _, t := range b.Tools {
				b.RepoOff = append(b.RepoOff, t.Name)
			}
		}
	}
	blocked := skillBlocked(ctx, b, repo.key) // R80-3: not wired, so no cost
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	head := "NAME\tKIND\tDEFAULT"
	for _, v := range adapter.Vendors() {
		head += "\t" + strings.ToUpper(string(v))
	}
	fmt.Fprintln(tw, head+"\tSOURCE\tDESCRIPTION")
	for _, t := range r.List() {
		kind, def := "user", "off"
		if t.BuiltIn {
			kind = "built-in"
		}
		if t.Default {
			def = "on"
		}
		row := []string{t.Name, kind, def}
		for _, v := range adapter.Vendors() {
			skillOn, partsOn := toolOnFor(b, t, v, repo.key, blocked)
			row = append(row, toolCost(t, v, skillOn, partsOn))
		}
		fmt.Fprintln(tw, strings.Join(append(row, termsafe.Field(t.Source), termsafe.Field(t.Description)), "\t"))
	}
	_ = tw.Flush()
	fmt.Fprintln(stdout, toolCostNote)
	logRefusedDefs(ctx, stderr, "tool list", refused)
	if len(refused) > 0 {
		return 1
	}
	return 0
}

// logRefusedDefs prints and records each user definition Load left out.
func logRefusedDefs(ctx context.Context, stderr io.Writer, cmd string, refused []error) {
	for _, e := range refused {
		toolRefuse(ctx, stderr, cmd, diag.EvToolDefRefused, fmt.Errorf("refused %w", e))
	}
}

// errDefsRefused says logRefusedDefs has printed the refused files.
var errDefsRefused = errors.New("user tool definitions refused")

// toolRefuse prints err in full with its [code · run id] tag, and
// records it. A definition's error can quote its argv, so the record
// holds only the file's base name and the key (attrs file and key).
func toolRefuse(ctx context.Context, stderr io.Writer, cmd string, ev diag.Event, err error) {
	code := toolCode(err)
	rec := errRec(diag.Rec{Event: ev, Code: code, Msg: fixedMsg(code)}, err)
	var te *tooldef.Error
	if errors.As(err, &te) {
		file := filepath.Base(te.File)
		rec.Msg = file + ": refused"
		rec.Attrs["file"] = file
		if te.Key != "" {
			key := recordKey(te.Key)
			rec.Msg = file + ": " + key + ": refused"
			rec.Attrs["key"] = key
		}
	}
	dlog.Log(ctx, rec)
	msg := termsafe.Field(err.Error())
	if tag := dlog.Tag(code); tag != "" {
		msg += " " + tag
	}
	fmt.Fprintf(stderr, "aggy %s: %s\n", cmd, msg)
}

// recordKey is a definition error's key for a record. The one map whose
// keys the user writes, [cost.vendor], is kept only for a known vendor:
// any other name is the file's own text (F73-3).
func recordKey(k string) string {
	if v, ok := strings.CutPrefix(k, "cost.vendor."); ok {
		if _, err := adapter.ParseVendor(v); err != nil {
			return "cost.vendor.<invalid>"
		}
	}
	return k
}

func toolAdd(src string, yes bool, in io.Reader, tty bool, stdout io.Writer) error {
	if err := delegated(); err != nil {
		return err
	}
	src, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	b, err := safefs.ReadFileLimit(src, tooldef.MaxSize)
	if err != nil {
		return err
	}
	d, err := tooldef.Parse(src, b, false)
	if err != nil {
		return err // tool.def_invalid, by toolCode
	}
	state, err := home.Dir()
	if err != nil {
		return err
	}
	r, _, err := tooldef.Load(state)
	if err != nil {
		return err
	}
	if old, ok := r.Get(d.Name); ok {
		if old.BuiltIn {
			return &tooldef.Error{File: src, Key: "name", Msg: "a built-in tool has this name, and built-ins cannot be shadowed"}
		}
		return &tooldef.Error{File: src, Key: "name", Msg: "a user tool has this name: " + old.Source + "; remove it first"}
	}
	dir := tooldef.Dir(state)
	dest := filepath.Join(dir, d.Name+".toml")
	errExists := fmt.Errorf("%s already exists; aggy tool remove %s first", dest, d.Name)
	if _, err := safefs.Lstat(dest); err == nil {
		return errExists // a refused file, or a symlink, holds the name
	}
	previewTool(stdout, d, dest)
	if !tty {
		return errors.New("not added: adding a tool needs you at a terminal, because its commands run as you once it is on; --yes only skips the question there")
	}
	if !yes {
		fmt.Fprintf(stdout, "Add %s? [y/N] ", d.Name)
		line, _ := bufio.NewReader(io.LimitReader(in, maxAnswer)).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "Nothing added.")
			return nil
		}
	}
	if err := safefs.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := tooldef.CheckDir(dir); err != nil {
		return err
	}
	if err := writeNew(dest, b); err != nil {
		if errors.Is(err, fs.ErrExist) {
			err = errExists
		}
		return err
	}
	// Settings left from an earlier tool of this name never apply to
	// this one (their digest differs too). The digest of what was shown
	// is recorded, so on turns on only that (S63-16).
	if err := recordToolAdd(context.Background(), d.Name, tooldef.Digest(b)); err != nil {
		// W71-12: never leave a file whose shown digest is not recorded,
		// nor an earlier tool's settings applying to it.
		if rerr := safefs.Remove(dest); rerr != nil {
			return fmt.Errorf("the index was not updated (%w), and %s could not be removed: %v; remove it by hand", err, dest, rerr)
		}
		return fmt.Errorf("not added: the index was not updated: %w", err)
	}
	fmt.Fprintf(stdout, "Added %s (off). aggy tool remove %s deletes it.\n", termsafe.Field(dest), d.Name)
	return nil
}

// writeNew creates dest (mode 0600) with O_EXCL, so it never replaces a
// file or follows a symlink put there after the checks (T59-15); a
// failed write removes the partial file.
func writeNew(dest string, b []byte) error { return writeNewMode(dest, b, 0o600) }

// writeNewMode is writeNew with mode perm.
func writeNewMode(dest string, b []byte, perm fs.FileMode) error {
	f, err := safefs.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = safefs.Remove(dest)
	}
	return err
}

// previewTool prints exactly what the definition would run and wire,
// per vendor, before it is added.
func previewTool(w io.Writer, d tooldef.Def, dest string) {
	fmt.Fprintf(w, "Tool %s %s: %s\n", d.Name, d.Version, termsafe.Field(d.Description))
	if d.Homepage != "" {
		fmt.Fprintf(w, "  homepage: %s\n", termsafe.Field(d.Homepage))
	}
	scope := "global"
	if d.Scope == "repo" {
		scope = "per repo"
		if d.When != "" {
			scope += ", " + whenText(d.When)
		}
	}
	fmt.Fprintf(w, "  scope: %s; off until you turn it on\n", scope)
	if d.Network {
		fmt.Fprintln(w, "  network: reaches the network (the default unless the file says network = false); an edit borrow gets it only with routing.edit_network_tools")
	} else {
		fmt.Fprintln(w, "  network: none, as the file declares (network = false); an edit borrow may get it")
	}
	fmt.Fprintf(w, "  cost estimate: %s\n", d.Cost.Method)
	if len(d.Install.Check)+len(d.Install.Install)+len(d.Install.Prebuild) > 0 || d.Install.Hint != "" {
		fmt.Fprintln(w, "  install (nothing runs now; install and prebuild run only on aggy tool install, after asking):")
	}
	cmd := func(label string, a []string) {
		if len(a) > 0 {
			fmt.Fprintf(w, "    %s: %s\n", label, quoteArgv(a))
		}
	}
	cmd("check", d.Install.Check)
	if d.Install.CheckPattern != "" {
		fmt.Fprintf(w, "    version pattern: %s\n", strconv.Quote(d.Install.CheckPattern))
	}
	if d.Install.Hint != "" {
		fmt.Fprintf(w, "    install hint, from this file (not aggy's advice): %s\n", termsafe.Field(strconv.Quote(d.Install.Hint)))
	}
	cmd("install", d.Install.Install)
	cmd("prebuild", d.Install.Prebuild)
	for _, v := range adapter.Vendors() {
		p := d.PartsFor(v)
		if p.Empty() {
			fmt.Fprintf(w, "  %s: nothing\n", v)
			continue
		}
		fmt.Fprintf(w, "  %s:\n", v)
		if p.MCP != nil {
			fmt.Fprintf(w, "    MCP server: %s\n", quoteArgv(p.MCP.Argv))
			if len(p.MCP.Env) > 0 {
				fmt.Fprintf(w, "      passes these variables from your environment: %s\n", strings.Join(p.MCP.Env, ", "))
			}
		}
		if p.Skill != nil {
			fmt.Fprintf(w, "    skill folder linked: %s\n", termsafe.Field(p.Skill.Path))
		}
		for _, h := range p.Hooks {
			fmt.Fprintf(w, "    %s hook: %s\n", h.Event, quoteArgv(h.Argv))
			if h.Matcher != "" {
				fmt.Fprintf(w, "      on tools matching %s\n", h.Matcher)
			}
			if h.Timeout > 0 {
				fmt.Fprintf(w, "      timeout %ds\n", h.Timeout)
			}
			if h.Decides {
				fmt.Fprintln(w, "      can allow or deny the action")
			}
		}
		for _, s := range p.Init {
			cmd("init step", s.Argv)
			fmt.Fprintf(w, "      writes: %s\n", termsafe.Field(strings.Join(s.Writes, "; ")))
			cmd("  undone by", s.Undo)
		}
	}
	if strings.Contains(fmt.Sprint(d.MCP, d.Hooks, d.Init, d.Install), tooldef.Placeholder) {
		fmt.Fprintf(w, "  %s is the repo or worktree the session runs in, passed as an argument only: never the program, never in a hook\n", tooldef.Placeholder)
	}
	if len(d.Unverified) > 0 {
		fmt.Fprintf(w, "  unverified: %s\n", strings.Join(d.Unverified, ", "))
	}
	fmt.Fprintf(w, "  file: %s\n", termsafe.Field(dest))
}

func quoteArgv(a []string) string {
	q := make([]string, len(a))
	for i, s := range a {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, " ")
}

func toolRemove(ctx context.Context, name string, stdout io.Writer) error {
	if err := delegated(); err != nil {
		return err
	}
	if !tooldef.ValidName(name) {
		return withCode(diag.CodeToolUnknown, fmt.Errorf("%q is not a tool name", name))
	}
	state, err := home.Dir()
	if err != nil {
		return err
	}
	dir := tooldef.Dir(state)
	dest := filepath.Join(dir, name+".toml")
	// A refused definition (invalid, or named like a built-in) is
	// removed too, by its file name; a built-in never is.
	_, statErr := safefs.Lstat(dest)
	if errors.Is(statErr, fs.ErrNotExist) {
		if r, err := tooldef.Builtins(); err == nil {
			if _, ok := r.Get(name); ok {
				return withCode(diag.CodeToolScope, fmt.Errorf("%s is a built-in tool and cannot be removed", name))
			}
		}
		return withCode(diag.CodeToolUnknown, fmt.Errorf("no user tool %s", name))
	}
	if statErr != nil {
		return statErr
	}
	// Never delete through a symlinked or shared tools folder.
	if err := tooldef.CheckDir(dir); err != nil {
		return err
	}
	// Refused while this very file is on somewhere. A refused or changed
	// definition is off already, so it can always be removed.
	r, _, err := tooldef.Load(state)
	if err != nil {
		return err
	}
	if t, ok := r.Get(name); ok && !t.BuiltIn && t.Source == dest {
		sws, err := toolSwitches(ctx)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(sws, func(s store.ToolSwitch) bool { return s.Tool == name && s.On && s.Digest == t.Digest }) {
			return withCode(diag.CodeToolStillOn, fmt.Errorf("%s is on; aggy tool off %s first", name, name))
		}
	}
	if err := safefs.Remove(dest); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Removed %s.\n", termsafe.Field(dest))
	if t, ok := r.Get(name); ok && t.BuiltIn {
		// L83-6: the name is a built-in's, whose settings and skill links
		// are its own; the refused file had none.
		fmt.Fprintf(stdout, "%s is also a built-in tool; its settings and skill links are left as they are.\n", name)
		return nil
	}
	if err := clearToolSwitches(ctx, name); err != nil {
		fmt.Fprintf(stdout, "warning: its settings were not cleared: %s; they apply to no file\n", termsafe.Line(err.Error()))
	}
	// T6727: a removed tool is off everywhere, so a skill it linked
	// while it was on (its file since edited, so off) goes too.
	if err := unlinkToolSkill(ctx, name, stdout); err != nil {
		fmt.Fprintf(stdout, "warning: not every skill link was removed (the changed lines above were): %s; aggy tool off %s tries again\n", termsafe.Line(err.Error()), name)
	}
	return nil
}
