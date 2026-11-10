package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
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

// freeModelsNote is said the first time the person turns the setting on at a
// terminal. It says only what OpenCode's own docs say (opencode.ai/docs/zen,
// read 2026-11-10): models are named opencode/<model-id>, and some free
// models' data may be retained or used to improve or train the model, model
// by model. OpenCode's general terms of service and privacy policy were not
// read for this note: unverified.
const freeModelsNote = `Before you turn this on: with OpenCode's free models, your prompts and the code
OpenCode sends with them go to OpenCode's own service, not to a plan or key of
yours, under OpenCode's terms. OpenCode's docs say some free models' data may be
kept or used to improve or train the model; which, and for how long, differs by
model: see https://opencode.ai/docs/zen. It applies to OpenCode sessions only,
never to borrows. A repo whose allowed_vendors does not list opencode-free still
refuses it.
`

// showFreeModelsNote prints the note unless it was shown before, and records
// that it was. A store that cannot be opened or written shows the note again
// next time, which is the safe side.
func showFreeModelsNote(ctx context.Context, out io.Writer) {
	s, err := store.OpenDefault(ctx)
	if err == nil {
		defer func() { _ = s.Close() }()
		if _, ok, err := s.Note(ctx, freeModelsNoteName); err == nil && ok {
			return
		}
	}
	fmt.Fprint(out, freeModelsNote)
	if err == nil {
		err = s.SetNote(ctx, freeModelsNoteName, "shown", time.Now())
	}
	if err != nil {
		fmt.Fprintf(out, "aggy: that this note was shown could not be saved, so it is shown again next time: %s %s\n",
			termsafe.Line(err.Error()), dlog.Tag(diag.CodeStore))
	}
}

// openCodeFreeWarns are the launch's lines about free_models: a repo's own
// setting, which is ignored, and the person's, when this repo refuses it.
func openCodeFreeWarns(e config.Effective, providers []string) []string {
	var out []string
	if e.Repo.SetsFreeModels() {
		out = append(out, "this repo's .aggy/config.toml sets [opencode] free_models, which aggy ignores: only your own config turns OpenCode's free models on (aggy config set "+freeModelsKey+" on)")
	}
	if e.OpenCode.FreeModels && !slices.Contains(providers, config.OpenCodeFreeProvider) {
		out = append(out, "OpenCode's free models are on in your config, but this repo's allowed_vendors does not list "+config.OpenCodeFreeRepoName+", so they are off here")
	}
	return out
}
