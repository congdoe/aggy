package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
)

// maxSkillEntries caps the files and folders checked in a skill folder.
const maxSkillEntries = 1000

// hasSkill reports whether t has a skill for any vendor.
func hasSkill(t tooldef.Tool) bool {
	for _, v := range adapter.Vendors() {
		if t.PartsFor(v).Skill != nil {
			return true
		}
	}
	return false
}

// toolSkillLayout is where t's skill is stored and linked: its folder is
// <state>/skills/<name>, the only place a tool's skill is linked from.
func toolSkillLayout(t tooldef.Tool) (skillLayout, error) {
	if t.Name == skillName {
		return skillLayout{}, fmt.Errorf("a tool named %s would share aggy's own skill links", skillName)
	}
	for _, v := range adapter.Vendors() {
		// TODO(T6715): a built-in skill stored elsewhere under home.
		if s := t.PartsFor(v).Skill; s != nil && s.Path != "~/.aggy/skills/"+t.Name {
			return skillLayout{}, fmt.Errorf("%s's skill is at %s; aggy links a tool's skill only from ~/.aggy/skills/%s", t.Name, s.Path, t.Name)
		}
	}
	return layoutFor(t.Name)
}

// errSkillMissing is a skill folder that is absent or not a plain
// folder of regular files.
var errSkillMissing = errors.New("skill folder missing or refused")

// checkSkillStore checks the installed skill folder before any link to
// it is made (the TODO(T6708) of tooldef.skillPath): a real folder, not
// a symlink, that resolves to itself under the state dir's skills
// folder; that safefs does not refuse; holding a regular SKILL.md; and
// holding only regular files and folders, no symlink, so no link can
// point a CLI's skill at a credential or anywhere else.
func checkSkillStore(dir string) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s %s", errSkillMissing, dir, why) }
	fi, err := safefs.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return bad("does not exist")
	case err != nil:
		return err
	case !fi.IsDir():
		return bad("is not a folder (a symlink is refused)")
	}
	parent, err := safefs.CanonicalPath(filepath.Dir(dir))
	if err != nil {
		return err
	}
	if real, err := safefs.CanonicalPath(dir); err != nil || real != filepath.Join(parent, filepath.Base(dir)) {
		return bad("resolves elsewhere")
	}
	if err := safefs.Check(dir); err != nil {
		return err
	}
	if fi, err := safefs.Lstat(filepath.Join(dir, "SKILL.md")); err != nil || !fi.Mode().IsRegular() {
		return bad("has no regular SKILL.md")
	}
	n := 0
	var walk func(d string) error
	walk = func(d string) error {
		des, err := safefs.ReadDir(d)
		if err != nil {
			return err
		}
		for _, de := range des {
			if n++; n > maxSkillEntries {
				return bad(fmt.Sprintf("holds more than %d entries", maxSkillEntries))
			}
			p := filepath.Join(d, de.Name())
			fi, err := safefs.Lstat(p)
			if err != nil {
				return err
			}
			switch {
			case fi.IsDir():
				if err := walk(p); err != nil {
					return err
				}
			case !fi.Mode().IsRegular():
				return bad("holds " + de.Name() + ", which is not a regular file or folder")
			}
		}
		return nil
	}
	return walk(dir)
}

// checkSkillLinked is aggy tool on's check of t's skill folder in dir
// against the digest recorded when it was first linked (L78-4).
func checkSkillLinked(ctx context.Context, s *store.Store, t tooldef.Tool, dir string) error {
	was, ok, err := s.ToolSkill(ctx, t.Name)
	if err != nil || !ok {
		return withCode(diag.CodeStore, err)
	}
	now, err := skillDigest(dir)
	if err != nil {
		return withCode(diag.CodeToolSkillMissing, err)
	}
	if now != was {
		return withCode(diag.CodeToolSkillChanged, fmt.Errorf("%s changed since it was first linked; aggy tool off %s everywhere, read the folder, then turn it on again", dir, t.Name))
	}
	return nil
}

// toolSkillSchema is the index version with the tool_skill table.
const toolSkillSchema = 18

// changedSkills is a launch's check of the skill pin (R80-3): for each
// tool in on with an MCP server or hook to wire and a linked skill (a
// tool_skill record), the error when its folder no longer hashes to the
// digest recorded at the first link (checkSkillLinked), keyed by name.
// The caller wires none of that tool's servers or hooks. Its links stay:
// a link is a symlink, so a CLI reads the folder as it now is, and only
// aggy tool off unlinks it. An index that cannot be read fails closed:
// every such tool has an error.
func changedSkills(ctx context.Context, on []switchboard.State) map[string]error {
	out := map[string]error{}
	var s *store.Store
	var openErr error
	opened := false
	defer func() {
		if s != nil {
			_ = s.Close()
		}
	}()
	for _, st := range on {
		if !hasSkill(st.Tool) || st.Parts.MCP == nil && len(st.Parts.Hooks) == 0 {
			continue
		}
		if !opened {
			opened = true
			var ver int
			if s, ver, openErr = openIndexReadOnly(ctx); openErr == nil && s != nil && ver < toolSkillSchema {
				_ = s.Close()
				s = nil
			}
		}
		if openErr != nil {
			out[st.Tool.Name] = withCode(diag.CodeStore, fmt.Errorf("the index could not be read to check the skill folder: %w", openErr))
			continue
		}
		if s == nil {
			continue // no index, or none with tool_skill: nothing linked
		}
		l, err := toolSkillLayout(st.Tool)
		if err == nil {
			err = checkSkillLinked(ctx, s, st.Tool, l.store)
		}
		if err != nil {
			out[st.Tool.Name] = err
		}
	}
	return out
}

// maxSkillBytes caps what skillDigest reads of a skill folder.
const maxSkillBytes = 8 << 20

// skillDigest is the sha256 over a checked skill folder's (checkSkillStore)
// regular files: each one's path relative to dir and its content, in
// path order, each length-prefixed (L78-4).
func skillDigest(dir string) (string, error) {
	h := sha256.New()
	total := 0
	var walk func(rel string) error
	walk = func(rel string) error {
		des, err := safefs.ReadDir(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		slices.SortFunc(des, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		for _, de := range des {
			r := filepath.Join(rel, de.Name())
			fi, err := safefs.Lstat(filepath.Join(dir, r))
			if err != nil {
				return err
			}
			switch {
			case fi.IsDir():
				if err := walk(r); err != nil {
					return err
				}
				continue
			case !fi.Mode().IsRegular():
				return fmt.Errorf("%w: %s holds %s, which is not a regular file or folder", errSkillMissing, dir, r)
			}
			b, err := safefs.ReadFileLimit(filepath.Join(dir, r), int64(maxSkillBytes-total))
			if err != nil {
				return err
			}
			if total += len(b); total >= maxSkillBytes {
				return fmt.Errorf("%w: %s holds more than %d bytes", errSkillMissing, dir, maxSkillBytes)
			}
			fmt.Fprintf(h, "%d:%s;%d:", len(r), filepath.ToSlash(r), len(b))
			h.Write(b)
		}
		return nil
	}
	if err := walk(""); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// skillHowTo says how to put a tool's skill in place: aggy never fetches
// one.
func skillHowTo(t tooldef.Tool, dir string) string {
	src := "the skill's folder"
	if t.Homepage != "" {
		src = fmt.Sprintf("the skill folder from %s at version %s", t.Homepage, t.Version)
	}
	return fmt.Sprintf("aggy never fetches a skill. Copy %s into %s (a plain folder holding SKILL.md, no symlinks), then run aggy tool on %s again", src, dir, t.Name)
}

// syncToolSkill links t's skill into each vendor whose setting for every
// repo has it on, and unlinks it from the others, through the manifest
// (aggy unwire undoes it). Grok's [skills] ignore gets the folder while
// Grok is off but a Claude or Codex link is in a folder it scans
// (T6709). A skill is linked for every repo: a per-repo on or a repo's
// tools_off does not change its links, and it reaches every session of
// that CLI, aggy's or not, borrow children included (L78-4).
//
// The folder's digest (skillDigest) is recorded when it is first linked,
// and aggy tool on refuses a folder that hashes otherwise
// (checkSkillLinked, tool.skill_changed), so a changed skill gets no new
// link. A link already made is a symlink, so it shows the folder as it
// now is. Once no vendor links it, the record goes, and the next link
// records the folder as it is then.
func syncToolSkill(ctx context.Context, t tooldef.Tool, b switchboard.Board, stdout io.Writer) error {
	if !hasSkill(t) {
		return nil
	}
	l, err := toolSkillLayout(t)
	if err != nil {
		return withCode(diag.CodeToolSkillMissing, err)
	}
	want := map[string]bool{}
	for _, v := range adapter.Vendors() {
		if t.PartsFor(v).Skill != nil {
			want[string(v)] = b.State(t, v, "").On
		}
	}
	return setSkillLinks(ctx, t.Name, l, want, stdout)
}

// unlinkToolSkill takes every link of aggy's to the skill folder of
// name out of the vendors' skill folders, and the folder out of Grok's
// [skills] ignore (T6727): for aggy tool off of a tool that is no longer
// loaded (refused or deleted) and aggy tool remove, whose tool is off
// everywhere and so must leave no skill any CLI loads. A folder in a
// vendor's skill folder that is not aggy's link is left alone.
func unlinkToolSkill(ctx context.Context, name string, stdout io.Writer) error {
	if name == skillName {
		return nil // aggy's own skill is aggy skill unlink's
	}
	l, err := layoutFor(name)
	if err != nil {
		return withCode(diag.CodeToolFailed, err)
	}
	return setSkillLinks(ctx, name, l, map[string]bool{"claude": false, "codex": false, "grok": false}, stdout)
}

// setSkillLinks links the skill of tool name into each vendor want has
// true and unlinks it from each it has false, through the manifest.
func setSkillLinks(ctx context.Context, name string, l skillLayout, want map[string]bool, stdout io.Writer) error {
	link := slices.Contains(slices.Collect(maps.Values(want)), true)
	var err error
	digest := ""
	if link {
		if err := checkSkillStore(l.store); err != nil {
			return withCode(diag.CodeToolSkillMissing, err)
		}
		if digest, err = skillDigest(l.store); err != nil {
			return withCode(diag.CodeToolSkillMissing, err)
		}
	}
	ps, err := l.planSet(want)
	if err != nil || len(ps) == 0 {
		return withCode(diag.CodeToolFailed, err)
	}
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return withCode(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	if link {
		// aggy tool on refused a folder that changed (checkSkillLinked);
		// the first link records it.
		if _, ok, err := s.ToolSkill(ctx, name); err != nil {
			return withCode(diag.CodeStore, err)
		} else if !ok {
			if err := s.PutToolSkill(ctx, name, digest, time.Now()); err != nil {
				return withCode(diag.CodeStore, err)
			}
		}
	} else if err := s.DeleteToolSkill(ctx, name); err != nil {
		return withCode(diag.CodeStore, err)
	}
	m, err := manifest.Open(s)
	if err != nil {
		return withCode(diag.CodeStore, err)
	}
	runID := manifest.NewRunID()
	cs, err := m.Run(ctx, manifest.Write, runID, ps, io.Discard)
	for _, c := range cs {
		if c.Status == store.ChangeApplied {
			fmt.Fprintf(stdout, "changed %s\n", termsafe.Field(c.File))
		}
	}
	if err != nil {
		return withCode(diag.CodeToolFailed, err)
	}
	fmt.Fprintf(stdout, "Skill links updated (run %s); aggy unwire --run %s undoes it.\n", runID, runID)
	return nil
}
