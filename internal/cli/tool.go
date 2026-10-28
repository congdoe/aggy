package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
)

const toolUsage = `Usage:
  aggy tool list
  aggy tool add <file> [--yes]
  aggy tool remove <name>

Managed tools are described by definitions: aggy's built-ins, and your
own in ~/.aggy/tools/<name>.toml. Definitions are never read from a repo.

  list     show each tool: name, built-in or user, default, source file
  add      validate a definition, show what it would run and wire per
           vendor, and copy it into ~/.aggy/tools after you answer y
           (--yes without a terminal). A user tool starts off.
  remove   delete a user definition; refused while the tool is wired
`

func runTool(args []string, stdout, stderr io.Writer) int {
	return toolWith(args, os.Stdin, isTerminal(os.Stdin), stdout, stderr)
}

func toolWith(args []string, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, toolUsage)
		return 2
	}
	usageErr := func() int {
		fmt.Fprintf(stderr, "aggy tool %s: bad arguments\n\n%s", termsafe.Field(args[0]), toolUsage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "aggy tool %s: %s\n", termsafe.Field(args[0]), termsafe.Line(err.Error()))
		return 1
	}
	var pos []string
	yes := false
	for _, a := range args[1:] {
		switch {
		case a == "--yes":
			yes = true
		case strings.HasPrefix(a, "-"):
			return usageErr()
		default:
			pos = append(pos, a)
		}
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, toolUsage)
		return 0
	case "list":
		if len(pos) != 0 || yes {
			return usageErr()
		}
		return toolList(stdout, stderr)
	case "add":
		if len(pos) != 1 {
			return usageErr()
		}
		if err := toolAdd(pos[0], yes, in, tty, stdout); err != nil {
			return fail(err)
		}
		return 0
	case "remove":
		if len(pos) != 1 || yes {
			return usageErr()
		}
		if err := toolRemove(pos[0], stdout); err != nil {
			return fail(err)
		}
		return 0
	}
	fmt.Fprint(stderr, toolUsage)
	return 2
}

// toolWired reports whether a tool is wired into any vendor.
// TODO(T6708): read the switchboard's wiring; nothing is wired before it.
var toolWired = func(string) (bool, error) { return false, nil }

// errToolDelegated refuses add and remove from a borrow child: a
// delegated CLI must not change which tools aggy trusts.
var errToolDelegated = errors.New("a borrow child cannot add or remove tool definitions (" + borrow.Delegated + " is set)")

func toolList(stdout, stderr io.Writer) int {
	r, refused, err := tooldef.LoadDefault()
	if err != nil {
		fmt.Fprintf(stderr, "aggy tool list: %s\n", termsafe.Line(err.Error()))
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tDEFAULT\tSOURCE\tDESCRIPTION")
	for _, t := range r.List() {
		kind, def := "user", "off"
		if t.BuiltIn {
			kind = "built-in"
		}
		if t.Default {
			def = "on"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, kind, def, termsafe.Field(t.Source), termsafe.Field(t.Description))
	}
	_ = tw.Flush()
	for _, e := range refused {
		fmt.Fprintf(stderr, "aggy tool list: refused %s\n", termsafe.Line(e.Error()))
	}
	if len(refused) > 0 {
		return 1
	}
	return 0
}

func toolAdd(src string, yes bool, in io.Reader, tty bool, stdout io.Writer) error {
	if os.Getenv(borrow.Delegated) != "" {
		return errToolDelegated
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
		// TODO(T6753): log tool.def_invalid.
		return err
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
		// TODO(T6753): log tool.def_invalid (name collision).
		if old.BuiltIn {
			return &tooldef.Error{File: src, Key: "name", Msg: "a built-in tool has this name, and built-ins cannot be shadowed"}
		}
		return &tooldef.Error{File: src, Key: "name", Msg: "a user tool has this name: " + old.Source + "; remove it first"}
	}
	dir := tooldef.Dir(state)
	dest := filepath.Join(dir, d.Name+".toml")
	previewTool(stdout, d, dest)
	if !yes {
		if !tty {
			fmt.Fprintln(stdout, "Not added: run again with --yes, or in a terminal, to add it.")
			return nil
		}
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
	if _, err := safefs.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			err = fmt.Errorf("%s already exists; aggy tool remove %s first", dest, d.Name)
		}
		return err
	}
	if err := safefs.WriteFile(dest, b, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Added %s (off). aggy tool remove %s deletes it.\n", termsafe.Field(dest), d.Name)
	return nil
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
			scope += ", only where " + d.When
		}
	}
	fmt.Fprintf(w, "  scope: %s; off until you turn it on\n", scope)
	fmt.Fprintf(w, "  cost estimate: %s\n", d.Cost.Method)
	if len(d.Install.Check)+len(d.Install.Install)+len(d.Install.Prebuild) > 0 {
		fmt.Fprintln(w, "  install (nothing runs now; install and prebuild run only on aggy tool install, after asking):")
	}
	cmd := func(label string, a []string) {
		if len(a) > 0 {
			fmt.Fprintf(w, "    %s: %s\n", label, quoteArgv(a))
		}
	}
	cmd("check", d.Install.Check)
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
		}
		for _, s := range p.Init {
			cmd("init step", s.Argv)
		}
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

func toolRemove(name string, stdout io.Writer) error {
	if os.Getenv(borrow.Delegated) != "" {
		return errToolDelegated
	}
	if !tooldef.ValidName(name) {
		return fmt.Errorf("%q is not a tool name", name)
	}
	state, err := home.Dir()
	if err != nil {
		return err
	}
	r, _, err := tooldef.Load(state)
	if err != nil {
		return err
	}
	if t, ok := r.Get(name); ok && t.BuiltIn {
		return fmt.Errorf("%s is a built-in tool and cannot be removed", name)
	}
	wired, err := toolWired(name)
	if err != nil {
		return err
	}
	if wired {
		// TODO(T6753): log tool.remove_wired.
		return fmt.Errorf("%s is wired into a CLI; turn it off first", name)
	}
	// A refused (invalid) definition is removed too, by its file name.
	dest := filepath.Join(tooldef.Dir(state), name+".toml")
	if _, err := safefs.Lstat(dest); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no user tool %s", name)
		}
		return err
	}
	if err := safefs.Remove(dest); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Removed %s.\n", termsafe.Field(dest))
	return nil
}
