package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

// OpenCode's free models (T6956, docs/design.md section 15): the person's
// [opencode] free_models adds OpenCode's own provider to a session's
// allow-list. It is on screen only: config.Validate keeps opencode out of
// every borrow route, so no borrow child reaches it.

// freeModelsKey is the aggy config key.
const freeModelsKey = "opencode-free-models"

// freeModelsNoteName is the notes-table row saying the note was shown.
const freeModelsNoteName = "opencode_free_models_note"

// freeModelsNote is said before the question, until the person first says yes
// to it. It says only what OpenCode's own docs say (opencode.ai/docs/zen, read
// 2026-11-10): models are named opencode/<model-id>, paid ones among them that
// need OpenCode credit; the page names each model's provider and links some of
// their terms; and some free models' data may be retained or used to improve
// or train the model, model by model. OpenCode's general terms of service and
// privacy policy were not read for this note: unverified.
const freeModelsNote = `Before you turn this on: this allows OpenCode's own provider (opencode/*), not
only its free models. OpenCode's paid models on it become selectable too, and
spend your OpenCode credit if you have any. With any of them, your prompts and
the code OpenCode sends with them go to OpenCode's own service, not to a plan or
key of yours, under OpenCode's and each model provider's terms. OpenCode's docs
say some free models' data may be kept or used to improve or train the model;
which, and for how long, differs by model: see https://opencode.ai/docs/zen.
It applies to OpenCode sessions only, never to borrows. A repo whose
allowed_vendors does not list opencode-free still refuses it.
`

// freeModelsQuestion is the y/N for turning the setting on.
const freeModelsQuestion = "Allow OpenCode's own models in OpenCode sessions, free and paid (paid ones spend your OpenCode credit)?"

// freeModelsNoteShown reports whether the person said yes after the note
// before. It opens the index read-only and creates nothing; an index that
// cannot be read counts as no, so the note is shown again, the safe side.
func freeModelsNoteShown(ctx context.Context) bool {
	path, err := store.DefaultPath()
	if err != nil {
		return false
	}
	if _, err := safefs.Stat(path); err != nil {
		return false
	}
	s, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return false
	}
	defer func() { _ = s.Close() }()
	_, ok, err := s.Note(ctx, freeModelsNoteName)
	return err == nil && ok
}

// recordFreeModelsNote records that the person said yes after the note. A
// failure is said, and the note is shown again next time.
func recordFreeModelsNote(ctx context.Context, out io.Writer) {
	s, err := store.OpenDefault(ctx)
	if err == nil {
		err = s.SetNote(ctx, freeModelsNoteName, "shown", time.Now())
		_ = s.Close()
	}
	if err != nil {
		fmt.Fprintf(out, "aggy: that you saw this note could not be saved, so it is shown again next time: %s %s\n",
			termsafe.Line(err.Error()), dlog.Tag(diag.CodeStore))
	}
}

// openCodeFreeRefusal says why a model on OpenCode's own provider is refused:
// the person's setting is off (a repo's own does not count), this repo's
// allowed_vendors does not list opencode-free, or this folder's trust list
// does not have the row ticked (here false; T6977). All three must allow it.
func openCodeFreeRefusal(e config.Effective, model string, here bool, root string) error {
	if !e.OpenCode.FreeModels {
		why := "OpenCode's own models are off; aggy config set " + freeModelsKey + " on allows them"
		if e.Repo.SetsFreeModels() {
			why += " (this repo's [opencode] free_models is ignored: only your own config counts)"
		}
		return fmt.Errorf("model %s is refused: %s", model, why)
	}
	if !slices.Contains(e.OpenCodeProviders(), config.OpenCodeFreeProvider) {
		return fmt.Errorf("model %s is refused: OpenCode's own models are on in your config, but this repo's allowed_vendors does not list %s",
			model, config.OpenCodeFreeRepoName)
	}
	return fmt.Errorf("model %s is refused: OpenCode's own models are on in your config, but %s is not ticked for this folder in aggy's trust list; aggy trust %s lists it",
		model, freeModelsRow, root)
}

// freeModelsRow names the trust list's row in the lines below.
const freeModelsRow = `"OpenCode free models"`

// openCodeFreeProviders are the providers a session in this folder may use:
// the repo's (e.OpenCodeProviders), without OpenCode's own when this folder's
// trust list does not have it ticked (here).
func openCodeFreeProviders(e config.Effective, here bool) []string {
	providers := e.OpenCodeProviders()
	if !here {
		providers = slices.DeleteFunc(providers, func(p string) bool { return p == config.OpenCodeFreeProvider })
	}
	return providers
}

// openCodeFreeWarns are the launch's lines about free_models: a repo's own
// setting, which is ignored, and the person's, when this repo refuses it or
// this folder's trust list has it unticked (here false).
func openCodeFreeWarns(e config.Effective, here bool, root string) []string {
	var out []string
	if e.Repo.SetsFreeModels() {
		out = append(out, "this repo's .aggy/config.toml sets [opencode] free_models, which aggy ignores: only your own config turns OpenCode's free models on (aggy config set "+freeModelsKey+" on)")
	}
	switch {
	case !e.OpenCode.FreeModels:
	case !slices.Contains(e.OpenCodeProviders(), config.OpenCodeFreeProvider):
		out = append(out, "OpenCode's free models are on in your config, but this repo's allowed_vendors does not list "+config.OpenCodeFreeRepoName+", so they are off here")
	case !here:
		out = append(out, "OpenCode's free models are on in your config, but "+freeModelsRow+" is not ticked for this folder in aggy's trust list, so they are off here; aggy trust "+root+" lists it")
	}
	return out
}
