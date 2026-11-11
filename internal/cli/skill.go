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
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
	"github.com/congdoe/aggy/skills"
)

const skillUsage = `Usage: aggy skill link [--vendor V] [--dry-run]
       aggy skill unlink [--vendor V] [--dry-run]
       aggy skill status
       aggy skill add <dir> [--name N] [--yes] [--replace]
       aggy skill remove <name>

Installs aggy's skills once, in ~/.aggy/skills/<name>, and links them
into each vendor CLI's user skill folder: aggy (/aggy in Claude Code and
Grok Build, $aggy in Codex), and the command skills switch, borrow, and
inbox, which put those commands in each CLI's menu (aggy catches them
before the model sees them, inside a session started with aggy):

  claude   ~/.claude/skills/<name>
  codex    ~/.agents/skills/<name>
  grok     ~/.grok/skills/<name> ($GROK_HOME/skills/<name>)

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

// ownSkill reports whether name is one of aggy's own skills (skills.Own):
// no tool or skill of the person's may take it, since it would share their
// links.
func ownSkill(name string) bool {
	return slices.ContainsFunc(skills.Own(), func(s skills.Skill) bool { return s.Name == name })
}

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

// ownLayouts are the layouts of aggy's own skills (skills.Own): the aggy
// skill first, then the command skills (T6910), each in its own folder.
func ownLayouts() ([]skillLayout, error) {
	var ls []skillLayout
	for _, sk := range skills.Own() {
		l, err := layoutFor(sk.Name)
		if err != nil {
			return nil, err
		}
		l.content = sk.Content
		ls = append(ls, l)
	}
	return ls, nil
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
	if len(args) == 0 {
		return cmdUsage(stderr, "skill", "name a subcommand", skillUsage)
	}
	if args[0] == "help" {
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
		return cmdUsage(stderr, "skill", "bad arguments", skillUsage)
	}
	vendors := skillVendors
	if *vendor != "" {
		if !slices.Contains(skillVendors, *vendor) {
			return cmdUsage(stderr, "skill", fmt.Sprintf("unknown vendor %q (claude, codex, or grok)", *vendor), "")
		}
		vendors = []string{*vendor}
	}
	ls, err := ownLayouts()
	if err != nil {
		return cmdErr(stderr, "skill", diag.CodeWireFailed, err)
	}
	switch op {
	case "status":
		if *vendor != "" || *dryRun {
			return cmdUsage(stderr, "skill status", "takes no flags", "")
		}
		return skillStatus(ls, stdout, stderr)
	case "link", "unlink":
	default:
		return cmdUsage(stderr, "skill", fmt.Sprintf("unknown subcommand %q", op), skillUsage)
	}
	// A folder under one of aggy's names that aggy did not write (a skill of
	// the person's, or one changed by hand) is neither written nor linked nor
	// unlinked here (R121).
	ls, notes, err := ownWritable(context.Background(), ls)
	if err != nil {
		return cmdErrDry(stderr, "skill "+op, diag.CodeWireFailed, err, *dryRun)
	}
	for _, n := range notes {
		fmt.Fprintf(stdout, "note: %s.\n", n)
	}
	want := map[string]bool{}
	for _, v := range vendors {
		want[v] = op == "link"
	}
	ps, err := planSkills(ls, want)
	if err != nil {
		return cmdErrDry(stderr, "skill "+op, diag.CodeWireFailed, err, *dryRun)
	}
	if *dryRun {
		if _, err := manifest.Preview(stdout, ps); err != nil {
			return cmdErrDry(stderr, "skill "+op, diag.CodeWireFailed, err, true)
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
		return cmdErr(stderr, "skill", diag.CodeWireFailed, err)
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		return cmdErr(stderr, "skill", diag.CodeWireFailed, err)
	}
	runID := manifest.NewRunID()
	cs, err := m.Run(ctx, manifest.Write, runID, ps, io.Discard)
	changed := 0
	for _, c := range cs {
		if c.Status == store.ChangeApplied {
			changed++
			fmt.Fprintf(stdout, "changed %s\n", termsafe.Field(c.File))
		}
	}
	if err != nil {
		return cmdErr(stderr, "skill "+op, diag.CodeWireFailed, err)
	}
	if changed == 0 {
		fmt.Fprintln(stdout, "Nothing to change.")
		return 0
	}
	if op == "unlink" {
		// Unlinked on purpose: the launch stops saying they are missing until
		// another aggy version runs (T6982).
		_ = s.SetNote(ctx, skillsUnlinkedNote, Version, time.Now())
	}
	fmt.Fprintf(stdout, "Done (run %s). aggy unwire --run %s undoes it.\n", runID, runID)
	return 0
}

// planSet returns the patches with each vendor of want linked (true) or unlinked
// (false) in one plan, so Grok's ignore entry matches the end state.
func (l skillLayout) planSet(want map[string]bool) ([]patch.Patch, error) {
	return planSkills([]skillLayout{l}, want)
}

// planSkills is planSet for several skills at once (aggy's own, T6910), so
// Grok's config gets one patch holding every ignore entry that changes:
// patches planned apart would each expect the file as it was. The order is
// each SKILL.md, then, when anything is linked, Grok's ignore before any
// link, so Grok never loads a skill it should not; when nothing is, the
// links go first.
func planSkills(ls []skillLayout, want map[string]bool) ([]patch.Patch, error) {
	var contents, links []patch.Patch
	var add, remove []string
	for _, l := range ls {
		c, lk, ignore, err := l.linkPatches(want)
		if err != nil {
			return nil, err
		}
		contents, links = append(contents, c...), append(links, lk...)
		switch have, err := l.grokIgnored(); {
		case err != nil:
			return nil, err
		case ignore && !have:
			add = append(add, l.store)
		case !ignore && have:
			remove = append(remove, l.store)
		}
	}
	p, err := ls[0].grokIgnoresPatch(add, remove)
	if err != nil {
		return nil, err
	}
	ps := contents
	if slices.Contains(slices.Collect(maps.Values(want)), true) {
		if p != nil {
			ps = append(ps, *p)
		}
		return append(ps, links...), nil
	}
	ps = append(ps, links...)
	if p != nil {
		ps = append(ps, *p)
	}
	return ps, nil
}

// linkPatches returns the patches that write the skill's SKILL.md (when
// anything is linked and aggy writes it) and link or unlink it for each
// vendor of want, and whether Grok's ignore should then hold the skill:
// exactly when Grok is not linked but another vendor's link is in a folder
// it scans.
func (l skillLayout) linkPatches(want map[string]bool) (contents, links []patch.Patch, ignore bool, err error) {
	after := map[string]bool{}
	for _, v := range skillVendors {
		ok, err := l.linked(l.links[v])
		if err != nil {
			return nil, nil, false, err
		}
		after[v] = ok
	}
	anyLink := slices.Contains(slices.Collect(maps.Values(want)), true)
	if anyLink && l.content != nil {
		file := filepath.Join(l.store, "SKILL.md")
		h, err := patch.HashFile(file)
		if err != nil {
			return nil, nil, false, err
		}
		content := l.content
		contents = append(contents, patch.Patch{File: file, ExpectedHash: h, Mode: 0o644,
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
			return nil, nil, false, err
		}
		ours := st.Hash == patch.LinkHash(l.store)
		switch {
		case link && !ours && st.Hash != patch.NoFile:
			return nil, nil, false, fmt.Errorf("%s exists and is not aggy's link; move it away and run this again", path)
		case link && !ours:
			links = append(links, patch.Patch{File: path, ExpectedHash: patch.NoFile, Link: l.store})
		case !link && ours:
			links = append(links, patch.Patch{File: path, ExpectedHash: st.Hash, Remove: true})
		}
		after[v] = link && (ours || st.Hash == patch.NoFile)
	}
	return contents, links, !after["grok"] && (after["claude"] || after["codex"]), nil
}

// grokIgnored reports whether Grok's [skills] ignore holds the installed
// skill now. Without a Grok home it holds nothing.
func (l skillLayout) grokIgnored() (bool, error) {
	if _, err := safefs.Lstat(l.grokHome); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	b, _, err := readMaybe(l.grokConfig)
	if err != nil {
		return false, err
	}
	return grokIgnores(b, l.store)
}

// grokIgnoresPatch returns the one patch that adds the add entries to Grok's
// [skills] ignore and takes the remove entries out, or nil when there is
// nothing to do. Grok matches ignore entries by real path, so one entry hides
// a skill through every link. Without a Grok home there is no Grok to
// configure and nothing is written; aggy skill status reports it if one
// appears later. A removal takes out only what aggy added: the file stays,
// even empty, rather than being deleted and brought back by aggy unwire
// (B14-4).
func (l skillLayout) grokIgnoresPatch(add, remove []string) (*patch.Patch, error) {
	if len(add) == 0 && len(remove) == 0 {
		return nil, nil
	}
	if _, err := safefs.Lstat(l.grokHome); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	_, h, err := readMaybe(l.grokConfig)
	if err != nil {
		return nil, err
	}
	steps := make([]func([]byte) ([]byte, error), 0, len(add)+len(remove))
	for _, e := range add {
		steps = append(steps, addGrokIgnore(e))
	}
	for _, e := range remove {
		steps = append(steps, removeGrokIgnore(e))
	}
	p := patch.Patch{File: l.grokConfig, ExpectedHash: h, Transform: func(src []byte) ([]byte, error) {
		for _, f := range steps {
			var err error
			if src, err = f(src); err != nil {
				return nil, err
			}
		}
		return src, nil
	}}
	if len(add) > 0 {
		p.Mode = 0o600
	}
	return &p, nil
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
	ignore = []string{} // present, even empty
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
			// A list already (the person's, or aggy's for another skill): the
			// entry goes at its end, when the list is on one line.
			from, to := ignoreListLine(src)
			if from < 0 {
				return nil, fmt.Errorf("the Grok config.toml has a [skills] ignore list aggy cannot extend; add '%s' to it by hand", entry)
			}
			lit, _ := ignoreLine(entry)
			lit = strings.TrimSuffix(strings.TrimPrefix(lit, "ignore = ["), "]")
			ln := string(src[from:to])
			cut := strings.LastIndex(ln, "]")
			head := strings.TrimRight(ln[:cut], " ")
			sep := ", "
			if strings.HasSuffix(head, "[") {
				sep = ""
			}
			out = append(append(append([]byte{}, src[:from]...), head+sep+lit+ln[cut:]...), src[to:]...)
			sk := doc["skills"].(map[string]any)
			sk["ignore"] = append(slices.Clone(sk["ignore"].([]any)), entry)
			return checked(out, doc)
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
		if len(ignore) > 1 {
			// One of several: only its literal goes, from the one-line list.
			from, to := ignoreListLine(src)
			lit, _ := ignoreLine(entry)
			lit = strings.TrimSuffix(strings.TrimPrefix(lit, "ignore = ["), "]")
			ln := ""
			if from >= 0 {
				ln = string(src[from:to])
			}
			switch {
			case from < 0:
			case strings.Contains(ln, ", "+lit):
				ln = strings.Replace(ln, ", "+lit, "", 1)
			case strings.Contains(ln, lit+", "):
				ln = strings.Replace(ln, lit+", ", "", 1)
			default:
				from = -1
			}
			if from < 0 {
				return nil, fmt.Errorf("the [skills] ignore list in Grok's config.toml was edited; remove '%s' from it by hand", entry)
			}
			sk["ignore"] = slices.DeleteFunc(slices.Clone(sk["ignore"].([]any)), func(v any) bool { return v == entry })
			return checked(append(append(append([]byte{}, src[:from]...), ln...), src[to:]...), doc)
		}
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

// ignoreListLine finds the ignore list in the [skills] table when it is on one
// line: where the line starts and ends, its line ending left out, or -1, -1.
func ignoreListLine(src []byte) (from, to int) {
	i := skillsHeader(src)
	if i < 0 {
		return -1, -1
	}
	off := i
	for _, ln := range bytes.SplitAfter(src[i:], []byte("\n")) {
		t := bytes.TrimSpace(ln)
		if len(t) > 0 && t[0] == '[' {
			break // the next table
		}
		if ignoreListRE.Match(t) && bytes.HasPrefix(ln, t) {
			return off, off + len(bytes.TrimRight(ln, "\r\n"))
		}
		off += len(ln)
	}
	return -1, -1
}

// ignoreListRE is an ignore list on one line.
var ignoreListRE = regexp.MustCompile(`^ignore\s*=\s*\[[^\[\]#]*\]$`)

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

func skillStatus(ls []skillLayout, stdout, stderr io.Writer) int {
	code := aggySkillStatus(ls[0], stdout, stderr)
	if _, notes, err := ownWritable(context.Background(), ls); err == nil {
		for _, n := range notes {
			fmt.Fprintf(stdout, "note: %s.\n", n)
		}
	}
	for _, l := range ls[1:] {
		name := filepath.Base(l.store)
		var parts []string
		for _, v := range skillVendors {
			ok, err := l.linked(l.links[v])
			if err != nil {
				fmt.Fprintf(stderr, "%s, %s: %s %s\n", name, v, termsafe.Line(err.Error()), dlog.Tag(diag.CodeWireFailed))
				code = 1
				continue
			}
			state := "not linked"
			if ok {
				state = "linked"
			}
			parts = append(parts, v+" "+state)
		}
		fmt.Fprintf(stdout, "command %s (/%s, $%s on Codex): %s\n", name, name, name, strings.Join(parts, ", "))
	}
	return code
}

// aggySkillStatus is the aggy skill's part of skillStatus.
func aggySkillStatus(l skillLayout, stdout, stderr io.Writer) int {
	if _, err := safefs.Stat(filepath.Join(l.store, "SKILL.md")); err == nil {
		fmt.Fprintf(stdout, "installed: %s\n", termsafe.Field(l.store))
	} else {
		fmt.Fprintln(stdout, "installed: no; aggy skill link installs it")
	}
	// An estimate for a CLI that loads it, so it says what linking would cost.
	always, body := skills.Estimate(skills.Aggy)
	fmt.Fprintf(stdout, "cost: about %d tokens every turn in a CLI that loads it, %d more when invoked (bytes/4 estimate)\n", always, body)
	code := 0
	for _, v := range skillVendors {
		ok, err := l.linked(l.links[v])
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s %s\n", v, termsafe.Line(err.Error()), dlog.Tag(diag.CodeWireFailed))
			code = 1
			continue
		}
		vis, err := l.visible(v)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s %s\n", v, termsafe.Line(err.Error()), dlog.Tag(diag.CodeWireFailed))
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

// ownState is whose a skill folder under one of aggy's own names is.
type ownState int

const (
	// ownAbsent: no SKILL.md there; aggy may write one.
	ownAbsent ownState = iota
	// ownAggy: the SKILL.md is what aggy itself last wrote there, by the
	// manifest's record.
	ownAggy
	// ownEdited: aggy wrote there once, and the file has changed since.
	ownEdited
	// ownForeign: aggy never wrote what is there, or a tool of the person's
	// has the name (a skill aggy skill add stored before the name was
	// aggy's, R121).
	ownForeign
)

// ownStates says, for each of aggy's own skills (by name), whose its installed
// SKILL.md is. It only reads: the tool definitions, the file, and the index's
// change rows, read-only.
func ownStates(ctx context.Context, ls []skillLayout) (map[string]ownState, error) {
	state, err := home.Dir()
	if err != nil {
		return nil, err
	}
	r, _, err := tooldef.Load(state)
	if err != nil {
		return nil, err
	}
	var rows []store.Change
	loaded := false
	out := map[string]ownState{}
	for _, l := range ls {
		name := filepath.Base(l.store)
		if t, ok := r.Get(name); ok && !t.BuiltIn {
			out[name] = ownForeign
			continue
		}
		file := filepath.Join(l.store, "SKILL.md")
		cur, err := patch.HashFile(file)
		if err != nil {
			return nil, err
		}
		if cur == patch.NoFile {
			out[name] = ownAbsent
			continue
		}
		if !loaded {
			loaded = true
			s, _, err := openIndexReadOnly(ctx)
			if err != nil {
				return nil, err
			}
			if s != nil {
				rows, err = s.ChangesNewestFirst(ctx, "")
				_ = s.Close()
				if err != nil {
					return nil, err
				}
			}
		}
		out[name] = ownForeign
		// Paths are compared canonical (T6975): a row written under an
		// AGGY_HOME spelled another way (through a symlink, or a folder whose
		// case differs) is the same file. Only rows for a SKILL.md in a folder
		// of this name are resolved, so the cost stays small.
		abs, _ := filepath.Abs(file)
		canon := ""
		for _, c := range rows {
			if c.Status != store.ChangeApplied && c.Status != store.ChangeUndone {
				continue
			}
			if f := filepath.Clean(c.File); f != abs {
				if filepath.Base(f) != "SKILL.md" || !strings.EqualFold(filepath.Base(filepath.Dir(f)), name) {
					continue
				}
				if canon == "" {
					canon = canonicalPath(file)
				}
				if canonicalPath(f) != canon {
					continue
				}
			}
			if c.Status == store.ChangeApplied && c.HashAfter == cur {
				out[name] = ownAggy
				break
			}
			out[name] = ownEdited // aggy wrote there once; not what is there now
		}
	}
	return out, nil
}

// ownNote says what to do about one of aggy's own skill folders it will not
// write, or "" for one it may.
func ownNote(l skillLayout, st ownState) string {
	name := filepath.Base(l.store)
	switch {
	case st == ownForeign && name == skillName:
		return fmt.Sprintf("%s holds a SKILL.md aggy did not write, so aggy leaves it; move it away and run aggy skill link for aggy's own", termsafe.Field(l.store))
	case st == ownForeign:
		return fmt.Sprintf("%s holds a skill of yours, so aggy's /%s command skill stays unlinked and yours is left as it is. To use aggy's, store yours under another name (aggy skill remove %s, then aggy skill add <dir> --name <other>) and run aggy skill link", termsafe.Field(l.store), name, name)
	case st == ownEdited:
		return fmt.Sprintf("%s was changed by hand since aggy wrote it, so aggy does not update it; to get this version's, move SKILL.md out of that folder and run aggy skill link", termsafe.Field(l.store))
	}
	return ""
}

// ownWritable returns the layouts aggy may write and link (aggy skill link,
// setup), and a note for each it leaves alone. The aggy skill is always kept,
// as before this check: only an explicit link rewrites it, as it always has.
func ownWritable(ctx context.Context, ls []skillLayout) ([]skillLayout, []string, error) {
	sts, err := ownStates(ctx, ls)
	if err != nil {
		return nil, nil, err
	}
	var keep []skillLayout
	var notes []string
	for _, l := range ls {
		st := sts[filepath.Base(l.store)]
		if filepath.Base(l.store) == skillName || st == ownAbsent || st == ownAggy {
			keep = append(keep, l)
			continue
		}
		notes = append(notes, ownNote(l, st))
	}
	return keep, notes, nil
}

// refreshPatches returns, for each of aggy's own skills linked into any CLI,
// a patch that rewrites its installed SKILL.md when that differs from the one
// this aggy carries (T6911): after an upgrade, a linked skill would otherwise
// keep the old text. Only a file whose content is what aggy itself last wrote
// there (ownAggy) is rewritten: one changed by hand, or a skill of the
// person's under the same name, is left as it is, with a note saying what to
// do (R121). An unlinked skill is left alone, and none is ever installed here.
func refreshPatches(ls []skillLayout, sts map[string]ownState) ([]patch.Patch, []string, error) {
	var ps []patch.Patch
	var notes []string
	for _, l := range ls {
		if l.content == nil {
			continue
		}
		linked := false
		for _, v := range skillVendors {
			ok, err := l.linked(l.links[v])
			if err != nil {
				return nil, nil, err
			}
			linked = linked || ok
		}
		if !linked {
			continue
		}
		file := filepath.Join(l.store, "SKILL.md")
		h, err := patch.HashFile(file)
		if err != nil {
			return nil, nil, err
		}
		if h == patch.Hash(l.content) {
			continue
		}
		if st := sts[filepath.Base(l.store)]; st != ownAggy {
			if n := ownNote(l, st); n != "" {
				notes = append(notes, n)
			}
			continue
		}
		content := l.content
		ps = append(ps, patch.Patch{File: file, ExpectedHash: h, Mode: 0o644,
			Transform: func([]byte) ([]byte, error) { return content, nil }})
	}
	return ps, notes, nil
}

// noteOnce reports whether note was not yet shown, and records it as shown, in
// aggy's own state folder (<state>/notes), so a launch says it once. A failure
// to record shows it again next time.
func noteOnce(note string) bool {
	state, err := home.Dir()
	if err != nil {
		return true
	}
	dir := filepath.Join(state, "notes")
	file := filepath.Join(dir, "skill-"+patch.Hash([]byte(note))[:16])
	if _, err := safefs.Lstat(file); err == nil {
		return false
	}
	if safefs.MkdirAll(dir, 0o700) == nil {
		_ = safefs.WriteFile(file, nil, 0o600)
	}
	return true
}

// refreshSkills applies refreshPatches through the manifest, so aggy unwire
// undoes it, and says on w what it changed, and once each what it left alone.
// It runs at each launch and in aggy setup; a failure is a warning, and it
// writes no skill at all when no installed copy of aggy's is out of date. It
// never changes a file aggy did not write.
func refreshSkills(ctx context.Context, w io.Writer) {
	err := func() error {
		ls, err := ownLayouts()
		if err != nil {
			return err
		}
		// Only a linked skill out of date is looked at further, so a launch
		// with nothing to do reads no index.
		var stale []skillLayout
		for _, l := range ls {
			file := filepath.Join(l.store, "SKILL.md")
			if h, err := patch.HashFile(file); err != nil {
				return err
			} else if h != patch.NoFile && h != patch.Hash(l.content) {
				stale = append(stale, l)
			}
		}
		if len(stale) == 0 {
			return nil
		}
		sts, err := ownStates(ctx, stale)
		if err != nil {
			return err
		}
		ps, notes, err := refreshPatches(stale, sts)
		if err != nil {
			return err
		}
		for _, n := range notes {
			if noteOnce(n) {
				fmt.Fprintf(w, "aggy: %s.\n", n)
			}
		}
		if len(ps) == 0 {
			return nil
		}
		s, err := store.OpenDefault(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		m, err := manifest.Open(s)
		if err != nil {
			return err
		}
		runID := manifest.NewRunID()
		cs, err := m.Run(ctx, manifest.Write, runID, ps, io.Discard)
		for _, c := range cs {
			if c.Status == store.ChangeApplied {
				fmt.Fprintf(w, "aggy: updated its skill at %s to this version (aggy unwire --run %s undoes it).\n", termsafe.Field(c.File), runID)
			}
		}
		return err
	}()
	if err != nil {
		fmt.Fprintf(w, "aggy: warning: its installed skills were not updated: %s\n", termsafe.Line(err.Error()))
	}
}

// linkSkills links aggy's own skills for each vendor of want through the
// manifest, as aggy skill link does, and says on w what changed.
func linkSkills(ctx context.Context, ls []skillLayout, want map[string]bool, w io.Writer) error {
	ps, err := planSkills(ls, want)
	if err != nil || len(ps) == 0 {
		return err
	}
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		return err
	}
	runID := manifest.NewRunID()
	cs, err := m.Run(ctx, manifest.Write, runID, ps, io.Discard)
	changed := 0
	for _, c := range cs {
		if c.Status == store.ChangeApplied {
			changed++
			fmt.Fprintf(w, "  changed %s\n", termsafe.Field(c.File))
		}
	}
	if changed > 0 {
		fmt.Fprintf(w, "  linked (run %s). aggy unwire --run %s undoes it.\n", runID, runID)
	}
	return err
}
