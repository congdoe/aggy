package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	defFile bool          // that file exists, loaded or not
	store   string        // ~/.aggy/skills/<name>
	stored  bool          // something is there
}

func lookupSkillTarget(name string) (skillTarget, error) {
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
	if _, err := safefs.Lstat(st.defPath); err == nil {
		st.defFile = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return st, err
	}
	if _, err := safefs.Lstat(st.store); err == nil {
		st.stored = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return st, err
	}
	return st, nil
}

// skillInUse refuses while tool name is on anywhere or its skill is
// still linked into any vendor: --replace and remove never touch a
// skill a CLI loads.
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
	st, err := lookupSkillTarget(name)
	if err != nil {
		return err
	}
	// The source: a real folder (not a symlink) of regular files, read
	// whole once, so what is shown is what is stored.
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

	// Name clashes.
	exists := func(what string) error {
		return withCode(diag.CodeToolSkillExists, fmt.Errorf("%s %s; pick another --name, or pass --replace", what, name))
	}
	var def []byte
	switch {
	case st.builtIn != nil && !hasSkill(*st.builtIn):
		return withCode(diag.CodeToolSkillExists, fmt.Errorf("%s is a built-in tool with no skill; pick another --name", name))
	case st.builtIn != nil && !a.replace:
		return exists("a built-in tool has the name")
	case st.builtIn == nil && st.defFile && (st.user == nil || !st.user.SkillOnly()):
		// Only a skill-only user tool is replaced; anything else is the
		// person's to remove.
		return withCode(diag.CodeToolSkillExists, fmt.Errorf("%s holds a user tool that is not only a skill, or a refused one; aggy tool remove %s first", st.defPath, name))
	case st.defFile && !a.replace:
		return exists("a user tool has the name")
	}
	if st.stored && !a.replace {
		return exists(st.store + " already holds the skill")
	}
	// Even a fresh name: a link left from an earlier tool would show the
	// new folder to a CLI at once.
	if err := skillInUse(ctx, name, st); err != nil {
		return err
	}
	if st.builtIn == nil {
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
	fmt.Fprintf(stdout, "  %d %s, %d bytes:\n", len(files), plural(len(files), "file", "files"), total)
	for _, f := range files {
		fmt.Fprintf(stdout, "    %s (%d bytes)\n", termsafe.Field(filepath.ToSlash(f.rel)), len(f.data))
	}
	fmt.Fprintf(stdout, "  sha256 (paths and contents; the pin aggy tool on checks): %s\n", digest)
	fmt.Fprintf(stdout, "  stored in: %s\n", termsafe.Field(st.store))
	if st.stored {
		fmt.Fprintln(stdout, "  replaces the folder there now")
	}
	if def == nil {
		fmt.Fprintf(stdout, "  definition: none; the built-in %s links this folder\n", name)
	} else {
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

	// Store: the old folder goes only now, after the yes.
	if st.stored {
		if err := safefs.RemoveAll(st.store); err != nil {
			return err
		}
	}
	if err := storeSkill(st.store, files, digest); err != nil {
		if rerr := safefs.RemoveAll(st.store); rerr != nil {
			return fmt.Errorf("%w; %s could not be cleaned up: %v", err, st.store, rerr)
		}
		return err
	}
	undo := func(err error) error {
		if rerr := safefs.RemoveAll(st.store); rerr != nil {
			return fmt.Errorf("not added: %w; %s could not be removed: %v; remove it by hand", err, st.store, rerr)
		}
		return fmt.Errorf("not added: %w", err)
	}
	if def != nil {
		if st.defFile {
			if err := safefs.Remove(st.defPath); err != nil {
				return undo(err)
			}
		}
		if err := safefs.MkdirAll(filepath.Dir(st.defPath), 0o700); err != nil {
			return undo(err)
		}
		if err := tooldef.CheckDir(filepath.Dir(st.defPath)); err != nil {
			return undo(err)
		}
		if err := writeNew(st.defPath, def); err != nil {
			return undo(err)
		}
		if err := recordToolAdd(ctx, name, tooldef.Digest(def)); err != nil {
			_ = safefs.Remove(st.defPath)
			return undo(err)
		}
	}
	// The pin: a later change to the folder is caught (L78-4).
	if err := putSkillPin(ctx, name, digest); err != nil {
		if def != nil {
			_ = safefs.Remove(st.defPath)
		}
		return undo(err)
	}
	fmt.Fprintf(stdout, "Added %s (off). aggy tool on %s [--vendor V] links it; aggy skill remove %s deletes it.\n", name, name, name)
	return nil
}

// storeSkill writes files into dir, a folder it creates, and checks the
// copy hashes to digest.
func storeSkill(dir string, files []skillFile, digest string) error {
	if err := safefs.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := safefs.Mkdir(dir, 0o700); err != nil {
		return err
	}
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
	if err := checkSkillStore(dir); err != nil {
		return err
	}
	if got, err := skillDigest(dir); err != nil || got != digest {
		return withCode(diag.CodeToolSkillRefused, fmt.Errorf("the copy in %s does not match what was shown (%v)", dir, err))
	}
	return nil
}

func putSkillPin(ctx context.Context, name, digest string) error {
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return withCode(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	return withCode(diag.CodeStore, s.PutToolSkill(ctx, name, digest, time.Now()))
}

// skillRemove is aggy skill remove: it deletes the stored folder and the
// user definition aggy skill add wrote, and is refused while the tool is
// on anywhere or its skill still linked.
func skillRemove(ctx context.Context, name string, stdout io.Writer) error {
	if err := delegated(); err != nil {
		return err
	}
	st, err := lookupSkillTarget(name)
	if err != nil {
		return err
	}
	if !st.stored && !st.defFile {
		return withCode(diag.CodeToolUnknown, fmt.Errorf("no skill %s in %s", name, st.store))
	}
	if st.defFile && st.user != nil && !st.user.SkillOnly() {
		return withCode(diag.CodeToolScope, fmt.Errorf("%s is a user tool that is not only a skill; aggy tool remove %s removes its definition", name, name))
	}
	if err := skillInUse(ctx, name, st); err != nil {
		return err
	}
	if st.defFile {
		if err := toolRemove(ctx, name, stdout); err != nil {
			return err
		}
	}
	if st.stored {
		if err := safefs.RemoveAll(st.store); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Removed %s.\n", termsafe.Field(st.store))
	}
	s, err := openIndex(ctx)
	if err != nil || s == nil {
		return withCode(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	return withCode(diag.CodeStore, s.DeleteToolSkill(ctx, name))
}
