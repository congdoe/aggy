package tooldef

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/safefs"
)

//go:embed builtin/*.toml
var builtinFS embed.FS

// Tool is a loaded definition and where it came from.
type Tool struct {
	Def
	BuiltIn bool
	// Source is "built-in:<file>" for a built-in, else the file's path.
	Source string
}

// Registry holds every loaded tool, sorted by name.
type Registry struct {
	tools []Tool
}

// List returns every tool, sorted by name.
func (r *Registry) List() []Tool { return slices.Clone(r.tools) }

// Get returns the tool named name.
func (r *Registry) Get(name string) (Tool, bool) {
	i := slices.IndexFunc(r.tools, func(t Tool) bool { return t.Name == name })
	if i < 0 {
		return Tool{}, false
	}
	return r.tools[i], true
}

// add adds t unless its name is taken; a built-in is never shadowed.
func (r *Registry) add(t Tool) error {
	if old, ok := r.Get(t.Name); ok {
		why := "the name is taken by " + old.Source
		if old.BuiltIn {
			why = "a built-in tool has this name, and built-ins cannot be shadowed"
		}
		// TODO(T6753): log tool.def_invalid (name collision).
		return &Error{File: t.Source, Key: "name", Msg: why}
	}
	i, _ := slices.BinarySearchFunc(r.tools, t.Name, func(x Tool, n string) int { return strings.Compare(x.Name, n) })
	r.tools = slices.Insert(r.tools, i, t)
	return nil
}

// Builtins returns a registry of aggy's own definitions. They pass the
// same validator as user definitions; an invalid one is an error.
func Builtins() (*Registry, error) {
	ents, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return nil, err
	}
	r := &Registry{}
	for _, e := range ents {
		src := "built-in:" + e.Name()
		b, err := builtinFS.ReadFile("builtin/" + e.Name())
		if err != nil {
			return nil, err
		}
		d, err := Parse(src, b, true)
		if err != nil {
			return nil, err
		}
		if e.Name() != d.Name+".toml" {
			return nil, &Error{File: src, Key: "name", Msg: "the file must be named <name>.toml"}
		}
		if err := r.add(Tool{Def: d, BuiltIn: true, Source: src}); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Dir is the folder of user definitions under the state directory.
func Dir(stateDir string) string { return filepath.Join(stateDir, "tools") }

// LoadDefault is Load on aggy's state directory ($AGGY_HOME or ~/.aggy).
// Definitions are never read from a repo or the working directory.
func LoadDefault() (*Registry, []error, error) {
	d, err := home.Dir()
	if err != nil {
		return nil, nil, err
	}
	return Load(d)
}

// Load returns the built-ins plus the user definitions in
// <stateDir>/tools/*.toml. A user file that is refused (invalid,
// untrusted, or colliding) is left out and returned in refused; err is
// for failures that leave no registry at all.
//
// A user file must be a regular file named <name>.toml, not a symlink,
// owned by the user, and writable by neither group nor others, in a tools
// folder that passes the same checks.
func Load(stateDir string) (r *Registry, refused []error, err error) {
	r, err = Builtins()
	if err != nil {
		return nil, nil, err
	}
	dir := Dir(stateDir)
	if err := CheckDir(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return r, nil, nil
		}
		return r, []error{err}, nil
	}
	ents, err := safefs.ReadDir(dir)
	if err != nil {
		return r, []error{err}, nil
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		t, err := loadUser(filepath.Join(dir, e.Name()))
		if err == nil {
			err = r.add(t)
		}
		if err != nil {
			refused = append(refused, err)
		}
	}
	return r, refused, nil
}

func loadUser(file string) (Tool, error) {
	if err := checkFile(file, false); err != nil {
		return Tool{}, err
	}
	// TODO(T6752): follow-up to review finding T59-15: check owner and mode on the descriptor
	// ReadConfig opens (fstat), not on the earlier Lstat. A swap between
	// the two needs write access to the checked tools folder.
	b, err := safefs.ReadConfig(file, MaxSize)
	if err != nil {
		// TODO(T6753): log tool.def_untrusted.
		return Tool{}, fmt.Errorf("%s: %w", file, err)
	}
	d, err := Parse(file, b, false)
	if err != nil {
		// TODO(T6753): log tool.def_invalid.
		return Tool{}, err
	}
	if filepath.Base(file) != d.Name+".toml" {
		// TODO(T6753): log tool.def_invalid.
		return Tool{}, &Error{File: file, Key: "name", Msg: fmt.Sprintf("the file must be named %s.toml", d.Name)}
	}
	return Tool{Def: d, Source: file}, nil
}

// CheckDir refuses a tools folder that is a symlink, not a directory,
// owned by another user, or writable by group or others.
func CheckDir(dir string) error { return checkFile(dir, true) }

func checkFile(p string, isDir bool) error {
	fi, err := safefs.Lstat(p)
	if err != nil {
		return err
	}
	untrusted := func(why string) error {
		// TODO(T6753): log tool.def_untrusted.
		return &Error{File: p, Msg: why, Err: ErrUntrusted}
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return untrusted("a symlink")
	case isDir && !fi.IsDir():
		return untrusted("not a directory")
	case !isDir && !fi.Mode().IsRegular():
		return untrusted("not a regular file")
	case fi.Mode().Perm()&0o022 != 0:
		return untrusted(fmt.Sprintf("writable by group or others (mode %#o)", fi.Mode().Perm()))
	}
	if why := notMine(fi); why != "" {
		return untrusted(why)
	}
	return nil
}
