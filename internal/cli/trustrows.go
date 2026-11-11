package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/claudetrust"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/trustlist"
)

// The trust list's rows (T6977). Each harness or source registers its own row
// here, with what it shows, when it is offered, and what ticking it changes;
// the list (trustlist.go), aggy trust, and /aggy trust read them from
// trustRowDefs and never name a row themselves, except the shortcut aggy trust
// --opencode, which ticks one by its id.

// trustRowDef is one registered row.
type trustRowDef struct {
	trustlist.Row
	// order sorts the rows within their group.
	order int
	// bin, for a harness, is its CLI: the row is offered at a start only when
	// it is on PATH.
	bin string
	// offered, when set, says whether the row is offered at all, from the
	// person's own config.
	offered func(cfg config.Config) bool
	// current reports whether the row's change is in place already, for a
	// folder whose record has not shown the row.
	current func(tg trustTarget) bool
	// defaultOn, when set, ticks the row in a folder with no answer yet
	// (T6980: Claude's sandbox, where it can start).
	defaultOn func() bool
	// note, when set and not "", is the row's warning in place of Warning,
	// worked out when the list is shown.
	note func() string
	// plan is what ticking (on) or unticking it changes for tg, planned just
	// before the write: patches for the manifest, and lines to say.
	plan func(tg trustTarget, on bool) (trustRowPlan, error)
	// later: inside a running session the row's change waits for the next
	// start here (trustlist.Folder.Later), since the CLI it changes may be
	// writing that file now and keeps no lock aggy can take.
	later bool
	// oldTrustNote, when set, is the row's line in a repo aggy trusted before
	// the list whose answer has not shown the row: the row is new and
	// unticked there, since ticking it does more than that trust did.
	oldTrustNote string
}

// trustRowPlan is what one row changes.
type trustRowPlan struct {
	patches []patch.Patch
	// last are patches to apply after every other, as a file another tool
	// rewrites itself (Claude's) is the one most likely to refuse.
	last  []patch.Patch
	lines []string
}

// trustRowDefs are the registered rows, by group, then order.
var trustRowDefs []trustRowDef

// registerTrustRow adds a row.
func registerTrustRow(d trustRowDef) {
	trustRowDefs = append(trustRowDefs, d)
	slices.SortStableFunc(trustRowDefs, func(a, b trustRowDef) int {
		if a.Group != b.Group {
			return int(a.Group) - int(b.Group)
		}
		return a.order - b.order
	})
}

// The rows' ids.
const (
	trustRowClaude       = "claude"
	trustRowOpenCode     = "opencode"
	trustRowOpenCodeFree = "opencode-free"
	trustRowHermes       = "hermes"
)

// hermesTrustWarning is the Hermes row's warning (docs/design.md, section 15,
// "Hermes"): Hermes' approvals and approvals.deny cover terminal commands only
// (tools/approval_floors.py:23-42 at v0.23.1), and its own guard says its
// file-read denies are "not a security boundary; the terminal tool can still
// bypass" (agent/file_safety.py:343-345).
const hermesTrustWarning = "Hermes' own approvals cover terminal commands only, and its terminal tool can read any file you can; aggy's hook asks about its file tools too, but catches mistakes only"

// openCodeFreeWarning is the free-models row's warning: what freeModelsNote
// says, from OpenCode's own docs (opencode.ai/docs/zen, read 2026-11-10).
const openCodeFreeWarning = "your prompts and this folder's code go to OpenCode's own service, not a plan or key of yours; OpenCode's docs say some free models' data may be kept or used to train the model (opencode.ai/docs/zen)"

func init() {
	registerTrustRow(trustRowDef{
		Row: trustlist.Row{ID: trustRowClaude, Label: "Claude Code", Group: trustlist.Harness},
		bin: vendorBin(adapter.Claude), order: 10,
		// Never ticked from aggy's own record (PR #130 review): aggy trust
		// never wrote Claude's own trust, so a repo trusted before the list
		// shows the row new and unticked, saying so.
		oldTrustNote: "ticking it also records Claude's own trust for this folder, which aggy trust did not; aggy's own trust for this repo stays as it is either way",
		plan:         claudeRowPlan,
		later:        true,
	})
	registerTrustRow(trustRowDef{
		Row: trustlist.Row{ID: trustRowOpenCode, Label: "OpenCode (the ChatGPT and Grok harness)", Group: trustlist.Harness,
			Warning: "this repo's own OpenCode plugins can run code, as you, on the full screen"},
		bin: vendorBin(adapter.OpenCode), order: 20,
		current: func(tg trustTarget) bool {
			on, _ := openCodeTrusted(tg.root)
			return on
		},
		plan: openCodeRowPlan,
	})
	registerTrustRow(trustRowDef{
		Row: trustlist.Row{ID: trustRowHermes, Label: "Hermes Agent", Group: trustlist.Harness, Warning: hermesTrustWarning},
		bin: vendorBin(adapter.Hermes), order: 30,
		// Its tick is the folder's record itself, which the launch reads
		// (hermesTicked): unticked, aggy does not start Hermes here.
		plan: func(trustTarget, bool) (trustRowPlan, error) { return trustRowPlan{}, nil },
	})
	registerTrustRow(trustRowDef{
		Row:     trustlist.Row{ID: trustRowOpenCodeFree, Label: "OpenCode free models", Group: trustlist.Source, Warning: openCodeFreeWarning},
		order:   10,
		offered: func(cfg config.Config) bool { return cfg.OpenCode.FreeModels },
		// Its tick is the folder's record itself, which the launch reads
		// (openCodeFreeHere).
		plan: func(trustTarget, bool) (trustRowPlan, error) { return trustRowPlan{}, nil },
	})
}

// trustLookPath finds a row's CLI; a test sets it.
var trustLookPath = func(bin string) bool {
	_, err := probe.LookPath(bin)
	return err == nil
}

// trustRows are the rows to show: those offered by cfg, and, of the harnesses,
// those installed, or every harness when all is set (aggy trust from the
// shell, where a harness not installed is marked so).
func trustRows(cfg config.Config, all bool) []trustRowDef {
	var out []trustRowDef
	for _, d := range trustRowDefs {
		if d.offered != nil && !d.offered(cfg) {
			continue
		}
		if d.bin != "" && !trustLookPath(d.bin) {
			if !all {
				continue
			}
			d.Missing = true
		}
		if d.note != nil {
			if n := d.note(); n != "" {
				d.Warning = n
			}
		}
		out = append(out, d)
	}
	return out
}

// claudeRowPlan is the Claude Code row: ticked, Claude's own trust key for the
// folder, so its trust screen does not appear (claudetrust). The file is read
// now, just before the write, and the change is guarded by its hash: changed
// underneath, it is refused. A file that is not there is not made, and Claude
// asks itself, once.
func claudeRowPlan(tg trustTarget, on bool) (trustRowPlan, error) {
	var p trustRowPlan
	if !on {
		return p, nil
	}
	path, err := claudetrust.Path(os.Getenv)
	if err != nil {
		p.lines = append(p.lines, "Claude Code: aggy cannot find Claude's config, so Claude asks you itself: "+err.Error())
		return p, nil
	}
	b, err := safefs.ReadFileLimit(path, claudetrust.MaxFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		p.lines = append(p.lines, "Claude Code: "+path+" is not there yet, so aggy does not make it; Claude asks you itself once.")
		return p, nil
	case err != nil:
		// Refused for Claude alone: the rest of the answer is saved, and the
		// line says what to do. Never the file's content: only where.
		p.lines = append(p.lines, fmt.Sprintf("Claude Code: refused: %s cannot be read, so Claude asks you itself: %v; aggy trust %s tries again %s",
			path, err, tg.root, dlog.Tag(diag.CodeTrustBadFile)))
		return p, nil
	}
	before, err := claudetrust.State(b, tg.root)
	if err != nil {
		p.lines = append(p.lines, fmt.Sprintf("Claude Code: refused: %s is left as it is, so Claude asks you itself: %v; fix the file and run aggy trust %s again, or answer Claude's own question %s",
			path, err, tg.root, dlog.Tag(diag.CodeTrustBadFile)))
		return p, nil
	}
	if before == claudetrust.WasTrue {
		p.lines = append(p.lines, "Claude Code: Claude already trusts this folder.")
		return p, nil
	}
	folder := tg.root
	p.last = append(p.last, patch.Patch{File: path, ExpectedHash: patch.Hash(b), NoBackup: true,
		Reverse: claudetrust.Reverse(before, folder),
		Summary: "set " + claudetrust.Key + " for " + folder + " (Claude's own trust; nothing else in the file changes)",
		Transform: func(src []byte) ([]byte, error) {
			return claudetrust.Set(src, folder)
		}})
	p.lines = append(p.lines, "Claude Code: "+path+" gains "+claudetrust.Key+" for this folder, so Claude does not ask (checked live on Claude Code 2.1.322; a subfolder is not known to be covered).")
	return p, nil
}

// openCodeRowPlan is the OpenCode row: what aggy trust --opencode records,
// added when ticked and taken out when unticked.
func openCodeRowPlan(tg trustTarget, on bool) (trustRowPlan, error) {
	var p trustRowPlan
	path, err := openCodeTrustFile()
	if err != nil {
		return p, err
	}
	key, err := safefs.CanonicalPath(tg.root)
	if err != nil {
		return p, err
	}
	pt, changes, err := trustRepoPatch(path, key, on)
	if err != nil {
		return p, err
	}
	if changes {
		p.patches = append(p.patches, pt)
		if on {
			p.lines = append(p.lines, "OpenCode: this repo opens the full OpenCode screen under aggy; its screen plugins will run.")
		} else {
			p.lines = append(p.lines, "OpenCode: no longer trusted here; it gets opencode mini.")
		}
	}
	return p, nil
}
