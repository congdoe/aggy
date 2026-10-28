package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
)

// toolSwitchSchema is the index version that has tool_switches.
const toolSwitchSchema = 16

// toolSwitches reads every aggy tool on/off setting, read-only: none
// when there is no index yet or it predates the switchboard.
func toolSwitches(ctx context.Context) ([]store.ToolSwitch, error) {
	s, v, err := openIndexReadOnly(ctx)
	if err != nil || s == nil {
		return nil, withCode(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	if v < toolSwitchSchema {
		return nil, nil
	}
	sws, err := s.ToolSwitches(ctx)
	return sws, withCode(diag.CodeStore, err)
}

// toolRepo is the repo a switch is about: the work-tree root holding
// dir, and its key in tool_switches (the canonical path). Outside a git
// work tree both are "".
func toolRepo(dir string) (root, key string, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	root, ok := repoRoot(abs)
	if !ok {
		return "", "", nil
	}
	key, err = safefs.CanonicalPath(root)
	return root, key, err
}

// toolBoard loads the definitions, the settings, and the tools_off of
// the repo at root ("" for none).
func toolBoard(ctx context.Context, root string) (switchboard.Board, *tooldef.Registry, []error, error) {
	r, refused, err := tooldef.LoadDefault()
	if err != nil {
		return switchboard.Board{}, nil, nil, err
	}
	b := switchboard.Board{Tools: r.List()}
	if b.Switches, err = toolSwitches(ctx); err != nil {
		return b, nil, nil, err
	}
	if root != "" {
		rc, err := config.LoadRepo(root)
		if err != nil {
			// A repo config that cannot be read never lets a tool on.
			return b, nil, nil, withCode(diag.CodeConfig, fmt.Errorf("read repo config: %w", err))
		}
		b.RepoOff = rc.ToolsOff
	}
	return b, r, refused, nil
}

// toolVendors is the vendor a --vendor flag names, or every vendor.
func toolVendors(name string) ([]adapter.Vendor, error) {
	if name == "" {
		return adapter.Vendors(), nil
	}
	v, err := adapter.ParseVendor(name)
	if err != nil {
		return nil, withCode(diag.CodeUsage, err)
	}
	return []adapter.Vendor{v}, nil
}

// toolSwitch is aggy tool on|off: it records one setting. Nothing runs
// and nothing is wired here; the next launch wires what is on.
func toolSwitch(ctx context.Context, a toolArgs, on bool, stdout io.Writer) error {
	if err := delegated(); err != nil {
		return err
	}
	name := a.pos[0]
	vendors, err := toolVendors(a.vendor)
	if err != nil {
		return err
	}
	root, key := "", ""
	if a.hasRepo {
		if root, key, err = toolRepo(a.repo); err != nil {
			return err
		}
		if root == "" {
			return withCode(diag.CodeToolScope, fmt.Errorf("--repo %s is not in a git work tree", a.repo))
		}
	}
	b, r, _, err := toolBoard(ctx, root)
	if err != nil {
		return err
	}
	t, ok := r.Get(name)
	if !ok {
		return withCode(diag.CodeToolUnknown, fmt.Errorf("no tool %q; aggy tool list shows the tools, and any user file it refused", name))
	}
	if on {
		if !slices.ContainsFunc(vendors, func(v adapter.Vendor) bool { return !t.PartsFor(v).Empty() }) {
			return withCode(diag.CodeToolScope, fmt.Errorf("%s has nothing for %s", name, a.vendor))
		}
		if t.Scope == "repo" && !a.hasRepo {
			return withCode(diag.CodeToolScope, fmt.Errorf("%s is a per-repo tool: turn it on with --repo DIR", name))
		}
		if slices.Contains(b.RepoOff, name) {
			return withCode(diag.CodeToolRepoOff, fmt.Errorf("%s/.aggy/config.toml lists %s in tools_off, and a repo's config always wins", root, name))
		}
	}
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return withCode(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	sw := store.ToolSwitch{Tool: name, Vendor: a.vendor, Repo: key, On: on, At: time.Now()}
	if err := s.PutToolSwitch(ctx, sw); err != nil {
		return withCode(diag.CodeStore, err)
	}
	dlog.Log(ctx, diag.Rec{Event: diag.EvToolSwitched, Vendor: a.vendor,
		Attrs: map[string]any{"tool": name, "repo": key != "", "on": on}})

	b.Switches, err = s.ToolSwitches(ctx)
	if err != nil {
		return withCode(diag.CodeStore, err)
	}
	where := "every repo"
	if root != "" {
		where = termsafe.Field(root)
	}
	for _, v := range adapter.Vendors() {
		st := b.State(t, v, key)
		state := "off"
		if st.On {
			state = "on"
		}
		fmt.Fprintf(stdout, "%s for %s in %s: %s (%s)\n", name, v, where, state, st.Why)
	}
	if on {
		if !a.hasRepo {
			fmt.Fprintln(stdout, "A repo whose .aggy/config.toml lists it in tools_off keeps it off.")
		}
		if !t.BuiltIn {
			fmt.Fprintln(stdout, "Its commands run as you once it is wired, as aggy tool add showed.")
		}
		if len(t.Unverified) > 0 {
			fmt.Fprintf(stdout, "Unverified vendor facts: %s.\n", strings.Join(t.Unverified, ", "))
		}
	}
	// TODO(T6708): wire at launch; until then a switch changes no CLI.
	fmt.Fprintln(stdout, "It takes effect at the next launch.")
	return nil
}

// toolStatus is aggy tool status: whether each tool is on per vendor in
// this repo (or --repo DIR), and why. It writes nothing.
func toolStatus(ctx context.Context, a toolArgs, stdout io.Writer) error {
	vendors, err := toolVendors(a.vendor)
	if err != nil {
		return err
	}
	dir := a.repo
	if !a.hasRepo {
		if dir, err = os.Getwd(); err != nil {
			return err
		}
	}
	root, key, err := toolRepo(dir)
	if err != nil {
		return err
	}
	if a.hasRepo && root == "" {
		return withCode(diag.CodeToolScope, fmt.Errorf("--repo %s is not in a git work tree", a.repo))
	}
	b, r, refused, err := toolBoard(ctx, root)
	if err != nil {
		return err
	}
	tools := r.List()
	if len(a.pos) == 1 {
		t, ok := r.Get(a.pos[0])
		if !ok {
			return withCode(diag.CodeToolUnknown, fmt.Errorf("no tool %q", a.pos[0]))
		}
		tools = []tooldef.Tool{t}
	}
	if root == "" {
		fmt.Fprintln(stdout, "Outside a repo: per-repo tools are off.")
	} else {
		fmt.Fprintf(stdout, "Repo %s\n", termsafe.Field(root))
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	head := []string{"TOOL"}
	for _, v := range vendors {
		head = append(head, strings.ToUpper(string(v)))
	}
	fmt.Fprintln(tw, strings.Join(append(head, "WHY"), "\t"))
	for _, t := range tools {
		row := []string{t.Name}
		var whys []string
		for _, v := range vendors {
			st := b.State(t, v, key)
			row = append(row, map[bool]string{true: "on", false: "off"}[st.On])
			whys = append(whys, string(v)+": "+st.Why)
		}
		fmt.Fprintln(tw, strings.Join(append(row, sameWhy(whys)), "\t"))
	}
	_ = tw.Flush()
	if len(refused) > 0 {
		var es []string
		for _, e := range refused {
			es = append(es, e.Error())
		}
		return withCode(toolCode(refused[0]), errors.New("refused: "+strings.Join(es, "; ")))
	}
	return nil
}

// sameWhy is one reason when every vendor has the same one, else each
// vendor's.
func sameWhy(whys []string) string {
	first := whys[0][strings.Index(whys[0], ": ")+2:]
	for _, w := range whys[1:] {
		if w[strings.Index(w, ": ")+2:] != first {
			return strings.Join(whys, "; ")
		}
	}
	return first
}
