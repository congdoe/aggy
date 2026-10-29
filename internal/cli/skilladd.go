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
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
	"github.com/congdoe/aggy/skills"
)

// skillAddArgs are aggy skill add's and remove's parsed arguments.
type skillAddArgs struct {
	pos           []string
	name          string
	yes, replace  bool
	hasName, flag bool // flag: any flag at all
}

func parseSkillAddArgs(args []string) (skillAddArgs, bool) {
	var a skillAddArgs
	for i := 0; i < len(args); i++ {
		w := args[i]
		name, val, hasVal := strings.Cut(w, "=")
		switch {
		case w == "--yes":
			a.yes, a.flag = true, true
		case w == "--replace":
			a.replace, a.flag = true, true
		case name == "--name":
			if !hasVal {
				if i+1 >= len(args) {
					return a, false
				}
				i++
				val = args[i]
			}
			if a.hasName {
				return a, false
			}
			a.name, a.hasName, a.flag = val, true, true
		case strings.HasPrefix(w, "-"):
			return a, false
		default:
			a.pos = append(a.pos, w)
		}
	}
	return a, len(a.pos) == 1
}

// skillManage is aggy skill add and aggy skill remove: a user's own
// skill, stored once in ~/.aggy/skills/<name> and linked by aggy tool
// on like any tool's skill (T6706).
func skillManage(args []string, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	ctx := context.Background()
	cmd := "skill " + args[0]
	a, ok := parseSkillAddArgs(args[1:])
	if !ok || args[0] == "remove" && a.flag {
		refuse(ctx, stderr, cmd, diag.EvToolRefused, diag.CodeUsage, "bad arguments", diag.Rec{})
		fmt.Fprintf(stderr, "\n%s", skillUsage)
		return 2
	}
	var err error
	if args[0] == "add" {
		err = skillAdd(ctx, a, in, tty, stdout)
	} else {
		err = skillRemove(ctx, a.pos[0], stdout)
	}
	if err != nil {
		toolRefuse(ctx, stderr, cmd, diag.EvToolRefused, err)
		return 1
	}
	return 0
}

// skillTarget is what a skill name is already used by.
type skillTarget struct {
	builtIn *tooldef.Tool // a built-in of the name
	user    *tooldef.Tool // a loaded user tool of the name
	defPath string        // ~/.aggy/tools/<name>.toml
	defFI   fs.FileInfo   // that file, loaded or not; nil when absent
	store   string        // ~/.aggy/skills/<name>
	storeFI fs.FileInfo   // what is there; nil when absent
	pin     string        // its tool_skill digest, or ""
}

func (st skillTarget) defFile() bool { return st.defFI != nil }
func (st skillTarget) stored() bool  { return st.storeFI != nil }

func lookupSkillTarget(ctx context.Context, name string) (skillTarget, error) {
	if !tooldef.ValidName(name) || name == skillName {
		return skillTarget{}, withCode(diag.CodeUsage, fmt.Errorf("%q cannot name a skill: want 1 to 32 of a-z, 0-9, and -, starting with a letter, and not %s", name, skillName))
	}
	state, err := home.Dir()
	if err != nil {
		return skillTarget{}, err
	}
	st := skillTarget{
		defPath: filepath.Join(tooldef.Dir(state), name+".toml"),
		store:   filepath.Join(state, "skills", name),
	}
	r, _, err := tooldef.Load(state)
	if err != nil {
		return st, err
	}
	if t, ok := r.Get(name); ok && t.BuiltIn {
		st.builtIn = &t
	} else if ok {
		st.user = &t
	}
	if st.defFI, err = safefs.Lstat(st.defPath); errors.Is(err, fs.ErrNotExist) {
		st.defFI = nil
	} else if err != nil {
		return st, err
	}
	if st.storeFI, err = safefs.Lstat(st.store); errors.Is(err, fs.ErrNotExist) {
		st.storeFI = nil
	} else if err != nil {
		return st, err
	}
	s, ver, err := openIndexReadOnly(ctx)
	if err != nil {
		return st, withCode(diag.CodeStore, err)
	}
	if s != nil {
		defer func() { _ = s.Close() }()
		if ver >= toolSkillSchema {
			if st.pin, _, err = s.ToolSkill(ctx, name); err != nil {
				return st, withCode(diag.CodeStore, err)
			}
		}
	}
	return st, nil
}

// sameTarget reports whether nothing about the name changed between two
// lookups: the tools of the name, the definition file, the stored
// folder (its identity and its own mtime), and the pin (M1).
func sameTarget(a, b skillTarget) bool {
	digest := func(t *tooldef.Tool) string {
		if t == nil {
			return ""
		}
		return t.Digest
	}
	same := func(x, y fs.FileInfo) bool {
		return x == nil && y == nil || x != nil && y != nil && os.SameFile(x, y) && x.ModTime().Equal(y.ModTime()) && x.Size() == y.Size()
	}
	return (a.builtIn == nil) == (b.builtIn == nil) && digest(a.user) == digest(b.user) &&
		same(a.defFI, b.defFI) && same(a.storeFI, b.storeFI) && a.pin == b.pin
}

// skillClash refuses a name already in use, unless --replace may take
// it, and reports whether a user definition is to be written.
func skillClash(name string, st skillTarget, replace bool) (writeDef bool, err error) {
	exists := func(what string) error {
		return withCode(diag.CodeToolSkillExists, fmt.Errorf("%s %s; pick another --name, or pass --replace", what, name))
	}
	switch {
	case st.builtIn != nil && (!hasSkill(*st.builtIn) || !st.builtIn.SkillOnly()):
		return false, withCode(diag.CodeToolSkillExists, fmt.Errorf("%s is a built-in tool that is not only a skill; pick another --name", name))
	case st.builtIn != nil && !replace:
		return false, exists("a built-in tool has the name")
	case st.builtIn == nil && st.defFile() && (st.user == nil || !st.user.SkillOnly()):
		// Only a skill-only user tool is replaced; anything else is the
		// person's to remove.
		return false, withCode(diag.CodeToolSkillExists, fmt.Errorf("%s holds a user tool that is not only a skill, or a refused one; aggy tool remove %s first", st.defPath, name))
	case st.defFile() && !replace:
		return false, exists("a user tool has the name")
	case st.stored() && !replace:
		return false, exists(st.store + " already holds the skill")
	}
	return st.builtIn == nil, nil
}

// skillInUse refuses while tool name is on anywhere or its skill is
// still linked into any vendor: add (even of a fresh name, since a link
// left from an earlier tool would show the new folder at once) and
// remove never touch a skill a CLI loads.
func skillInUse(ctx context.Context, name string, st skillTarget) error {
	stillOn := func(why string) error {
		return withCode(diag.CodeToolStillOn, fmt.Errorf("%s %s; aggy tool off %s first", name, why, name))
	}
	if st.builtIn != nil && st.builtIn.Default {
		return stillOn("is a built-in that is on by default")
	}
	sws, err := toolSwitches(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(sws, func(s store.ToolSwitch) bool { return s.Tool == name && s.On }) {
		return stillOn("is on")
	}
	l, err := layoutFor(name)
	if err != nil {
		return err
	}
	for _, v := range skillVendors {
		if ok, err := l.linked(l.links[v]); err != nil {
			return err
		} else if ok {
			return stillOn("is still linked into " + v)
		}
	}
	return nil
}

// checkSkillTarget is every check on the name: the clash and that the
// tool is off and unlinked.
func checkSkillTarget(ctx context.Context, name string, st skillTarget, replace bool) (bool, error) {
	writeDef, err := skillClash(name, st, replace)
	if err == nil {
		err = skillInUse(ctx, name, st)
	}
	return writeDef, err
}

// skillDescription is the definition's description: the frontmatter's,
// kept to printable ASCII without quotes or backslashes, so it is a
// TOML basic string with no escape, or a fixed one.
func skillDescription(name, front string) string {
	d := strings.Map(func(r rune) rune {
		if r < ' ' || r > '~' || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, front)
	if d = strings.TrimSpace(d); d == "" {
		return "The skill " + name + ", stored by aggy skill add"
	}
	return strings.TrimSpace(d[:min(len(d), 200)])
}

// skillDefFor is the minimal user definition for a skill-only tool: off,
// global, linked for every vendor from ~/.aggy/skills/<name>. Its
// version names the folder's digest.
func skillDefFor(name, desc, digest string) []byte {
	return fmt.Appendf(nil, `# Written by aggy skill add: a skill only, off until aggy tool on %s.
name = %q
description = %q
version = "0+sha256.%s"
scope = "global"
network = false

[[skill]]
vendors = ["claude", "codex", "grok"]
path = "~/.aggy/skills/%s"

[cost]
method = "skill-md"
`, name, name, desc, digest[:12], name)
}

// frontWarnings are the preview's warnings about the frontmatter name a
// CLI lists the skill by (L4).
func frontWarnings(name, frontName string) []string {
	var out []string
	switch {
	case frontName == "":
		out = append(out, "its frontmatter has no name")
	case frontName != name:
		out = append(out, "its frontmatter names it "+termsafe.Field(frontName)+", not "+name+": a CLI lists it by that name")
	}
	if frontName == skillName {
		out = append(out, "that is aggy's own skill's name")
	} else if r, err := tooldef.Builtins(); err == nil && frontName != name {
		if _, ok := r.Get(frontName); ok {
			out = append(out, "that is the name of aggy's built-in tool "+frontName)
		}
	}
	return out
}

func skillAdd(ctx context.Context, a skillAddArgs, in io.Reader, tty bool, stdout io.Writer) error {
	if err := delegated(); err != nil {
		return err
	}
	src, err := filepath.Abs(a.pos[0])
	if err != nil {
		return err
	}
	name := filepath.Base(src)
	if a.hasName {
		name = a.name
	}
	st, err := lookupSkillTarget(ctx, name)
	if err != nil {
		return err
	}
	// The source: a real folder (not a symlink) of regular files, read
	// whole once, so what is shown is what is stored; readSkillTree walks
	// it through descriptors (T6820).
	refused := func(err error) error { return withCode(diag.CodeToolSkillRefused, err) }
	if fi, err := safefs.Lstat(src); err != nil {
		return err
	} else if !fi.IsDir() {
		return refused(fmt.Errorf("%s is not a folder (a symlink is refused)", src))
	}
	files, err := readSkillTree(src)
	if err != nil {
		return refused(err)
	}
	i := slices.IndexFunc(files, func(f skillFile) bool { return f.rel == "SKILL.md" })
	if i < 0 {
		return refused(fmt.Errorf("%s has no regular SKILL.md", src))
	}
	digest := digestSkillFiles(files)
	frontName, frontDesc := skills.Front(files[i].data)

	writeDef, err := checkSkillTarget(ctx, name, st, a.replace)
	if err != nil {
		return err
	}
	var def []byte
	if writeDef {
		def = skillDefFor(name, skillDescription(name, frontDesc), digest)
		if _, err := tooldef.Parse(st.defPath, def, false); err != nil {
			return err // a bug: the definition aggy writes is always valid
		}
	}

	// Show it all before storing anything.
	total := 0
	for _, f := range files {
		total += len(f.data)
	}
	fmt.Fprintf(stdout, "Skill %s, from %s\n", name, termsafe.Field(src))
	fmt.Fprintf(stdout, "  frontmatter name: %s\n", termsafe.Field(frontName))
	fmt.Fprintf(stdout, "  frontmatter description: %s\n", termsafe.Field(frontDesc))
	for _, w := range frontWarnings(name, frontName) {
		fmt.Fprintf(stdout, "  warning: %s\n", w)
	}
	fmt.Fprintf(stdout, "  %d %s, %d bytes:\n", len(files), plural(len(files), "file", "files"), total)
	for _, f := range files {
		fmt.Fprintf(stdout, "    %s (%d bytes)\n", termsafe.Field(filepath.ToSlash(f.rel)), len(f.data))
	}
	fmt.Fprintf(stdout, "  sha256 (paths and contents; the pin aggy tool on checks): %s\n", digest)
	fmt.Fprintf(stdout, "  stored in: %s\n", termsafe.Field(st.store))
	if st.stored() {
		fmt.Fprintln(stdout, "  replaces the folder there now")
	}
	switch {
	case def == nil:
		fmt.Fprintf(stdout, "  definition: none; the built-in %s links this folder\n", name)
	case st.defFile():
		fmt.Fprintf(stdout, "  definition: %s, a user tool that is only this skill, off; replaces the user definition there now\n", termsafe.Field(st.defPath))
	default:
		fmt.Fprintf(stdout, "  definition: %s, a user tool that is only this skill, off\n", termsafe.Field(st.defPath))
	}
	fmt.Fprintf(stdout, "  Nothing is linked until aggy tool on %s [--vendor V]; a linked skill reaches every session of that CLI.\n", name)
	if !tty {
		return errors.New("not added: adding a skill needs you at a terminal, because every CLI it is linked into loads it; --yes only skips the question there")
	}
	if !a.yes {
		fmt.Fprintf(stdout, "Add %s? [y/N] ", name)
		line, _ := bufio.NewReader(io.LimitReader(in, maxAnswer)).ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			fmt.Fprintln(stdout, "Nothing added.")
			return nil
		}
	}

	// M1: anything may have changed while the question waited (a tool on
	// in another shell, a folder put there): check it all again.
	now, err := lookupSkillTarget(ctx, name)
	if err == nil && !sameTarget(st, now) {
		err = withCode(diag.CodeToolSkillExists, fmt.Errorf("%s changed while the question waited; nothing was added, run it again", name))
	}
	if err == nil {
		_, err = checkSkillTarget(ctx, name, now, a.replace)
	}
	if err != nil {
		return err
	}
	if err := installSkill(ctx, name, st, files, digest, def); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Added %s (off). aggy tool on %s [--vendor V] links it; aggy skill remove %s deletes it.\n", name, name, name)
	return nil
}

// installSkill puts the shown files in st.store and def (when not nil)
// at st.defPath, then records them (H1). The copy is staged in a temp
// folder beside the store and checked there; an old folder or
// definition is moved aside, never removed, until everything else is
// done, so any failure puts back what was there. Only what this call
// made is cleaned up.
func installSkill(ctx context.Context, name string, st skillTarget, files []skillFile, digest string, def []byte) (err error) {
	var undo []func() error
	defer func() {
		if err == nil {
			return
		}
		for i := len(undo) - 1; i >= 0; i-- {
			if uerr := undo[i](); uerr != nil {
				err = fmt.Errorf("%w; putting things back failed too: %v; check %s and %s by hand", err, uerr, st.store, st.defPath)
			}
		}
		err = fmt.Errorf("not added: %w", err)
	}()
	skillsDir := filepath.Dir(st.store)
	if err := safefs.MkdirAll(skillsDir, 0o700); err != nil {
		return err
	}
	tmp, err := safefs.MkdirTemp(skillsDir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	undo = append(undo, func() error { return removeIfThere(tmp) })
	if err := writeSkillFiles(tmp, files); err != nil {
		return err
	}
	if err := checkSkillStore(tmp); err != nil {
		return err
	}
	if got, err := skillDigest(tmp); err != nil || got != digest {
		return withCode(diag.CodeToolSkillRefused, fmt.Errorf("the copy in %s does not match what was shown (%v)", tmp, err))
	}
	tmpFI, err := safefs.Lstat(tmp)
	if err != nil {
		return err
	}
	var tmpDef string
	var tmpDefFI fs.FileInfo
	if def != nil {
		toolsDir := filepath.Dir(st.defPath)
		if err := safefs.MkdirAll(toolsDir, 0o700); err != nil {
			return err
		}
		if err := tooldef.CheckDir(toolsDir); err != nil {
			return err
		}
		tmpDef = filepath.Join(toolsDir, "."+filepath.Base(tmp)+".def") // not *.toml: never loaded
		if err := writeNew(tmpDef, def); err != nil {
			return err
		}
		undo = append(undo, func() error { return removeIfThere(tmpDef) })
		if tmpDefFI, err = safefs.Lstat(tmpDef); err != nil {
			return err
		}
	}
	// Swap: the old folder and definition aside (with the identity the
	// checks saw), the new ones in.
	aside := tmp + ".old"
	if st.stored() {
		if err := safefs.RenameNoReplace(st.store, aside, st.storeFI); err != nil {
			return err
		}
		undo = append(undo, func() error { return safefs.RenameNoReplace(aside, st.store, st.storeFI) })
	}
	if err := safefs.RenameNoReplace(tmp, st.store, tmpFI); err != nil {
		return err
	}
	undo = append(undo, func() error { return safefs.RenameNoReplace(st.store, tmp, tmpFI) })
	oldDef := tmpDef + ".old"
	if def != nil {
		if st.defFile() {
			if err := safefs.RenameNoReplace(st.defPath, oldDef, st.defFI); err != nil {
				return err
			}
			undo = append(undo, func() error { return safefs.RenameNoReplace(oldDef, st.defPath, st.defFI) })
		}
		if err := safefs.RenameNoReplace(tmpDef, st.defPath, tmpDefFI); err != nil {
			return err
		}
		undo = append(undo, func() error { return safefs.RenameNoReplace(st.defPath, tmpDef, tmpDefFI) })
	}
	if err := recordSkillAdd(ctx, name, def, digest); err != nil {
		return err
	}
	// Done: what was moved aside goes. A failure here leaves a dot entry
	// no tool loads or links.
	if st.stored() {
		_ = safefs.RemoveAll(aside)
	}
	if def != nil && st.defFile() {
		_ = safefs.Remove(oldDef)
	}
	return nil
}

// recordSkillAdd records the added definition (as aggy tool add does)
// and the pin; a failed pin puts back the old add record.
func recordSkillAdd(ctx context.Context, name string, def []byte, digest string) error {
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return withCode(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	oldAdded, hadAdded, err := s.ToolAdded(ctx, name)
	if err != nil {
		return withCode(diag.CodeStore, err)
	}
	if def != nil {
		if err := s.DeleteToolSwitches(ctx, name); err != nil {
			return withCode(diag.CodeStore, err)
		}
		if err := s.PutToolAdded(ctx, name, tooldef.Digest(def), time.Now()); err != nil {
			return withCode(diag.CodeStore, err)
		}
	}
	// The pin: a later change to the folder is caught (L78-4).
	if err := s.PutToolSkill(ctx, name, digest, time.Now()); err != nil {
		if def != nil && hadAdded {
			_ = s.PutToolAdded(ctx, name, oldAdded, time.Now())
		}
		return withCode(diag.CodeStore, err)
	}
	return nil
}

// removeIfThere removes a path this run made, if it is still there.
func removeIfThere(p string) error {
	if _, err := safefs.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return safefs.RemoveAll(p)
}

// writeSkillFiles writes files into dir, a new folder of this run's.
func writeSkillFiles(dir string, files []skillFile) error {
	for _, f := range files {
		p := filepath.Join(dir, f.rel)
		if err := safefs.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		perm := fs.FileMode(0o600)
		if f.perm&0o100 != 0 {
			perm = 0o700 // a script the skill runs stays executable
		}
		if err := writeNewMode(p, f.data, perm); err != nil {
			return err
		}
	}
	return nil
}

// skillRemove is aggy skill remove: it deletes the stored folder and the
// user definition aggy skill add wrote, and is refused while the tool is
// on anywhere or its skill still linked.
func skillRemove(ctx context.Context, name string, stdout io.Writer) error {
	if err := delegated(); err != nil {
		return err
	}
	st, err := lookupSkillTarget(ctx, name)
	if err != nil {
		return err
	}
	if !st.stored() && !st.defFile() {
		return withCode(diag.CodeToolUnknown, fmt.Errorf("no skill %s in %s", name, st.store))
	}
	if st.defFile() && (st.user == nil || !st.user.SkillOnly()) {
		// M3: a refused or invalid file, or a tool with more than a
		// skill, is aggy tool remove's, which says what it removes.
		return withCode(diag.CodeToolScope, fmt.Errorf("%s is not a definition that is only a skill; aggy tool remove %s removes it", st.defPath, name))
	}
	if err := skillInUse(ctx, name, st); err != nil {
		return err
	}
	if st.defFile() {
		if err := toolRemove(ctx, name, stdout); err != nil {
			return err
		}
	}
	if st.stored() {
		if err := safefs.RemoveAll(st.store); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Removed %s.\n", termsafe.Field(st.store))
	}
	return forgetSkillPin(ctx, name)
}

// forgetSkillPin deletes name's skill pin, when there is an index.
func forgetSkillPin(ctx context.Context, name string) error {
	s, err := openIndex(ctx)
	if err != nil || s == nil {
		return withCode(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	return withCode(diag.CodeStore, s.DeleteToolSkill(ctx, name))
}
