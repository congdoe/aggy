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
	"github.com/congdoe/aggy/internal/parity"
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

// clearToolSwitches deletes every setting of name, when there is an
// index that has them.
func clearToolSwitches(ctx context.Context, name string) error {
	s, err := openIndex(ctx)
	if err != nil || s == nil {
		return withCode(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	return withCode(diag.CodeStore, s.DeleteToolSwitches(ctx, name))
}

// recordToolAdd clears every setting of name and records the digest of
// the definition aggy tool add showed.
func recordToolAdd(ctx context.Context, name, digest string) error {
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return withCode(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	if err := s.DeleteToolSwitches(ctx, name); err != nil {
		return withCode(diag.CodeStore, err)
	}
	return withCode(diag.CodeStore, s.PutToolAdded(ctx, name, digest, time.Now()))
}

// toolRepoAt is the repo a switch is about.
type toolRepoAt struct {
	root string // the work-tree root holding the directory
	main string // the main checkout: root itself, or the one a linked worktree belongs to
	key  string // main's canonical path, the repo key in tool_switches
}

// toolRepo finds the repo holding dir: its work-tree root, and the main
// checkout from git's common dir, so a setting follows the repo into its
// worktrees. A .git link that is not git's own keys the work tree by its
// own root (S63-11, parity.GitMainCheckout). Outside a git work tree it
// is the zero value. strict is for aggy tool on: a .git that cannot be
// read is an error; otherwise (off, status, a launch) the work tree is
// keyed by its own root (S63-17).
func toolRepo(dir string, strict bool) (toolRepoAt, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return toolRepoAt{}, err
	}
	root, ok := repoRoot(abs)
	if !ok {
		return toolRepoAt{}, nil
	}
	main, err := parity.GitMainCheckout(root)
	if err != nil {
		if strict {
			return toolRepoAt{}, withCode(diag.CodeToolScope, err)
		}
		main = root
	}
	key, err := safefs.CanonicalPath(main)
	return toolRepoAt{root: root, main: main, key: key}, err
}

// repoToolsOff is the union of tools_off in the main checkout's config
// and, in a linked worktree, the worktree's own.
func repoToolsOff(r toolRepoAt) ([]string, error) {
	var off []string
	for _, dir := range slices.Compact([]string{r.main, r.root}) {
		rc, err := config.LoadRepo(dir)
		if err != nil {
			return nil, withCode(diag.CodeConfig, fmt.Errorf("read repo config: %w", err))
		}
		off = append(off, rc.ToolsOff...)
	}
	return off, nil
}

// toolBoard loads the definitions and the settings.
func toolBoard(ctx context.Context) (switchboard.Board, *tooldef.Registry, []error, error) {
	r, refused, err := tooldef.LoadDefault()
	if err != nil {
		return switchboard.Board{}, nil, nil, err
	}
	b := switchboard.Board{Tools: r.List()}
	if b.Switches, err = toolSwitches(ctx); err != nil {
		return b, nil, nil, err
	}
	return b, r, refused, nil
}

// toolAddedSchema is the index version with the tool_added table.
const toolAddedSchema = 17

// whenText says what a definition's when condition means: a label for
// the person, which aggy never checks.
func whenText(when string) string {
	if when == "has-ui" {
		return "meant for repos with a UI (has-ui; aggy does not check, you choose the repo)"
	}
	return "meant for repos where " + when + " (aggy does not check; you choose the repo)"
}

// checkAdded is S63-16 for user tool t: its file must still be the one
// aggy tool add showed. aggy tool on lets a file aggy tool add never
// recorded through (needRow false); a session's --tool does not (W76-1).
// s nil is an index with no tool_added table: no record.
func checkAdded(ctx context.Context, s *store.Store, t tooldef.Tool, needRow bool) error {
	if t.BuiltIn {
		return nil
	}
	added, ok := "", false
	if s != nil {
		var err error
		if added, ok, err = s.ToolAdded(ctx, t.Name); err != nil {
			return withCode(diag.CodeStore, err)
		}
	}
	switch {
	case ok && added != t.Digest:
		return withCode(diag.CodeToolChanged, fmt.Errorf("%s changed since aggy tool add showed it; aggy tool remove %s, then add it again to see what it runs", t.Source, t.Name))
	case !ok && needRow:
		return withCode(diag.CodeToolChanged, fmt.Errorf("aggy tool add has no record of %s; aggy tool remove %s, then add it again to see what it runs", t.Source, t.Name))
	}
	return nil
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
// and nothing is wired here; the next launch wires what is on. Off takes
// any tool name, loaded or not, and never reads the repo's config.
func toolSwitch(ctx context.Context, a toolArgs, on bool, in io.Reader, tty bool, stdout io.Writer) (err error) {
	if err := delegated(); err != nil {
		return err
	}
	name := a.pos[0]
	if !tooldef.ValidName(name) {
		return withCode(diag.CodeToolUnknown, fmt.Errorf("%q is not a tool name", name))
	}
	vendors, err := toolVendors(a.vendor)
	if err != nil {
		return err
	}
	if on && slices.Equal(vendors, []adapter.Vendor{adapter.Grok}) && !grokToolsVerified {
		return withCode(diag.CodeToolUnverified, errors.New(grokUnverified)) // W71-4
	}
	var repo toolRepoAt
	if a.hasRepo {
		if repo, err = toolRepo(a.repo, on); err != nil {
			return err
		}
		if repo.root == "" {
			return withCode(diag.CodeToolScope, fmt.Errorf("--repo %s is not in a git work tree", a.repo))
		}
	}
	b, r, _, err := toolBoard(ctx)
	if err != nil {
		return err
	}
	t, loaded := r.Get(name)
	if on {
		if !loaded {
			return withCode(diag.CodeToolUnknown, fmt.Errorf("no tool %q; aggy tool list shows the tools, and any user file it refused", name))
		}
		if !slices.ContainsFunc(vendors, func(v adapter.Vendor) bool { return !t.PartsFor(v).Empty() }) {
			return withCode(diag.CodeToolScope, fmt.Errorf("%s has nothing for %s", name, a.vendor))
		}
		if a.proxy {
			// T6780: a proxy turns on only with --proxy, for a vendor the
			// person names and its definition supports.
			switch {
			case t.Proxy == nil:
				return withCode(diag.CodeToolScope, fmt.Errorf("%s has no local proxy", name))
			case a.vendor == "":
				return withCode(diag.CodeProxyVendor, fmt.Errorf("--proxy turns a proxy on only for a vendor you name: aggy tool on %s --proxy --vendor <vendor> (its proxy supports %s)", name, vendorList(t.Proxy.Vendors)))
			case t.PartsFor(vendors[0]).Proxy == nil:
				return withCode(diag.CodeToolScope, fmt.Errorf("%s's proxy supports only %s", name, vendorList(t.Proxy.Vendors)))
			}
		}
		if a.hasRepo && t.SkillOnly() {
			// L78-8: its skill is linked for every repo, never one.
			return withCode(diag.CodeToolScope, fmt.Errorf("%s is only a skill, which is linked for every repo: turn it on without --repo", name))
		}
		if t.Scope == "repo" && !a.hasRepo {
			return withCode(diag.CodeToolScope, fmt.Errorf("%s is a per-repo tool: turn it on with --repo DIR", name))
		}
		if a.hasRepo {
			// Strict only here: a repo config that cannot be read never
			// lets a tool on.
			if b.RepoOff, err = repoToolsOff(repo); err != nil {
				return err
			}
			if slices.Contains(b.RepoOff, name) {
				return withCode(diag.CodeToolRepoOff, fmt.Errorf("the repo's .aggy/config.toml lists %s in tools_off, and a repo's config always wins", name))
			}
		}
	}
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return withCode(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	if on && !t.BuiltIn {
		// #86: before anything prints the file's hint, it must be the
		// file aggy tool add showed.
		if err := checkAdded(ctx, s, t, false); err != nil {
			return err
		}
	}
	// T6780: the warning and the typed confirmation, once the file is the
	// one aggy tool add showed, and before a prebuild runs anything.
	proxy := on && a.proxy
	if proxy {
		if err := confirmProxy(t, vendors[0], in, tty, stdout); err != nil {
			return err
		}
	}
	if on && a.hasRepo {
		// T6711: a per-repo index is built here, the first time, or
		// nothing is recorded.
		if err := prebuildIndex(ctx, t, repo, in, tty, stdout); errors.Is(err, errNotIndexed) {
			return nil
		} else if err != nil {
			return err
		}
	}
	if on && hasSkill(t) {
		// Nothing is recorded or linked while the skill is not in place.
		l, err := toolSkillLayout(t)
		if err != nil {
			return withCode(diag.CodeToolSkillMissing, err)
		}
		if err := checkSkillStore(l.store); err != nil {
			return withCode(diag.CodeToolSkillMissing, fmt.Errorf("%w. %s", err, skillHowTo(t, l.store)))
		}
	}
	if on && hasSkill(t) {
		l, err := toolSkillLayout(t)
		if err != nil {
			return withCode(diag.CodeToolSkillMissing, err)
		}
		if err := checkSkillLinked(ctx, s, t, l.store); err != nil {
			return err
		}
	}
	sw := store.ToolSwitch{Tool: name, Vendor: a.vendor, Repo: repo.key, On: on, At: time.Now()}
	if proxy {
		sw.ProxyDigest = tooldef.ProxyDigest(*t.Proxy, vendors[0])
	}
	switch {
	case on && t.BuiltIn:
		sw.Digest = switchboard.BuiltInDigest
	case on:
		sw.Digest = t.Digest // checkAdded passed above
	}
	if err := s.PutToolSwitch(ctx, sw); err != nil {
		return withCode(diag.CodeStore, err)
	}
	dlog.Log(ctx, diag.Rec{Event: diag.EvToolSwitched, Vendor: a.vendor,
		Attrs: map[string]any{"tool": name, "repo": repo.key != "", "on": on}})
	// Running sessions are told last, after any skill link changed, even
	// when that failed: the setting is recorded (T6729). What it says
	// comes only after a success, never before an error's text (L6).
	defer func() {
		n := notifySessions(ctx, a.vendor, repo.key)
		if err != nil || !loaded {
			return
		}
		fmt.Fprintln(stdout, "It takes effect at the next launch.")
		if n > 0 {
			fmt.Fprintf(stdout, "Told %d running aggy session(s): one whose tools changed offers to resume its CLI on the same session at the turn end (Claude and Codex; Grok's tools are not wired).\n", n)
		}
	}()

	if !loaded {
		fmt.Fprintf(stdout, "%s is not loaded; its settings say off.\n", name)
		// T6727: a skill linked while it was loaded must not outlive it.
		if err := unlinkToolSkill(ctx, name, stdout); err != nil {
			return fmt.Errorf("the setting is recorded, but not every skill link was removed (the changed lines above were): %w", err)
		}
		return nil
	}
	if b.Switches, err = s.ToolSwitches(ctx); err != nil {
		return withCode(diag.CodeStore, err)
	}
	where := "every repo"
	if repo.root != "" {
		where = termsafe.Field(repo.main)
	}
	for _, v := range adapter.Vendors() {
		st := b.State(t, v, repo.key)
		fmt.Fprintf(stdout, "%s for %s in %s: %s (%s%s)\n", name, v, where, onOff(st.On), st.Why, proxyNote(t, v, st, proxiesOn(b, v, repo.key)))
	}
	switch {
	case proxy:
		fmt.Fprintf(stdout, "Its proxy starts with each on-screen %s session from the next launch, never for a borrow child.\n", a.vendor)
	case on && t.Proxy != nil:
		fmt.Fprintf(stdout, "Its proxy stays off; add --proxy --vendor <vendor> to turn it on (it supports %s).\n", vendorList(t.Proxy.Vendors))
	case !on && t.Proxy != nil && slices.ContainsFunc(vendors, func(v adapter.Vendor) bool { return t.PartsFor(v).Proxy != nil }):
		fmt.Fprintf(stdout, "A running session keeps its proxy and %s until its CLI restarts.\n", strings.Join(proxyVars(t), " or "))
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
		if t.When != "" {
			fmt.Fprintf(stdout, "%s is %s.\n", name, whenText(t.When))
		}
		if s := telemetrySets(t.Def); s != "" {
			fmt.Fprintln(stdout, s)
		}
		if prog := missingProgram(t); prog != "" {
			fmt.Fprintf(stdout, "%s is not an executable on PATH, so %s cannot start yet: %s. aggy never installs it.\n", termsafe.Field(prog), name, termsafe.Field(installHowTo(t)))
		}
	}
	// MCP servers and hooks are wired at launch (addTools); a skill is
	// linked now, for every repo.
	if hasSkill(t) {
		if a.hasRepo {
			fmt.Fprintln(stdout, "A skill is linked for every repo: --repo does not change its links, and a repo's tools_off cannot hide it.")
		}
		if err := syncToolSkill(ctx, t, b, stdout); err != nil {
			return fmt.Errorf("the setting is recorded, but not every skill link change was made (the changed lines above were): %w", err)
		}
	} else if err := unlinkToolSkill(ctx, name, stdout); err != nil {
		// H83-2: an edit that dropped the tool's [[skill]] leaves no link.
		return fmt.Errorf("the setting is recorded, but not every old skill link was removed (the changed lines above were): %w", err)
	}
	return nil
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// toolStatus is aggy tool status: whether each tool is on per vendor in
// this repo (or --repo DIR), and why. It writes nothing. A repo config
// that cannot be read is a warning, and every tool is shown off there,
// as a launch would treat it.
func toolStatus(ctx context.Context, a toolArgs, stdout, stderr io.Writer) error {
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
	repo, err := toolRepo(dir, false)
	if err != nil {
		return err
	}
	if a.hasRepo && repo.root == "" {
		return withCode(diag.CodeToolScope, fmt.Errorf("--repo %s is not in a git work tree", a.repo))
	}
	b, r, refused, err := toolBoard(ctx)
	if err != nil {
		return err
	}
	if repo.root != "" {
		if b.RepoOff, err = repoToolsOff(repo); err != nil {
			fmt.Fprintf(stderr, "aggy tool status: warning: %s; every tool is off in this repo until it is fixed\n", termsafe.Line(err.Error()))
			for _, t := range b.Tools {
				b.RepoOff = append(b.RepoOff, t.Name)
			}
		}
	}
	tools := r.List()
	if len(a.pos) == 1 {
		t, ok := r.Get(a.pos[0])
		if !ok {
			return withCode(diag.CodeToolUnknown, fmt.Errorf("no tool %q", a.pos[0]))
		}
		tools = []tooldef.Tool{t}
	}
	switch {
	case repo.root == "":
		fmt.Fprintln(stdout, "Outside a repo: per-repo tools are off.")
	case repo.root != repo.main:
		fmt.Fprintf(stdout, "Repo %s (worktree %s)\n", termsafe.Field(repo.main), termsafe.Field(repo.root))
	default:
		fmt.Fprintf(stdout, "Repo %s\n", termsafe.Field(repo.main))
	}
	cwd, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	// T6726: what doctor reports per tool and vendor decides wired.
	notWired := wireProblems(boardFindings(ctx, b, nil, cwd, repo))
	idx, ver, idxErr := openIndexReadOnly(ctx)
	if idxErr != nil {
		fmt.Fprintf(stderr, "aggy tool status: warning: the index cannot be read (%s); skill pins show as unknown\n", termsafe.Line(idxErr.Error()))
	}
	if idx != nil {
		defer func() { _ = idx.Close() }()
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	head := []string{"TOOL"}
	for _, v := range vendors {
		head = append(head, strings.ToUpper(string(v)))
	}
	fmt.Fprintln(tw, strings.Join(append(head, "INSTALLED", "READY", "WIRED", "WHY"), "\t"))
	for _, t := range tools {
		row := []string{t.Name}
		var whys, wired []string
		for _, v := range vendors {
			st := b.State(t, v, repo.key)
			row = append(row, onOff(st.On))
			whys = append(whys, string(v)+": "+st.Why+proxyNote(t, v, st, proxiesOn(b, v, repo.key)))
			if !st.On {
				continue
			}
			code, bad := notWired[t.Name][v]
			if !bad {
				code, bad = notWired[t.Name][""]
			}
			if !bad && st.Parts.MCP != nil {
				code, bad = notWired[""][v] // the CLI's own MCP config cannot be read
			}
			if bad {
				wired = append(wired, string(v)+" no ("+code+")")
			} else {
				wired = append(wired, string(v))
			}
		}
		if len(wired) == 0 {
			wired = []string{"-"}
		}
		row = append(row, toolInstalled(t), toolReady(ctx, t, idx, ver, repo), strings.Join(wired, ", "))
		fmt.Fprintln(tw, strings.Join(append(row, sameWhy(whys)), "\t"))
	}
	_ = tw.Flush()
	if len(refused) > 0 {
		logRefusedDefs(ctx, stderr, "tool status", refused)
		return errDefsRefused
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
