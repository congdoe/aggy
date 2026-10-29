package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/skills"
)

const skillUsage = `Usage: aggy skill link [--vendor V] [--dry-run]
       aggy skill unlink [--vendor V] [--dry-run]
       aggy skill status
       aggy skill add <dir> [--name N] [--yes] [--replace]
       aggy skill remove <name>

Installs aggy's skill once, in ~/.aggy/skills/aggy, and links it into
each vendor CLI's user skill folder, where it is /aggy in Claude Code and
Grok Build and $aggy in Codex:

  claude   ~/.claude/skills/aggy
  codex    ~/.agents/skills/aggy
  grok     ~/.grok/skills/aggy ($GROK_HOME/skills/aggy)

Grok also scans ~/.claude/skills and ~/.agents/skills. All links point at
the one installed copy, and Grok drops a skill it reaches twice by the
same real path, so each CLI loads it once. unlink removes the vendor's
link, and for Grok, while another vendor's link is in a folder Grok
scans, adds the installed copy to [skills] ignore in Grok's config.toml,
so an unlinked CLI loads none of it.

Every change goes through the manifest; aggy unwire undoes it.

  --vendor V   claude, codex, or grok (default: all three)
  --dry-run    show the changes and make none

add stores your own skill folder (holding SKILL.md, regular files only,
no symlinks) once, in ~/.aggy/skills/<name> (the folder's name, or
--name), with a user tool definition that is only that skill, off. It
shows the skill's frontmatter, files, size, and sha256 first, and stores
it after you answer y at a terminal (--yes skips the question; it never
works without a terminal). aggy tool on <name> [--vendor V] then links
it. --replace replaces a folder of that name, or stores one for a
built-in tool's skill; never while the tool is on. remove deletes the
folder and the definition; refused while the tool is on.
`

// skillName is the skill's folder name and its invocation name.
const skillName = "aggy"

var skillVendors = []string{"claude", "codex", "grok"}

// skillLayout is where the skill lives and where each vendor finds it.
type skillLayout struct {
	store      string            // the installed skill directory
	links      map[string]string // vendor -> its link
	grokHome   string
	grokConfig string
	// content is the SKILL.md aggy writes into store on link: aggy's
	// own skill. A managed tool's skill (nil) is the person's to put
	// there; aggy never fetches or writes one.
	content []byte
}

// scans lists the links a vendor's CLI would load the skill through: its
// own, and for Grok also the Claude and Codex folders it scans by
// default (crates/codegen/xai-grok-agent/src/prompt/skills.rs at
// grok-build 2bdd1d6a; docs user-guide/08-skills.md).
func (l skillLayout) scans(v string) []string {
	if v == "grok" {
		return []string{l.links["grok"], l.links["codex"], l.links["claude"]}
	}
	return []string{l.links[v]}
}

// layout finds the paths. The folders are the user skill folders each
// vendor documents: Claude ~/.claude/skills (code.claude.com/docs skills;
// whether CLAUDE_CONFIG_DIR moves it is unverified, so it is not
// followed), Codex $HOME/.agents/skills (Codex skills docs; ~/.codex/skills
// is not a documented location), and Grok's grok home skills folder.
func layout() (skillLayout, error) {
	l, err := layoutFor(skillName)
	l.content = skills.Aggy
	return l, err
}

// layoutFor is layout for the skill named name, stored in
// <state>/skills/<name> (a managed tool's skill, T6708), whose SKILL.md
// aggy does not write.
func layoutFor(name string) (skillLayout, error) {
	h := os.Getenv("HOME")
	if !filepath.IsAbs(h) {
		return skillLayout{}, errors.New("HOME is not an absolute path")
	}
	state, err := home.Dir()
	if err != nil {
		return skillLayout{}, err
	}
	grokHome := filepath.Join(h, ".grok")
	if g := os.Getenv("GROK_HOME"); g != "" {
		if !filepath.IsAbs(g) {
			return skillLayout{}, fmt.Errorf("GROK_HOME=%q is not an absolute path", g)
		}
		grokHome = g
	}
	return skillLayout{
		store: filepath.Join(state, "skills", name),
		links: map[string]string{
			"claude": filepath.Join(h, ".claude", "skills", name),
			"codex":  filepath.Join(h, ".agents", "skills", name),
			"grok":   filepath.Join(grokHome, "skills", name),
		},
		grokHome:   grokHome,
		grokConfig: filepath.Join(grokHome, "config.toml"),
	}, nil
}

// linked reports whether path is aggy's link to the installed skill.
func (l skillLayout) linked(path string) (bool, error) {
	st, err := entryState(path)
	return err == nil && st.Hash == patch.LinkHash(l.store), err
}

// isDir is the state hash entryState gives a real directory, which no
// patch can name.
const isDir = "dir"

// entryState is patch.ReadState, except that a real directory (someone's
// own skill folder) is reported as isDir rather than an error.
func entryState(path string) (patch.State, error) {
	if fi, err := safefs.Lstat(path); err == nil && fi.IsDir() {
		return patch.State{Hash: isDir, Info: fi}, nil
	}
	return patch.ReadState(path)
}

func runSkill(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, skillUsage)
		return 0
	}
	op := args[0]
	if op == "add" || op == "remove" {
		return skillManage(args, os.Stdin, isTerminal(os.Stdin), stdout, stderr)
	}
	fl := flag.NewFlagSet("skill", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	vendor := fl.String("vendor", "", "")
	dryRun := fl.Bool("dry-run", false, "")
	if err := fl.Parse(args[1:]); err != nil || fl.NArg() > 0 {
		fmt.Fprintf(stderr, "aggy skill: bad arguments\n\n%s", skillUsage)
		return 2
	}
	vendors := skillVendors
	if *vendor != "" {
		if !slices.Contains(skillVendors, *vendor) {
			fmt.Fprintf(stderr, "aggy skill: unknown vendor %q (claude, codex, or grok)\n", *vendor)
			return 2
		}
		vendors = []string{*vendor}
	}
	l, err := layout()
	if err != nil {
		fmt.Fprintf(stderr, "aggy skill: %v\n", err)
		return 1
	}
	switch op {
	case "status":
		if *vendor != "" || *dryRun {
			fmt.Fprintf(stderr, "aggy skill status takes no flags\n")
			return 2
		}
		return skillStatus(l, stdout, stderr)
	case "link", "unlink":
	default:
		fmt.Fprintf(stderr, "aggy skill: unknown subcommand %q\n\n%s", op, skillUsage)
		return 2
	}
	ps, err := l.plan(op == "link", vendors)
	if err != nil {
		fmt.Fprintf(stderr, "aggy skill %s: %s\n", op, termsafe.Line(err.Error()))
		return 1
	}
	if *dryRun {
		if _, err := manifest.Preview(stdout, ps); err != nil {
			return 1
		}
		return 0
	}
	if len(ps) == 0 {
		fmt.Fprintln(stdout, "Nothing to change.")
		return 0
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aggy skill: %v\n", err)
		return 1
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		fmt.Fprintf(stderr, "aggy skill: %v\n", err)
		return 1
	}
	runID := manifest.NewRunID()
	cs, err := m.Run(ctx, manifest.Write, runID, ps, io.Discard)
	for _, c := range cs {
		if c.Status == store.ChangeApplied {
			fmt.Fprintf(stdout, "changed %s\n", termsafe.Field(c.File))
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "aggy skill %s: %s\n", op, termsafe.Line(err.Error()))
		return 1
	}
	fmt.Fprintf(stdout, "Done (run %s). aggy unwire --run %s undoes it.\n", runID, runID)
	return 0
}

// plan returns the patches that link (or unlink) the skill for vendors,
// and set Grok's ignore entry to match the result: present exactly when
// Grok is not linked but another vendor's link is in a folder it scans.
// It writes nothing.
func (l skillLayout) plan(link bool, vendors []string) ([]patch.Patch, error) {
	want := map[string]bool{}
	for _, v := range vendors {
		want[v] = link
	}
	return l.planSet(want)
}

// planSet is plan with each vendor of want linked (true) or unlinked
// (false) in one plan, so Grok's ignore entry matches the end state.
func (l skillLayout) planSet(want map[string]bool) ([]patch.Patch, error) {
	var ps []patch.Patch
	after := map[string]bool{}
	for _, v := range skillVendors {
		ok, err := l.linked(l.links[v])
		if err != nil {
			return nil, err
		}
		after[v] = ok
	}
	anyLink := slices.Contains(slices.Collect(maps.Values(want)), true)
	if anyLink && l.content != nil {
		file := filepath.Join(l.store, "SKILL.md")
		h, err := patch.HashFile(file)
		if err != nil {
			return nil, err
		}
		content := l.content
		ps = append(ps, patch.Patch{File: file, ExpectedHash: h, Mode: 0o644,
			Transform: func([]byte) ([]byte, error) { return content, nil }})
	}
	for _, v := range skillVendors {
		link, ok := want[v]
		if !ok {
			continue
		}
		path := l.links[v]
		st, err := entryState(path)
		if err != nil {
			return nil, err
		}
		ours := st.Hash == patch.LinkHash(l.store)
		switch {
		case link && !ours && st.Hash != patch.NoFile:
			return nil, fmt.Errorf("%s exists and is not aggy's link; move it away and run this again", path)
		case link && !ours:
			ps = append(ps, patch.Patch{File: path, ExpectedHash: patch.NoFile, Link: l.store})
		case !link && ours:
			ps = append(ps, patch.Patch{File: path, ExpectedHash: st.Hash, Remove: true})
		}
		after[v] = link && (ours || st.Hash == patch.NoFile)
	}
	p, err := l.grokIgnorePatch(!after["grok"] && (after["claude"] || after["codex"]))
	if err != nil {
		return nil, err
	}
	if p != nil {
		if anyLink {
			i := 0
			if l.content != nil {
				i = 1 // after SKILL.md
			}
			ps = slices.Insert(ps, i, *p) // before any link
		} else {
			ps = append(ps, *p)
		}
	}
	return ps, nil
}

// grokIgnorePatch returns the patch that adds (want) or removes the
// installed skill in Grok's [skills] ignore, or nil when it is already
// so. Grok matches ignore entries by real path, so the one entry hides
// the skill through every link. Without a Grok home there is no Grok to
// configure and nothing is written; aggy skill status reports it if one
// appears later.
func (l skillLayout) grokIgnorePatch(want bool) (*patch.Patch, error) {
	if _, err := safefs.Lstat(l.grokHome); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	b, h, err := readMaybe(l.grokConfig)
	if err != nil {
		return nil, err
	}
	have, err := grokIgnores(b, l.store)
	if err != nil {
		return nil, err
	}
	switch {
	case want == have:
		return nil, nil
	case want:
		return &patch.Patch{File: l.grokConfig, ExpectedHash: h, Transform: addGrokIgnore(l.store), Mode: 0o600}, nil
	}
	// Only the block is taken out, even when it is all the file holds:
	// the file stays, empty, rather than being deleted and brought back
	// by aggy unwire (B14-4).
	return &patch.Patch{File: l.grokConfig, ExpectedHash: h, Transform: removeGrokIgnore(l.store)}, nil
}

func readMaybe(path string) ([]byte, string, error) {
	b, err := safefs.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, patch.NoFile, nil
	}
	if err != nil {
		return nil, "", err
	}
	return b, patch.Hash(b), nil
}

// grokIgnores reports whether Grok's config src lists entry in [skills]
// ignore.
func grokIgnores(src []byte, entry string) (bool, error) {
	_, ignore, _, err := grokSkills(src)
	return slices.Contains(ignore, entry), err
}

// grokSkills decodes src and returns the document, [skills] ignore, and
// whether there is a skills table at all.
func grokSkills(src []byte) (doc map[string]any, ignore []string, hasSkills bool, err error) {
	doc = map[string]any{}
	if _, err := toml.Decode(string(src), &doc); err != nil {
		return nil, nil, false, fmt.Errorf("the Grok config.toml is not valid TOML, so aggy leaves it alone: %w", err)
	}
	sk, present := doc["skills"]
	if !present {
		return doc, nil, false, nil
	}
	t, ok := sk.(map[string]any)
	if !ok {
		return nil, nil, false, errors.New("skills in Grok's config.toml is not a table")
	}
	raw, present := t["ignore"]
	if !present {
		return doc, nil, true, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, nil, false, errors.New("[skills] ignore in Grok's config.toml is not a list")
	}
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			return nil, nil, false, errors.New("[skills] ignore in Grok's config.toml is not a list of paths")
		}
		ignore = append(ignore, s)
	}
	return doc, ignore, true, nil
}

// ignoreLine is the line aggy writes: a TOML literal string, so no
// escaping is needed; a path it cannot hold is refused.
func ignoreLine(entry string) (string, error) {
	if strings.ContainsAny(entry, "'\r\n") {
		return "", fmt.Errorf("cannot write %q as a TOML literal string", entry)
	}
	return "ignore = ['" + entry + "']", nil
}

// ignoreBlock is the [skills] table aggy appends to a config without one.
func ignoreBlock(entry, nl string) (string, error) {
	line, err := ignoreLine(entry)
	return "[skills]" + nl + line + nl, err
}

func eolOf(src []byte) string {
	if bytes.Contains(src, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// skillsHeader returns the end of the line that is exactly the [skills]
// header (blanks around it allowed), or -1.
func skillsHeader(src []byte) int {
	off := 0
	for _, ln := range bytes.SplitAfter(src, []byte("\n")) {
		off += len(ln)
		if string(bytes.TrimSpace(ln)) == "[skills]" {
			return off
		}
	}
	return -1
}

// addGrokIgnore is the transform that adds entry to [skills] ignore, as
// a minimal text edit: a new [skills] table at the end, or an ignore line
// right under an existing [skills] header. The result must decode to the
// old document with only that entry added.
//
// shortcut: an existing ignore list is refused rather than extended; add
// a list-insert edit if people hit it.
func addGrokIgnore(entry string) func([]byte) ([]byte, error) {
	return func(src []byte) ([]byte, error) {
		doc, ignore, hasSkills, err := grokSkills(src)
		if err != nil {
			return nil, err
		}
		if slices.Contains(ignore, entry) {
			return src, nil
		}
		nl := eolOf(src)
		line, err := ignoreLine(entry)
		if err != nil {
			return nil, err
		}
		out := append([]byte{}, src...)
		switch {
		case ignore != nil:
			return nil, fmt.Errorf("the Grok config.toml already has a [skills] ignore list; add '%s' to it by hand", entry)
		case !hasSkills:
			// One line ending, then the table: a blank line after a file
			// that ends in one, a line break after one that does not.
			// removeGrokIgnore cuts exactly this, so the old bytes come
			// back either way.
			block, _ := ignoreBlock(entry, nl)
			if len(out) > 0 {
				out = append(out, nl...)
			}
			out = append(out, block...)
		default:
			i := skillsHeader(src)
			if i < 0 {
				return nil, errors.New("the Grok config.toml sets skills without a [skills] header; add the ignore entry by hand")
			}
			out = append(append(append([]byte{}, src[:i]...), line+nl...), src[i:]...)
		}
		sk, _ := doc["skills"].(map[string]any)
		if sk == nil {
			sk = map[string]any{}
		}
		sk["ignore"] = []any{entry}
		doc["skills"] = sk
		return checked(out, doc)
	}
}

// removeGrokIgnore is the transform that takes out what addGrokIgnore
// added: the [skills] block at the end, or the ignore line under the
// header. Anything else was edited since, and is refused.
func removeGrokIgnore(entry string) func([]byte) ([]byte, error) {
	return func(src []byte) ([]byte, error) {
		doc, ignore, _, err := grokSkills(src)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(ignore, entry) {
			return src, nil
		}
		nl := eolOf(src)
		line, err := ignoreLine(entry)
		if err != nil {
			return nil, err
		}
		block, _ := ignoreBlock(entry, nl)
		sk := doc["skills"].(map[string]any)
		delete(sk, "ignore")
		var out []byte
		switch i := skillsHeader(src); {
		case len(sk) == 0 && bytes.Equal(src, []byte(block)):
			out = []byte{} // the block was the whole file: keep it, empty
			delete(doc, "skills")
		case len(sk) == 0 && bytes.HasSuffix(src, []byte(nl+block)):
			out = src[:len(src)-len(nl+block)]
			delete(doc, "skills")
		case i >= 0 && bytes.HasPrefix(src[i:], []byte(line+nl)):
			out = append(append([]byte{}, src[:i]...), src[i+len(line+nl):]...)
		default:
			return nil, fmt.Errorf("the [skills] ignore entry in Grok's config.toml was edited; remove '%s' from it by hand", entry)
		}
		return checked(out, doc)
	}
}

// checked returns out when it decodes to want.
func checked(out []byte, want map[string]any) ([]byte, error) {
	got := map[string]any{}
	if _, err := toml.Decode(string(out), &got); err != nil || !reflect.DeepEqual(got, want) {
		return nil, errors.New("cannot change [skills] ignore in Grok's config.toml safely")
	}
	return out, nil
}

// visible returns the links through which vendor v's CLI loads the
// installed skill: each folder it scans holding an aggy entry that leads
// to it, less, for Grok, those whose real path its [skills] ignore
// covers (Grok compares real paths, ignore entry as a prefix).
func (l skillLayout) visible(v string) ([]string, error) {
	var ignore []string
	if v == "grok" {
		b, _, err := readMaybe(l.grokConfig)
		if err != nil {
			return nil, err
		}
		_, list, _, err := grokSkills(b)
		if err != nil {
			return nil, err
		}
		for _, e := range list {
			if rest, ok := strings.CutPrefix(e, "~/"); ok {
				e = filepath.Join(os.Getenv("HOME"), rest)
			}
			if c, err := safefs.CanonicalPath(e); err == nil {
				ignore = append(ignore, c)
			}
		}
	}
	var out []string
	for _, p := range l.scans(v) {
		if _, err := safefs.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		real, err := safefs.CanonicalPath(p)
		if err != nil {
			return nil, err
		}
		hidden := slices.ContainsFunc(ignore, func(i string) bool {
			return real == i || strings.HasPrefix(real, i+string(filepath.Separator))
		})
		if !hidden {
			out = append(out, p)
		}
	}
	return out, nil
}

func skillStatus(l skillLayout, stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "installed: %s\n", termsafe.Field(l.store))
	always, body := skills.Estimate(skills.Aggy)
	fmt.Fprintf(stdout, "cost: about %d tokens every turn in a CLI that loads it, %d more when invoked (bytes/4 estimate)\n", always, body)
	code := 0
	for _, v := range skillVendors {
		ok, err := l.linked(l.links[v])
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s\n", v, termsafe.Line(err.Error()))
			code = 1
			continue
		}
		vis, err := l.visible(v)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s\n", v, termsafe.Line(err.Error()))
			code = 1
			continue
		}
		state := "not linked"
		if ok {
			state = "linked"
		}
		loads := "loads nothing"
		if len(vis) > 0 {
			loads = "loads it through " + strings.Join(vis, ", ")
		}
		fmt.Fprintf(stdout, "%s: %s; %s\n", v, state, termsafe.Line(loads))
		if !ok && len(vis) > 0 {
			fmt.Fprintf(stdout, "  %s is not linked but sees the skill; run aggy skill unlink --vendor %s\n", v, v)
		}
	}
	return code
}
